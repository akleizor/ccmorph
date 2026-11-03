package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hazardWorld writes a profile conf holding the given probed windows and a settings
// file offering the given models, and returns their paths.
func hazardWorld(t *testing.T, windows, settings string) (conf, set string) {
	t.Helper()
	dir := t.TempDir()
	conf, set = filepath.Join(dir, "gw.conf"), filepath.Join(dir, "settings.json")
	body := "type=proxy\n"
	for _, w := range strings.Fields(windows) {
		body += "context_window." + w + "\n"
	}
	if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(set, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	return conf, set
}

// unsafeText is what the launch's unsafe-model warning says for these readings.
func unsafeText(in DiagIn) string {
	var out []string
	for _, r := range LaunchDiagnostics(in) {
		if r.Kind == "unsafe" {
			out = append(out, r.Text)
		}
	}
	return strings.Join(out, "\n")
}

// The refuse-before-compacting warning sizes each offered model's trigger the way
// the engine does: its own output reserve, its own ceiling (a [1m] tag anywhere in
// the id, the 200,000 family default for a claude- id), the session budget and the
// percentage in force.
func TestUnsafeWarningSizesTheTriggerLikeTheEngine(t *testing.T) {
	for _, c := range []struct {
		name, windows, models string
		pct, maxOut, budget   string
		no1m                  bool
		want, notWant         string
	}{
		{"3-opus reserves 4096", "claude-3-opus=150000", `["claude-3-opus"]`, "80", "", "", false, "compaction starts at 156723", ""},
		{"an ordinary id keeps the 20k reserve", "claude-plain=150000", `["claude-plain"]`, "80", "", "", false, "", "claude-plain holds"},
		{"a pin above the model upper limit is capped to it", "claude-3-opus=150000", `["claude-3-opus"]`, "80", "20000", "", false, "compaction starts at 156723", ""},
		{"a pin under the limit is used as given", "claude-3-opus=150000", `["claude-3-opus"]`, "80", "2000", "", false, "compaction starts at 158400", ""},
		{"3-5-haiku reserves 8192", "claude-3-5-haiku=150000", `["claude-3-5-haiku"]`, "80", "", "", false, "compaction starts at 153446", ""},
		{"a dated id folds to its base", "claude-3-5-haiku-20241022=150000", `["claude-3-5-haiku-20241022"]`, "80", "", "", false, "compaction starts at 153446", ""},
		{"the budget caps the ceiling", "claude-foo=500000", `["claude-foo[1m]"]`, "80", "", "300000", false, "", "claude-foo holds"},
		{"no budget leaves the family ceiling", "claude-foo=500000", `["claude-foo[1m]"]`, "80", "", "", false, "claude-foo holds 500000", ""},
		{"a [1m] entry finds its bare key", "claude-foo=300000", `["claude-foo[1m]"]`, "", "", "", false, "claude-foo holds 300000", ""},
		{"a profile-scoped ceiling is honoured", "gw:claude-x=160000", `["claude-x"]`, "", "", "", false, "claude-x holds 160000", ""},
		{"above the trigger stays silent", "claude-x=200000", `["claude-x"]`, "", "", "", false, "", "claude-x holds"},
		{"a [1m] tag counts anywhere in the id", "claude-x=300000", `["claude-x[1m]-eu"]`, "", "", "", false, "claude-x holds 300000", ""},
		{"an untagged id keeps the 200k family ceiling", "claude-x=300000 claude-x-eu=300000", `["claude-x-eu"]`, "", "", "", false, "", "holds 300000"},
		{"the kill switch unset sizes [1m] at 1M", "claude-foo=300000", `["claude-foo[1m]"]`, "80", "", "", false, "claude-foo holds 300000", ""},
		{"the kill switch set drops the tag", "claude-foo=300000", `["claude-foo[1m]"]`, "80", "", "", true, "", "claude-foo holds"},
		{"a small output pin raises the trigger", "claude-x=170000", `["claude-x"]`, "", "8000", "", false, "claude-x holds 170000", ""},
		{"the default reserve keeps the same model safe", "claude-x=170000", `["claude-x"]`, "", "", "", false, "", "claude-x holds"},
		{"above the engine trigger (144k), not the stale 153k", "claude-small=150000", `["claude-small"]`, "80", "", "", false, "", "claude-small holds"},
		{"below the engine trigger is named", "claude-small=100000", `["claude-small"]`, "80", "", "", false, "claude-small holds 100000", ""},
	} {
		conf, set := hazardWorld(t, c.windows, `{"availableModels":`+c.models+`}`)
		got := unsafeText(DiagIn{Profile: "gw", ProfileConf: conf, Settings: set, PctInEnv: true, EnvPct: c.pct, MaxOut: c.maxOut, Budget: c.budget, No1m: c.no1m})
		if c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%s: want %q in %q", c.name, c.want, got)
		}
		if c.notWant != "" && strings.Contains(got, c.notWant) {
			t.Errorf("%s: %q must not appear in %q", c.name, c.notWant, got)
		}
	}
}

// The percentage may live only in a settings layer: the launch injects none, and the
// warning must read the layer's value, not the no-percentage trigger (#428).
func TestUnsafeWarningReadsALayerOnlyPercentage(t *testing.T) {
	for windows, want := range map[string]bool{"claude-small=150000": false, "claude-small=100000": true} {
		conf, set := hazardWorld(t, windows, `{"availableModels":["claude-small"]}`)
		layer := filepath.Join(t.TempDir(), "canon.json")
		os.WriteFile(layer, []byte(`{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"}}`), 0o600)
		got := strings.Contains(unsafeText(DiagIn{Profile: "gw", ProfileConf: conf, Settings: set, Paths: []string{layer, set}}), "claude-small holds")
		if got != want {
			t.Errorf("%s: named=%v, want %v", windows, got, want)
		}
	}
}

// hazardNames runs the switch advisory's calculation and returns its rows' text.
func hazardNames(t *testing.T, in HazardIn) string {
	t.Helper()
	rows, err := HazardSet(in)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(rows, "\n")
}

func TestHazardSetNamesEverySiblingTheTriggerWouldOverrun(t *testing.T) {
	for _, c := range []struct {
		name, windows, models          string
		budget, ceiling, pct, maxOut   string
		pctInEnv                       bool
		wantRows, notWant, mustContain string
	}{
		{"the budget caps the sibling ceiling", "claude-foo=500000", `["claude-foo[1m]"]`, "300000", "922000", "80", "", true, "", "claude-foo", ""},
		{"no budget leaves the 1M ceiling fatal", "claude-foo=500000", `["claude-foo[1m]"]`, "", "1000000", "80", "", true, "", "", "claude-foo"},
		{"a claude- id is sized by its own 200k family", "roomy=1000000 claude-mini=131072", `["roomy","claude-mini"]`, "1000000", "1000000", "80", "", true, "", "", "claude-mini"},
		{"a claude- id above its own trigger is safe", "roomy=1000000 claude-ample=200000", `["roomy","claude-ample"]`, "1000000", "1000000", "80", "", true, "", "claude-ample", ""},
		{"bare listed before [1m] still names the [1m] fatality", "roomy=1000000 claude-foo=300000", `["roomy","claude-foo","claude-foo[1m]"]`, "1000000", "1000000", "80", "", true, "1", "", "claude-foo"},
		{"[1m] listed before bare is named once", "roomy=1000000 claude-foo=300000", `["roomy","claude-foo[1m]","claude-foo"]`, "1000000", "1000000", "80", "", true, "1", "", "claude-foo"},
		{"fatal at both ceilings is still one row", "roomy=400000 plain=250000", `["roomy","plain","plain[1m]"]`, "", "400000", "80", "", true, "1", "", "plain"},
		{"a layer percentage sizes the set", "roomy=1000000 big-model=800000 small-model=500000", `["roomy","big-model","small-model"]`, "", "1000000", "", "", false, "", "big-model", "686000"},
	} {
		conf, set := hazardWorld(t, c.windows, `{"availableModels":`+c.models+`}`)
		sources := []string{set}
		if !c.pctInEnv {
			layer := filepath.Join(t.TempDir(), "canon.json")
			os.WriteFile(layer, []byte(`{"env":{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":"70"}}`), 0o600)
			sources = []string{layer, set}
		}
		out := hazardNames(t, HazardIn{Profile: "gw", Budget: c.budget, Ceiling: c.ceiling, Sources: sources, PctRaw: c.pct, RawOut: c.maxOut, Conf: conf, PctInEnv: c.pctInEnv})
		if c.mustContain != "" && !strings.Contains(out, c.mustContain) {
			t.Errorf("%s: want %q in %q", c.name, c.mustContain, out)
		}
		if c.notWant != "" && strings.Contains(out, c.notWant) {
			t.Errorf("%s: %q must not appear in %q", c.name, c.notWant, out)
		}
		if c.wantRows != "" && strings.Count(out, "\n") != 1 {
			t.Errorf("%s: want one model row, got %q", c.name, out)
		}
	}
}

// A profile that deletes availableModels offers no models, so none is a hazard.
func TestHazardSetHonoursAProfileTombstoneOfAvailableModels(t *testing.T) {
	conf, canon := hazardWorld(t, "m-small=150000", `{"availableModels":["m-small","m-big"]}`)
	tomb := filepath.Join(t.TempDir(), "settings.profile.json")
	os.WriteFile(tomb, []byte(`{"$ccmorphUnset":[["availableModels"]]}`), 0o600)
	in := HazardIn{Profile: "gw", Budget: "700000", Ceiling: "900000", Conf: conf}
	if out := hazardNames(t, HazardIn{Profile: in.Profile, Budget: in.Budget, Ceiling: in.Ceiling, Conf: conf, Sources: []string{canon}}); !strings.Contains(out, "m-small") {
		t.Fatalf("control: without the tombstone m-small is fatal, got %q", out)
	}
	if out := hazardNames(t, HazardIn{Profile: in.Profile, Budget: in.Budget, Ceiling: in.Ceiling, Conf: conf, Sources: []string{canon, tomb}}); strings.Contains(out, "m-small") {
		t.Fatalf("a tombstoned availableModels must offer nothing, got %q", out)
	}
}

// The percentage and budget parsers read what the engine reads: ECMAScript
// whitespace is trimmed (FEFF and NBSP yes, NEL no), non-ASCII digits are refused,
// a float percentage divides before it multiplies, and an exponent budget is valid.
func TestHazardSetParsesThePercentageAndBudgetLikeTheEngine(t *testing.T) {
	two := func(models string) (string, string) {
		return hazardWorld(t, "small=200000 onefifty=150000", `{"availableModels":`+models+`}`)
	}
	named := func(pct string) bool {
		conf, set := two(`["small","onefifty"]`)
		return strings.Contains(hazardNames(t, HazardIn{Profile: "gw", Budget: "200000", Sources: []string{set}, PctRaw: pct, Conf: conf, PctInEnv: true}), "onefifty")
	}
	for _, c := range []struct {
		name, pct string
		named     bool
	}{
		{"no percentage", "", true},
		{"plain 50 governs", "50", false},
		{"FEFF is engine whitespace", string(rune(0xFEFF)) + "50", false},
		{"NBSP is engine whitespace", " 50", false},
		{"NEL is not", "\u008550", true},
		{"Arabic-Indic digits are refused", "٥٠", true},
		{"a numeric prefix counts", "50junk", false},
	} {
		if got := named(c.pct); got != c.named {
			t.Errorf("%s: named=%v, want %v", c.name, got, c.named)
		}
	}

	// floor(100000*(29/100)) is 28999, not 29000: a wall exactly at the engine's
	// trigger is safe. Both round to 29k, so only the set can tell them apart.
	conf, set := hazardWorld(t, "roomy=120000 exactly=28999", `{"availableModels":["roomy","exactly"]}`)
	if out := hazardNames(t, HazardIn{Profile: "gw", Budget: "120000", Sources: []string{set}, PctRaw: "29", Conf: conf, PctInEnv: true}); strings.Contains(out, "exactly") {
		t.Errorf("a model walled exactly at the engine trigger is safe, got %q", out)
	}

	// The auto-compact window pin: non-ASCII digits are refused, so the derived
	// budget governs and the 120k sibling is fatal; an exponent form is valid.
	nonASCII := "٥٠"
	if n, ok := EngineInt(nonASCII); ok {
		t.Errorf("non-ASCII digits must be refused, got %d", n)
	}
	if got := EffectiveACW(nonASCII, "", "200000"); got != "200000" {
		t.Errorf("a refused pin leaves the derived budget, got %q", got)
	}
	budget := EffectiveACW("1e5", "", "")
	conf, set = hazardWorld(t, "big=400000 mid=300000", `{"availableModels":["big","mid"]}`)
	if out := hazardNames(t, HazardIn{Profile: "gw", Budget: budget, Ceiling: "400000", Sources: []string{set}, Conf: conf}); strings.Contains(out, "mid") {
		t.Errorf("an engine-valid exponent pin must govern, leaving the sibling safe, got %q", out)
	}
	if out := hazardNames(t, HazardIn{Profile: "gw", Budget: "400000", Ceiling: "400000", Sources: []string{set}, Conf: conf}); !strings.Contains(out, "mid") {
		t.Errorf("control: without the pin the sibling is fatal, got %q", out)
	}
}
