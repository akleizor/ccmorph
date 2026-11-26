package router

import (
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Acceptance outranks the decider, which counts half; an account's refusal and
// a format problem say nothing about whether the model can do the work.
func TestEvidenceWeighsAcceptanceAboveTheDeciderAndSkipsWhatIsNotTheModels(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    Row
		pass   bool
		weight float64
		counts bool
	}{
		{"accepted beats a wrong verdict", Row{"verdict": "wrong", "accepted": true}, true, 1, true},
		{"decider pass", Row{"verdict": "pass"}, true, 0.5, true},
		{"decider wrong", Row{"verdict": "wrong"}, false, 0.5, true},
		{"format", Row{"verdict": "format"}, false, 0, false},
		{"format, then accepted", Row{"verdict": "format", "accepted": true}, true, 1, true},
		{"a row from before verdicts", Row{"quality": false}, false, 0.5, true},
		{"timeout", Row{"outcome": "error", "failure": "timeout"}, false, 1, true},
		{"quota", Row{"outcome": "error", "failure": "quota"}, false, 0, false},
		{"accepted, but the account refused", Row{"outcome": "error", "failure": "quota", "accepted": false}, false, 0, false},
		{"cancelled", Row{"outcome": "error", "failure": "aborted"}, false, 0, false},
		{"auth", Row{"outcome": "auth"}, false, 0, false},
		{"unsettled", Row{"outcome": nil}, false, 0, false},
	} {
		pass, weight, counts := evidence(tc.row)
		if pass != tc.pass || weight != tc.weight || counts != tc.counts {
			t.Errorf("%s: evidence = %v %v %v, want %v %v %v", tc.name, pass, weight, counts, tc.pass, tc.weight, tc.counts)
		}
	}
}

// A cancelled or refused route is neither a result nor a time: a burst of
// instant cancels must not make a model look fast.
func TestARefusedOrCancelledRouteIsNotTimed(t *testing.T) {
	rows := []Row{{"model": "m", "kind": "code", "tier": "mid", "ts": 0.0, "finished": 600.0}}
	for range 30 {
		rows = append(rows, Row{"model": "m", "kind": "code", "tier": "mid", "ts": 0.0, "finished": 1.0, "outcome": "error", "failure": "aborted"})
	}
	if got := loadBanditStats(rows).minutes(arm{"m", "code", "mid"}); got < 9.9 {
		t.Fatalf("cancels timed the model at %.2f min", got)
	}
}

// The sampler draws from the Beta it is given: mean a/(a+b), variance
// ab/((a+b)²(a+b+1)), within sampling error at a fixed seed.
func TestBetaSampleMatchesItsDistribution(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	for _, ab := range [][2]float64{{2, 5}, {0.5, 0.5}, {30, 10}} {
		a, b := ab[0], ab[1]
		const n = 40000
		var sum, sq float64
		for range n {
			x := betaSample(rng, a, b)
			sum += x
			sq += x * x
		}
		mean, v := sum/n, sq/n-(sum/n)*(sum/n)
		wantMean, wantVar := a/(a+b), a*b/((a+b)*(a+b)*(a+b+1))
		if math.Abs(mean-wantMean) > 0.01 || math.Abs(v-wantVar) > 0.1*wantVar {
			t.Errorf("Beta(%g,%g): mean %.4f var %.5f, want %.4f %.5f", a, b, mean, v, wantMean, wantVar)
		}
	}
}

// A model two profiles serve draws one p: at the same price it does not get
// two chances against a model one profile serves, and a dearer profile never
// beats the cheaper one serving the same model.
func TestAModelServedTwiceDrawsOnce(t *testing.T) {
	task := Task{Kind: "code", Tier: "mid"}
	opts := []Option{
		{Profile: "a", Model: "twice", Class: "free", Family: "x"},
		{Profile: "b", Model: "twice", Class: "free", Family: "x"},
		{Profile: "a", Model: "once", Class: "free", Family: "y"},
	}
	rng := rand.New(rand.NewPCG(3, 4))
	wins := 0
	const n = 4000
	for range n {
		if shadowPick(Config{}, opts, task, loadBanditStats(nil), rng).Model == "twice" {
			wins++
		}
	}
	if share := float64(wins) / n; math.Abs(share-0.5) > 0.03 {
		t.Fatalf("the twice-served model won %.2f of equal draws, want about half", share)
	}
	dear := []Option{{Profile: "api", Model: "m", Class: "paid", Price: 20}, {Profile: "gw", Model: "m", Class: "free"}}
	for range 200 {
		if got := shadowPick(Config{}, dear, task, loadBanditStats(nil), rng); got.Profile != "gw" {
			t.Fatalf("a dearer profile beat the free one serving the same model: %+v", got)
		}
	}
}

// A time value so large the cost overflows leaves no shadow pick, and the
// route's reservation is written whole.
func TestAnInfiniteShadowCostNeverCostsTheReservation(t *testing.T) {
	hookEnv(t, true)
	s := loadBanditStats([]Row{{"model": "m", "kind": "code", "tier": "mid", "ts": 0.0, "finished": 120.0}})
	opts := []Option{{Profile: "gw", Model: "m", Class: "free"}}
	sh := shadowPick(Config{TimeValue: 1e308}, opts, Task{Kind: "code", Tier: "mid"}, s, rand.New(rand.NewPCG(1, 1)))
	if sh != nil {
		t.Fatalf("an infinite cost became a pick: %+v", sh)
	}
	reserve("route-1", Decision{Task: Task{Kind: "code", Tier: "mid"}, Pick: &opts[0], Shadow: sh}, "brief", nil)
	if rows := readRows(ledgerPath()); len(rows) != 1 || rows[0]["model"] != "m" {
		t.Fatalf("the reservation was lost: %v", rows)
	}
	if _, err := json.Marshal(Shadow{Cost: math.Inf(1)}); err == nil {
		t.Fatal("json now encodes +Inf; the guard above is no longer needed")
	}
}

// A hook's route never waits past half its remaining budget for the shadow.
func TestAHookNeverWaitsOnTheShadow(t *testing.T) {
	start := time.Now()
	got := shadowWithin(time.Now().Add(200*time.Millisecond), func() *Shadow {
		time.Sleep(2 * time.Second)
		return &Shadow{}
	})
	if got != nil || time.Since(start) > time.Second {
		t.Fatalf("waited %v for the shadow (%+v)", time.Since(start), got)
	}
}

// With the same cost, the model that keeps passing wins; with the same record,
// the faster free model wins over a paid one.
func TestShadowPickTakesTheCheapestExpectedPass(t *testing.T) {
	task := Task{Kind: "code", Tier: "mid"}
	free := func(m string) Option { return Option{Profile: "gw", Model: m, Class: "free", Family: m} }
	var rows []Row
	for range 40 {
		rows = append(rows, Row{"model": "good", "kind": "code", "tier": "mid", "accepted": true, "ts": 0.0, "finished": 60.0})
		rows = append(rows, Row{"model": "bad", "kind": "code", "tier": "mid", "accepted": false, "ts": 0.0, "finished": 60.0})
	}
	s := loadBanditStats(rows)
	rng := rand.New(rand.NewPCG(1, 2))
	for range 50 {
		if got := shadowPick(Config{}, []Option{free("bad"), free("good")}, task, s, rng); got.Model != "good" {
			t.Fatalf("picked %+v over a model that passes", got)
		}
	}
	paid := Option{Profile: "api", Model: "good", Class: "paid", Family: "good", Price: 20}
	for range 50 {
		if got := shadowPick(Config{}, []Option{paid, free("good")}, task, s, rng); got.Profile != "gw" {
			t.Fatalf("paid for the same model a free gateway serves: %+v", got)
		}
	}
}

// A model timed on a few short routes is pulled toward the kind and tier's
// mean: two one-minute runs beside forty ten-minute runs do not make it fast.
func TestAFewShortRunsDoNotMakeAModelLookFast(t *testing.T) {
	var rows []Row
	for range 40 {
		rows = append(rows, Row{"model": "slow", "kind": "code", "tier": "mid", "ts": 0.0, "finished": 600.0})
	}
	for range 2 {
		rows = append(rows, Row{"model": "lucky", "kind": "code", "tier": "mid", "ts": 0.0, "finished": 60.0})
	}
	s := loadBanditStats(rows)
	lucky, slow := s.minutes(arm{"lucky", "code", "mid"}), s.minutes(arm{"slow", "code", "mid"})
	if lucky < 3.5 || slow < 9.5 {
		t.Fatalf("lucky %.2f min, slow %.2f min: two short runs outweighed the kind's record", lucky, slow)
	}
	if unseen := s.minutes(arm{"new", "code", "mid"}); unseen < 9 {
		t.Fatalf("an untimed model reads %.2f min, not the kind's mean", unseen)
	}
}

// Decide records the shadow pick from every usable model, any tier, and leaves
// the real pick to the current rules.
func TestDecideRecordsAShadowPickAcrossTiersAndActsOnTheRealOne(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	conf := "type=apikey\nbase_url=http://127.0.0.1:9\nmodel=claude-sonnet-5-5\nenv.ANTHROPIC_DEFAULT_HAIKU_MODEL=claude-haiku-5-5\n"
	if err := os.WriteFile(filepath.Join(root, "gw.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 40 { // the small model keeps passing mid code tasks
		ledgerAppend(Row{"model": "claude-haiku-5-5", "kind": "code", "tier": "mid", "accepted": true})
		ledgerAppend(Row{"model": "claude-sonnet-5-5", "kind": "code", "tier": "mid", "accepted": false})
	}
	c, _ := LoadConfig()
	c.Profiles = map[string]ProfileConfig{"gw": {Class: "free"}}
	d, err := Decide(c, Request{Prompt: "fix the bug", Kind: "code", Tier: "mid", NoDecider: true})
	if err != nil || d.Pick == nil || d.Pick.Model != "claude-sonnet-5-5" {
		t.Fatalf("the real pick moved: %+v %v", d.Pick, err)
	}
	if d.Shadow == nil || d.Shadow.Model != "claude-haiku-5-5" {
		t.Fatalf("shadow %+v, want the small model the evidence favours", d.Shadow)
	}
	reserve("route-x", d, "fix the bug", nil)
	rows := readRows(ledgerPath())
	if sh, _ := rows[len(rows)-1]["shadow"].(map[string]any); sh["model"] != "claude-haiku-5-5" {
		t.Fatalf("the ledger did not record the shadow pick: %v", rows[len(rows)-1])
	}
}
