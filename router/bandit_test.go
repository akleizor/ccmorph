package router

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
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
		{"accepted beats a wrong verdict", Row{"verdict": "wrong", "accepted": true}, true, acceptedWeight, true},
		{"decider pass", Row{"verdict": "pass"}, true, deciderWeight, true},
		{"decider wrong", Row{"verdict": "wrong"}, false, deciderWeight, true},
		{"format", Row{"verdict": "format"}, false, 0, false},
		{"a row from before verdicts", Row{"quality": false}, false, deciderWeight, true},
		{"timeout", Row{"outcome": "error", "failure": "timeout"}, false, acceptedWeight, true},
		{"quota", Row{"outcome": "error", "failure": "quota"}, false, 0, false},
		{"auth", Row{"outcome": "auth"}, false, 0, false},
		{"unsettled", Row{"outcome": nil}, false, 0, false},
	} {
		pass, weight, counts := evidence(tc.row)
		if pass != tc.pass || weight != tc.weight || counts != tc.counts {
			t.Errorf("%s: evidence = %v %v %v, want %v %v %v", tc.name, pass, weight, counts, tc.pass, tc.weight, tc.counts)
		}
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
