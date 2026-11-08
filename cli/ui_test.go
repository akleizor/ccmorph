package cli

import (
	"strings"
	"testing"
)

// fit cuts on whole characters (valid UTF-8) and marks the cut with an ellipsis.
func TestFitCutsWholeCharacters(t *testing.T) {
	if got := fit("Sign-in method — demo", 18); got != "Sign-in method — …" {
		t.Fatalf("fit = %q", got)
	}
	if got := fit("short", 18); got != "short" {
		t.Fatalf("fit = %q", got)
	}
}

// startPos names the row to open on: the start row itself, or the first when
// start is empty or names nothing (issue 271).
func TestStartPosNamesTheStartRow(t *testing.T) {
	rows := []string{"alpha\tFirst", "beta\tSecond", "gamma\tThird"}
	if got := startPos(rows, ""); got != 0 {
		t.Fatalf("startPos(empty) = %d", got)
	}
	if got := startPos(rows, "beta"); got != 1 {
		t.Fatalf("startPos(beta) = %d", got)
	}
	if got := startPos(rows, "gamma"); got != 2 {
		t.Fatalf("startPos(gamma) = %d", got)
	}
	if got := startPos(rows, "zeta"); got != 0 {
		t.Fatalf("startPos(unknown) = %d", got)
	}
}

// fzfVersionOK draws the pos(N) line at 0.36.0, the first release with the
// start event and the action (issue 271).
func TestFzfVersionOK(t *testing.T) {
	for _, version := range []string{"0.75.0 (Homebrew)", "0.48.1", "0.36.0", "1.0.0"} {
		if !fzfVersionOK(version) {
			t.Fatalf("fzfVersionOK(%q) = false", version)
		}
	}
	for _, version := range []string{"0.35.1", "0.9.11", "not a version"} {
		if fzfVersionOK(version) {
			t.Fatalf("fzfVersionOK(%q) = true", version)
		}
	}
}

// fzfHeader keeps the whole hint on a 56-column terminal, on two lines, and
// stays one line once it fits (issue 271).
func TestFzfHeaderFitsTheWholeHint(t *testing.T) {
	narrow := fzfHeader(56)
	lines := strings.Split(narrow, "\n")
	if len(lines) != 2 || lines[1] != "Enter Select · Esc Cancel" {
		t.Fatalf("fzfHeader(56) = %q", narrow)
	}
	// fzf leaves roughly columns-8 for a header line; each line must fit.
	for _, line := range lines {
		if cells(line) > 56-8 {
			t.Fatalf("fzfHeader(56) line %q does not fit", line)
		}
	}
	if got := fzfHeader(80); got != "↑/↓ Move · Type to filter · Enter Select · Esc Cancel" {
		t.Fatalf("fzfHeader(80) = %q", got)
	}
}
