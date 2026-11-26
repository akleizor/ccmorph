package router

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"slices"
)

// The shadow picker is a Thompson-sampling bandit. It runs beside Choose and
// acts on nothing: the ledger records its pick next to the real one, so a week
// of traffic shows whether it would pass more tasks for less.
//
// Each (model, kind, tier) has a Beta belief that the model passes such a task.
// The pick samples one success chance p per usable option and takes the lowest
// expected cost of a passed task: (money + minutes × TimeValue) / p.

// TimeValue is US dollars per minute a caller waits: the one price that turns
// time into money. A default the user approved (2026-11-26); router.json's
// time_value replaces it.
const TimeValue = 0.05

// Evidence weights. An accepted result (merged, tests passed) is what the
// route was for; the decider only read the answer, so its verdict counts half.
// A hard failure of the model's own (timeout, error, empty) is a failed
// route; a quota or auth refusal is the account's and says nothing of the model.
const (
	acceptedWeight = 1.0
	deciderWeight  = 0.5
)

// tokensPerCall prices a paid call when nothing better is known: list prices
// are per 1M tokens, and a delegated task reads and writes on this order.
const tokensPerCall = 50_000

// shadowRand is one decision's generator: a *rand.Rand is not safe to share
// between two concurrent Decides.
func shadowRand() *rand.Rand { return rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())) }

type arm struct{ model, kind, tier string }

type belief struct{ pass, fail float64 }

// banditStats is the evidence the ledger holds: a belief per arm and the
// minutes its settled routes took, per arm and per kind and tier.
type banditStats struct {
	beliefs map[arm]belief
	took    map[arm]durations
	task    map[[2]string]durations // every model's routes of that kind and tier
	all     durations
}

type durations struct{ sum, n float64 }

func (d *durations) add(m float64) { d.sum += m; d.n++ }

// minutes is the expected minutes of one route on arm a. A model timed on a
// few routes is pulled toward the mean of every model on that kind and tier
// (one prior route at that mean), so a handful of short runs cannot make it
// look fast. 1 when nothing at all is timed.
func (s banditStats) minutes(a arm) float64 {
	base := 1.0
	if t := s.task[[2]string{a.kind, a.tier}]; t.n > 0 {
		base = t.sum / t.n
	} else if s.all.n > 0 {
		base = s.all.sum / s.all.n
	}
	d := s.took[a]
	return (d.sum + base) / (d.n + 1)
}

// banditRow is the part of a ledger row the bandit reads. Decoding only
// these fields skips each row's brief, the bulk of the ledger.
type banditRow struct {
	Model    string   `json:"model"`
	Kind     string   `json:"kind"`
	Tier     string   `json:"tier"`
	TS       *float64 `json:"ts"`
	Finished float64  `json:"finished"`
	Outcome  string   `json:"outcome"`
	Failure  string   `json:"failure"`
	Verdict  string   `json:"verdict"`
	Accepted *bool    `json:"accepted"`
	Quality  *bool    `json:"quality"`
}

// readBanditRows reads the ledger at path as bandit rows. A field of an
// unexpected type is left at its zero value; a line that is not JSON is skipped.
func readBanditRows(path string) []banditRow {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var rows []banditRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var r banditRow
		var typeErr *json.UnmarshalTypeError
		if err := json.Unmarshal(sc.Bytes(), &r); err == nil || errors.As(err, &typeErr) {
			rows = append(rows, r)
		}
	}
	return rows
}

func loadBanditStats(rows []banditRow) banditStats {
	s := banditStats{beliefs: map[arm]belief{}, took: map[arm]durations{}, task: map[[2]string]durations{}}
	for _, r := range rows {
		if r.Model == "" || r.Kind == "" || r.Tier == "" || r.notTheModels() {
			continue // a refusal or a cancel: neither its time nor its result is the model's
		}
		a, k := arm{r.Model, r.Kind, r.Tier}, [2]string{r.Kind, r.Tier}
		if r.TS != nil && r.Finished > *r.TS {
			m := (r.Finished - *r.TS) / 60
			d, t := s.took[a], s.task[k]
			d.add(m)
			t.add(m)
			s.took[a], s.task[k] = d, t
			s.all.add(m)
		}
		pass, weight, ok := r.evidence()
		if !ok {
			continue
		}
		b := s.beliefs[a]
		if pass {
			b.pass += weight
		} else {
			b.fail += weight
		}
		s.beliefs[a] = b
	}
	return s
}

// notTheModels is a route the account refused or the caller cancelled: it
// says nothing about the model, whatever else the row holds.
func (r banditRow) notTheModels() bool {
	return accountRefusal(r.Outcome) || accountRefusal(r.Failure) || r.Outcome == "aborted" || r.Failure == "aborted"
}

// evidence reads one settled row: whether it passed, and how much that counts.
// Acceptance overrides the decider; a format problem without acceptance, a
// refusal and an unsettled row count nothing.
func (r banditRow) evidence() (pass bool, weight float64, ok bool) {
	switch {
	case r.notTheModels():
		return false, 0, false
	case r.Accepted != nil:
		return *r.Accepted, acceptedWeight, true
	case r.Outcome == "timeout" || r.Outcome == "empty" || r.Outcome == "error":
		return false, acceptedWeight, true
	case r.Verdict == "pass":
		return true, deciderWeight, true
	case r.Verdict == "wrong":
		return false, deciderWeight, true
	case r.Verdict == "format":
		return false, 0, false
	case r.Quality != nil: // a row settled before verdicts
		return *r.Quality, deciderWeight, true
	}
	return false, 0, false
}

// prior is the belief before any evidence: the user's own judgments only. An
// explicit label in router.json at or above the task's tier leans to pass,
// below it leans to fail; a family the user ranks first or second for the kind
// leans to pass. A model nobody judged starts at Beta(1, 1): unknown.
func prior(c Config, o Option, t Task) belief {
	b := belief{1, 1}
	if l, ok := c.Labels[o.Model]; ok && Efforts[l.Tier] != "" {
		if slices.Index(Tiers, l.Tier) >= slices.Index(Tiers, t.Tier) {
			b.pass++
		} else {
			b.fail++
		}
	}
	if order := familyOrder(c.Kinds, t.Kind, t.Lang); slices.Contains(order[:min(2, len(order))], o.Family) {
		b.pass++
	}
	return b
}

// Shadow is the bandit's pick and why: the sampled pass chance and the
// expected cost of a passed task, in dollars.
type Shadow struct {
	Profile string  `json:"profile"`
	Model   string  `json:"model"`
	P       float64 `json:"p"`
	Cost    float64 `json:"cost"`
}

// callMoney is what one call costs in dollars: nothing on a free gateway or a
// subscription's perishable quota, list price on a paid API.
func callMoney(o Option) float64 {
	if o.Class == "free" || o.Class == "subscription" || math.IsInf(o.Price, 1) {
		return 0
	}
	return o.Price * tokensPerCall / 1e6
}

// shadowPick samples each arm's belief once, so a model two profiles serve
// draws one p, and returns the cheapest expected passed task; nil when there
// is no option or no finite cost.
func shadowPick(c Config, opts []Option, t Task, s banditStats, rng *rand.Rand) *Shadow {
	timeValue := TimeValue
	if c.TimeValue > 0 {
		timeValue = c.TimeValue
	}
	sampled := map[string]float64{}
	var best *Shadow
	for _, o := range opts {
		a := arm{o.Model, t.Kind, t.Tier}
		p, ok := sampled[o.Model]
		if !ok {
			b, e := prior(c, o, t), s.beliefs[a]
			p = betaSample(rng, b.pass+e.pass, b.fail+e.fail)
			sampled[o.Model] = p
		}
		cost := (callMoney(o) + s.minutes(a)*timeValue) / max(p, 1e-6)
		if math.IsInf(cost, 0) || math.IsNaN(cost) {
			continue // a cost JSON cannot hold would cost the route its reservation
		}
		if best == nil || cost < best.Cost {
			best = &Shadow{Profile: o.Profile, Model: o.Model, P: p, Cost: cost}
		}
	}
	return best
}

// betaSample draws from Beta(a, b) as X/(X+Y) of two Gamma draws.
func betaSample(rng *rand.Rand, a, b float64) float64 {
	x, y := gammaSample(rng, a), gammaSample(rng, b)
	return x / (x + y)
}

// gammaSample draws from Gamma(k, 1) by Marsaglia and Tsang; k < 1 is boosted.
func gammaSample(rng *rand.Rand, k float64) float64 {
	if k < 1 {
		return gammaSample(rng, k+1) * math.Pow(rng.Float64(), 1/k)
	}
	d := k - 1.0/3
	c := 1 / math.Sqrt(9*d)
	for {
		x := rng.NormFloat64()
		v := 1 + c*x
		if v <= 0 {
			continue
		}
		v = v * v * v
		u := rng.Float64()
		if math.Log(u) < 0.5*x*x+d-d*v+d*math.Log(v) {
			return d * v
		}
	}
}

// shadowSummary is report's line on the shadow picker: how often it agreed
// with the real pick. "" when no route carries a shadow pick.
func shadowSummary(rows []Row) string {
	n, same := 0, 0
	for _, r := range rows {
		sh, ok := r["shadow"].(map[string]any)
		if !ok {
			continue
		}
		n++
		if sh["profile"] == r["profile"] && sh["model"] == r["model"] {
			same++
		}
	}
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("shadow picker: %d routes, agreed with the real pick on %d (%.0f%%)", n, same, 100*float64(same)/float64(n))
}
