package router

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/quota"
	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
	"github.com/akleizor/ccmorph/util/tier"
)

// QuotaInput is `ccmorph quota`'s stdin for these profiles, built by the cli,
// which owns how each profile's sign-in reaches the quota reader.
var QuotaInput func(names []string) string

const quotaTTL = 300

// subscriptionModels: what a Claude subscription serves through Claude Code's aliases.
var subscriptionModels = []string{"opus", "sonnet", "haiku"}

type served struct {
	name, class string
	models      []string
}

// profileNames is every valid, non-symlinked conf in the profile root,
// the one scan util.ValidConfs holds for both its readers (#848).
func profileNames() []string {
	var names []string
	for _, conf := range util.ValidConfs(paths.ProfileRoot()) {
		names = append(names, strings.TrimSuffix(filepath.Base(conf), ".conf"))
	}
	return names
}

// readServed is one profile's class and served IDs, from ccmorph's own files (no network).
func readServed(name string) (served, bool) {
	path, err := ctxwindow.ProfilePath(name)
	if err != nil {
		return served{}, false
	}
	_, f, err := ctxwindow.ReadConf(path)
	if err != nil {
		return served{}, false
	}
	s := served{name: name, class: "api"}
	typ := f["type"]
	switch {
	case typ == "sub" || typ == "token":
		s.class, s.models = "subscription", subscriptionModels
		return s, true
	case typ == "provider" && (f["provider"] == "codex" || f["provider"] == "antigravity"):
		s.class = "subscription"
	case f["monthly_budget_usd"] != "":
		s.class = "budget"
	}
	set := map[string]bool{}
	for m := range ctxwindow.DiscoveredValues(f) {
		set[m] = true
	}
	for _, k := range tier.TierWords {
		set[f["env."+tier.TierEnvKey(k)]] = true
	}
	set[f["model"]] = true
	for m := range set {
		m = tier.TrimWindowMarker(m)
		if m != "" && !ctxwindow.IsExcluded(f, m) && !slices.Contains(s.models, m) {
			s.models = append(s.models, m)
		}
	}
	sort.Strings(s.models)
	if len(s.models) == 0 && f["provider"] == "codex" {
		s.models = []string{"luna", "terra", "sol"}
	}
	return s, true
}

type snapshot struct {
	ReadAt  float64       `json:"read_at"`
	Entries []quota.Entry `json:"entries"`
}

// readQuota is every profile's windows and spend as `ccmorph quota --json` reads
// them, kept for quotaTTL seconds. cachedOnly never starts a live sweep: it
// takes any stored reading, however old, so a hook never waits on a usage
// endpoint, and a fresh state dir answers with the missing-reading note.
func readQuota(names []string, refresh, cachedOnly bool) map[string]quota.Entry {
	path := filepath.Join(routerDir(), "quota.json")
	var snap snapshot
	have := readJSON(path, &snap)
	if cachedOnly && !have {
		byName := make(map[string]quota.Entry, len(names))
		for _, n := range names {
			byName[n] = quota.Entry{Profile: n, Error: "not read yet: no stored quota reading"}
		}
		return byName
	}
	if !cachedOnly && (refresh || !have || nowSec()-snap.ReadAt >= quotaTTL) && QuotaInput != nil {
		var out bytes.Buffer
		quota.Main([]string{"--json"}, strings.NewReader(QuotaInput(names)), &out, io.Discard)
		var got struct{ Profiles []quota.Entry }
		if json.Unmarshal(out.Bytes(), &got) == nil {
			snap = snapshot{ReadAt: nowSec(), Entries: got.Profiles}
			_ = writeJSON(path, snap)
		}
	}
	byName := map[string]quota.Entry{}
	for _, e := range snap.Entries {
		byName[e.Profile] = e
	}
	return byName
}

var windowLength = map[string]float64{"5h": 5 * 3600, "7d": 7 * 86400}

// windows turns a quota entry into router windows. A window past its reset
// starts again empty; a budget is one window over the calendar month.
func windows(e quota.Entry, now float64) []Window {
	var out []Window
	for _, w := range e.Windows {
		length := windowLength[w.ID[:min(2, len(w.ID))]]
		// A window with no share is not a reading to pace on: a budget month
		// whose cost is unknown carries none (#795).
		if length == 0 || w.ResetsAt == nil || w.Used == nil {
			continue
		}
		x := Window{Name: w.ID, Used: math.Max(0, math.Min(1, *w.Used)), ResetsAt: float64(*w.ResetsAt), Length: length, Models: w.Models}
		// Claude's per-model weekly windows ("7d_opus") pace only that alias.
		if _, model, ok := strings.Cut(w.ID, "_"); ok && len(x.Models) == 0 {
			x.Models = []string{model}
		}
		if x.ResetsAt <= now {
			x.ResetsAt += (math.Floor((now-x.ResetsAt)/length) + 1) * length
			x.Used = 0
		}
		out = append(out, x)
	}
	// A budget is the month's priced spend over the budget. When no turn this
	// month carried a price there is no share to show, so the bar carries no
	// number: the profile answers like one with no reading, quota-unknown,
	// never a 0% picture of a month with unknown spend (#301, #776).
	if s := e.Spend; s != nil && s.BudgetUSD != nil && *s.BudgetUSD > 0 && !s.AllUnpriced() {
		t := time.Unix(int64(now), 0)
		start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
		end := start.AddDate(0, 1, 0)
		out = append(out, Window{Name: "month", Used: math.Min(1, s.MonthUSD/(*s.BudgetUSD)),
			ResetsAt: float64(end.Unix()), Length: end.Sub(start).Seconds()})
	}
	return out
}

// Profiles is every ccmorph profile as the router sees it, with the notes a
// missing reading left.
func Profiles(c Config, refresh, cachedOnly bool, only []string) ([]Profile, []string) {
	names := profileNames()
	readings := readQuota(names, refresh, cachedOnly) // the stored snapshot always covers every profile
	if only != nil {
		names = slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(only, n) })
	}
	now := nowSec()
	var out []Profile
	var notes []string
	for _, n := range names {
		s, ok := readServed(n)
		if !ok {
			continue
		}
		pc := c.Profiles[n]
		p := Profile{Name: n, Class: s.class, Reserved: pc.Reserved, Enabled: !pc.Disabled,
			UnavailableUntil: unavailableUntil(n)}
		if pc.Class != "" {
			p.Class = pc.Class
		}
		for _, m := range s.models {
			if !slices.Contains(c.DisabledModels, m) && !slices.Contains(pc.DisabledModels, m) &&
				!advisor(m, c.AdvisorOnly) && unavailableUntil(n+"/"+m) <= now {
				p.Models = append(p.Models, m)
			}
		}
		if e, ok := readings[n]; ok && (p.Class == "subscription" || p.Class == "budget") {
			p.Windows = windows(e, now)
			if e.Error != "" {
				notes = append(notes, n+": "+e.Error)
			}
		}
		out = append(out, p)
	}
	return out, notes
}

func advisor(model string, fragments []string) bool {
	for _, a := range fragments {
		if a != "" && strings.Contains(strings.ToLower(model), strings.ToLower(a)) {
			return true
		}
	}
	return false
}

// blendedPrices is list price in $ per 1M tokens at an agent's usual 3 input : 1 output.
func blendedPrices(models []string) map[string]float64 {
	book := shim.CachedOwnedPriceBook("")
	out := map[string]float64{}
	for _, m := range models {
		if in, outp, ok := book.Lookup(m); ok {
			out[m] = (3*in + outp) / 4
		}
	}
	return out
}

// labeler is every routable model's label: aliases by their own words, other
// IDs by a cached decider label, the user's labels over both, and prices from
// ccmorph's price book. A deadline stops new decider calls (the hook's budget).
func labeler(c Config, profiles []Profile, d *Decider, deadline time.Time) func(string, string) (Label, bool) {
	labels := map[string]Label{}
	var models []string
	for _, p := range profiles {
		if p.Reserved || !p.Enabled {
			continue
		}
		for _, m := range p.Models {
			if _, done := labels[m]; done {
				continue
			}
			models = append(models, m)
			if p.Class == "subscription" && namedTier(m) {
				labels[m] = HeuristicLabel(m)
				continue
			}
			left := time.Until(deadline)
			if deadline.IsZero() {
				left = 3 * time.Second
			}
			labels[m] = labelModel(m, d, min(left, 3*time.Second))
		}
	}
	prices := blendedPrices(models)
	for m, l := range labels {
		if fixed, ok := c.Labels[m]; ok {
			if fixed.Family != "" {
				l.Family = fixed.Family
			}
			if fixed.Tier != "" {
				l.Tier = fixed.Tier
			}
			if fixed.Vision != nil {
				l.Vision = fixed.Vision
			}
			l.ImageGen = l.ImageGen || fixed.ImageGen
		}
		if p, ok := prices[m]; ok {
			l.Price = p
		}
		labels[m] = l
	}
	return func(_, m string) (Label, bool) { l, ok := labels[m]; return l, ok }
}

// SessionProfile is this Claude Code session's profile: the name of its config dir.
func SessionProfile() string {
	n := filepath.Base(os.Getenv("CLAUDE_CONFIG_DIR"))
	if profile.ValidName(n) {
		return n
	}
	return ""
}
