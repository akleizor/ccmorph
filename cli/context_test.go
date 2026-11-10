package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GH-370: the set form's warning trusted the discovered table — empty whenever
// no model could be sized — so a model the gateway lists was warned about like
// a made-up name. It now asks the route's own listing, the one `ccmorph models`
// reads. The gateway-listed, mapped and unreadable-listing cases are the zsh
// suite's (fake curl, tests/ccmorph_test.zsh); these are the cases decided
// without a fetch.
func TestTheUnlistedModelWarningAsksTheRouteThatCannotBeAskedOffline(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name+".conf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := &launcher{dir: dir}
	// A provider's catalogue is not readable with the profile's own credentials,
	// so the record its probe fills is the evidence: a bridge refuses Anthropic
	// ids, and an unprobed id — a made-up name or the provider's own — is warned
	// with the probe that would list it.
	write("b", "type=provider\nprovider=codex\n")
	for _, model := range []string{"claude-opus-5-5", "gpt-5", "nosuch-model"} {
		for _, value := range []string{"100k", "64000"} {
			msg := l.unlistedWarning("b", model, value)
			for _, needle := range []string{
				"'" + model + "' is not listed by b",
				"the limit is saved for when it appears",
				"ccmorph probe b",
			} {
				if !strings.Contains(msg, needle) {
					t.Fatalf("provider warning for %s at %s is missing %q: %q", model, value, needle, msg)
				}
			}
		}
	}
	// auto pins nothing, so there is nothing to warn about.
	if msg := l.unlistedWarning("b", "claude-opus-5-5", "auto"); msg != "" {
		t.Fatalf("auto must not warn: %q", msg)
	}
}

// GH-370: the warning was one line of 140 cells, so an 80-column terminal broke
// it inside "Check". Wrapped to 80, no line may be wider, and the words must be
// exactly the single line's words: wrapping re-breaks lines, it never splits or
// drops a word (#364's rule).
func TestTheUnlistedModelWarningNeverCutsAWordAtEightyColumns(t *testing.T) {
	msg := unlistedWarningText("alpha-sonnet-1", "work", "apikey")
	single := strings.Join(warnLines(msg, 32767), "\n") // a piped stderr keeps one line
	if single != "ccmorph: warning: "+msg {
		t.Fatalf("piped: %q", single)
	}
	wrapped := warnLines(msg, 80)
	if len(wrapped) < 2 {
		t.Fatalf("a 140-cell warning must wrap at 80 columns: %q", wrapped)
	}
	for _, line := range wrapped {
		if cells(line) > 80 {
			t.Errorf("line of %d cells: %q", cells(line), line)
		}
	}
	if got, want := strings.Join(strings.Fields(strings.Join(wrapped, "\n")), " "),
		strings.Join(strings.Fields(single), " "); got != want {
		t.Errorf("wrapping lost or cut words:\n got: %q\nwant: %q", got, want)
	}
	if lines := warnLines("", 80); len(lines) != 0 {
		t.Errorf("an empty warning prints nothing: %q", lines)
	}
}
