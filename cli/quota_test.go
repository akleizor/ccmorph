package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
// runs at, and a value the engine rejects is no ceiling. A `claude-*` id is
// the one model the ceiling never binds (#447): Claude Code ignores the var
// for its own ids, so there it is no ceiling at all.
func TestEnvCeilingReadsTheHandSetCapAsLaunchDoesIssue372(t *testing.T) {
	const k = "env.CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	for _, c := range []struct {
		model string
		f     map[string]string
		want  int64
		ok    bool
	}{
		{"glm-5.2", map[string]string{k: "300000"}, 300000, true},
		{"glm-5.2", map[string]string{k: "1e5"}, 100000, true},
		{"glm-5.2", map[string]string{k: "100,000"}, 100000, true},
		{"glm-5.2", map[string]string{k: "0"}, 0, false},
		{"glm-5.2", map[string]string{k: "junk100000"}, 0, false},
		{"glm-5.2", map[string]string{}, 0, false},
		{"glm-5.2", nil, 0, false},
		{"claude-sonnet-5-5", map[string]string{k: "300000"}, 0, false},
		{"claude-opus-5[1m]", map[string]string{k: "1e5"}, 0, false},
	} {
		if got, ok := envCeiling(c.f, c.model); got != c.want || ok != c.ok {
			t.Errorf("envCeiling(%v, %s) = %d, %v; want %d, %v", c.f, c.model, got, ok, c.want, c.ok)
		}
	}
}

// #447: the launch path and the window chain it answers for skip a hand-set
// CLAUDE_CODE_MAX_CONTEXT_TOKENS for an id starting with `claude-` — Claude
// Code resolves its own ids from its catalogue and ignores the var — but the
// #372 read applied the ceiling to every model, so it answered a manual cap a
// claude-* session never runs under. The read must answer the resolver's own
// window there, and keep answering the ceiling for every other id. The core is
// stubbed to one catalogue estimate, a count apart from the ceiling, so an
// answer of the ceiling can only be the manual cap.
func TestTheReadKeepsAHandSetCeilingOffAClaudeIDIssue447(t *testing.T) {
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "gw.conf"),
		[]byte("type=apikey\nenv.CLAUDE_CODE_MAX_CONTEXT_TOKENS=200000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	core := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s\\n' " +
		"'{\"window\":1000000,\"send_id\":\"m\",\"source\":\"catalogue-bundled\"}'\n"
	if err := os.WriteFile(filepath.Join(core, "ccmorph-core"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &launcher{dir: state, libexec: core, migrated: true}
	for _, c := range []struct{ model, want string }{
		{"claude-sonnet-5-5", "gw: claude-sonnet-5-5 context 1,000,000 tokens · estimated from the model catalogue"},
		{"claude-opus-5[1m]", "gw: claude-opus-5[1m] context 1,000,000 tokens · estimated from the model catalogue"},
		{"glm-5.2", "gw: glm-5.2 context 200,000 tokens · set by hand"},
	} {
		old := os.Stdout
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = w
		rc := l.context([]string{"gw", c.model})
		w.Close()
		os.Stdout = old
		out, _ := io.ReadAll(r)
		if rc != 0 || strings.TrimSpace(string(out)) != c.want {
			t.Errorf("%s: rc %d, got %q want %q", c.model, rc, strings.TrimSpace(string(out)), c.want)
		}
	}
}
