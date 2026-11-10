package cli

import "testing"

// The read form's one line, for each way a window is learned (#372): the
// count, its unit, the model it belongs to, and where the number came from —
// so a line copied out of the terminal is clear on its own.
func TestContextAnswerNamesTheCountItsUnitAndWhereItCameFromIssue372(t *testing.T) {
	cases := []struct {
		model    string
		n        int64
		label    string
		unprobed bool
		want     string
	}{
		{"claude-opus-5", 500000, "manual", false,
			"gw: claude-opus-5 context 500,000 tokens · set by hand"},
		{"known.model", 200000, "probed", false,
			"gw: known.model context 200,000 tokens · found by ccmorph"},
		{"claude-opus-5", 1000000, "catalogue-estimate", false,
			"gw: claude-opus-5 context 1,000,000 tokens · estimated from the model catalogue"},
		{"brand-new", 200000, "probed", true,
			"gw: brand-new context 200,000 tokens · left at Claude Code's default"},
	}
	for _, c := range cases {
		if got := contextAnswer("gw", c.model, c.n, c.label, c.unprobed); got != c.want {
			t.Errorf("%s: got %q want %q", c.model, got, c.want)
		}
	}
}

// The read and inspect apply the profile's hand-set ceiling parsed as launch
// and Claude Code parse it (#372): 1e5 and 100,000 are the 100,000 a session
// runs at, and a value the engine rejects is no ceiling.
func TestEnvCeilingReadsTheHandSetCapAsLaunchDoesIssue372(t *testing.T) {
	const k = "env.CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	for _, c := range []struct {
		f    map[string]string
		want int64
		ok   bool
	}{
		{map[string]string{k: "300000"}, 300000, true},
		{map[string]string{k: "1e5"}, 100000, true},
		{map[string]string{k: "100,000"}, 100000, true},
		{map[string]string{k: "0"}, 0, false},
		{map[string]string{k: "junk100000"}, 0, false},
		{map[string]string{}, 0, false},
		{nil, 0, false},
	} {
		if got, ok := envCeiling(c.f); got != c.want || ok != c.ok {
			t.Errorf("envCeiling(%v) = %d, %v; want %d, %v", c.f, got, ok, c.want, c.ok)
		}
	}
}
