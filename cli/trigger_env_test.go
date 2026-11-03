package cli

import (
	"slices"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// The trigger knobs arrive whole (a tab inside a value never bleeds into a
// neighbour), the kill switch reads like the engine, and a conf may declare it.
func TestTriggerEnvReadsTheThreeKnobsWhole(t *testing.T) {
	l := &launcher{}
	pct, out, no1m := l.triggerEnv([]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=8\t0", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=9000"})
	if pct != "8\t0" || out != "9000" || no1m != "" {
		t.Errorf("got %q %q %q", pct, out, no1m)
	}
	pct, out, no1m = l.triggerEnv([]string{"CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=80", "CLAUDE_CODE_MAX_OUTPUT_TOKENS=8000", "CLAUDE_CODE_DISABLE_1M_CONTEXT=1"})
	if pct != "80" || out != "8000" || no1m != "1" {
		t.Errorf("got %q %q %q", pct, out, no1m)
	}
	for v, want := range map[string]string{"1": "1", "true": "1", " on ": "1", "yes ": "1", "\u0085on": "", "0": "", "onx": "", "": ""} {
		if _, _, got := l.triggerEnv([]string{"CLAUDE_CODE_DISABLE_1M_CONTEXT=" + v}); got != want {
			t.Errorf("kill switch %q: got %q, want %q", v, got, want)
		}
	}
	if !slices.Contains(profile.ProfileEnv, "CLAUDE_CODE_DISABLE_1M_CONTEXT") {
		t.Error("the kill switch must be an allowlisted profile env key")
	}
}
