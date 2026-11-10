package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
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

// fit ends on a whole word when one fits before the width, so a cut row never
// ends in half a word (#369); the space before the ellipsis stays, and a single
// long word still hard-cuts, because a narrower lie would be worse.
func TestFitEndsOnAWholeWord(t *testing.T) {
	if got := fit("install cliproxyapi", 12); got != "install …" {
		t.Fatalf("fit = %q", got)
	}
	// The More sign-ins row's detail — the real one, whose names #366 made
	// too long for any 80-column budget — at the native menu's 60-cell detail
	// budget and the numbered list's 57: each cut lands after a whole name,
	// never inside one.
	if got := fit(browserLogins+" (cliproxyapi)", 60); got != "Antigravity, Codex, Devin, Kimi For Coding (global), Kimi …" {
		t.Fatalf("fit = %q", got)
	}
	if got := fit(browserLogins+" (cliproxyapi)", 57); got != "Antigravity, Codex, Devin, Kimi For Coding (global), …" {
		t.Fatalf("fit = %q", got)
	}
	if got := fit("cliproxyapi", 6); got != "clipr…" {
		t.Fatalf("fit = %q", got)
	}
}

// moreLine names the rows a native menu's window cuts, and keeps its line even
// when that side cuts nothing, so the menu keeps one shape while it scrolls.
func TestMoreLineNamesTheCutAndKeepsItsShape(t *testing.T) {
	plain := uiTheme{}
	if got := plain.moreLine("↓", 9); got != "\033[2K"+"  "+"↓ 9 more" {
		t.Fatalf("moreLine = %q", got)
	}
	if got := plain.moreLine("↑", 0); got != "\033[2K" {
		t.Fatalf("blank edge = %q", got)
	}
}

// fzfAtLeast gates the scrollbar, the Home/End jump and the adaptive window on
// the fzf release that added them, and keeps an unreadable version conservative.
func TestFzfAtLeast(t *testing.T) {
	for _, tc := range []struct {
		version    string
		at36, at71 bool
	}{
		{"0.75.0 (brew)\n", true, true},
		{"0.71.0\n", true, true},
		{"0.70.0 (fd)\n", true, false},
		{"0.36.0\n", true, false},
		{"0.35.1 (homebrew)\n", false, false},
		{"0.29.0\n", false, false},
		{"1.2.3\n", true, true},
		{"", false, false},
		{"not a version", false, false},
	} {
		if got := fzfAtLeast(tc.version, 36); got != tc.at36 {
			t.Errorf("fzfAtLeast(%q, 36) = %v, want %v", tc.version, got, tc.at36)
		}
		if got := fzfAtLeast(tc.version, 71); got != tc.at71 {
			t.Errorf("fzfAtLeast(%q, 71) = %v, want %v", tc.version, got, tc.at71)
		}
	}
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// An fzf too old for the scrollbar gets a window sized by ccmorph: the whole
// list when the terminal allows it, and a header that says rows are cut when
// not. Every version cycles; a newer one gets the scrollbar and the Home/End
// jump (#278).
func TestFzfArgsNeverHideRowsSilently(t *testing.T) {
	cut := "More rows than fit here:"
	old := fzfArgs("0.35.1 (homebrew)\n", 19, 45, "Provider", nil)
	if flagValue(old, "--height") != "24" || !strings.HasPrefix(flagValue(old, "--header"), "↑/↓ Move") {
		t.Errorf("a list that fits gets a window of rows+5: %v", old)
	}
	if !strings.Contains(strings.Join(old, " "), "--cycle") || strings.Contains(strings.Join(old, " "), "--scrollbar") ||
		strings.Contains(strings.Join(old, " "), "end:last") {
		t.Errorf("old fzf cycles and gets no flags it lacks: %v", old)
	}
	tight := fzfArgs("0.29.0\n", 19, 24, "Provider", nil)
	if flagValue(tight, "--height") != "23" || !strings.HasPrefix(flagValue(tight, "--header"), cut) {
		t.Errorf("a list the terminal cannot hold must say rows are cut: %v", tight)
	}
	if got := flagValue(fzfArgs("", 3, 24, "P", nil), "--height"); got != "8" {
		t.Errorf("an unreadable version is treated as old: height %q", got)
	}
	mid := fzfArgs("0.40.0\n", 19, 45, "Provider", nil)
	if flagValue(mid, "--height") != "45%" || !strings.Contains(strings.Join(mid, " "), "--scrollbar") ||
		!strings.Contains(strings.Join(mid, " "), "home:first,end:last") {
		t.Errorf("0.40 gets the scrollbar and jump, not the adaptive window: %v", mid)
	}
	if got := flagValue(fzfArgs("0.75.0\n", 19, 45, "P", nil), "--height"); got != "~-1" {
		t.Errorf("0.75 grows the window to the list: %q", got)
	}
	for lines := 6; lines <= 60; lines++ {
		for n := 1; n <= 40; n++ {
			args := fzfArgs("0.29.0\n", n, lines, "P", nil)
			h, _ := strconv.Atoi(flagValue(args, "--height"))
			says := strings.HasPrefix(flagValue(args, "--header"), cut)
			if (n+fzfChrome > h) != says {
				t.Fatalf("n=%d lines=%d: height %d, header says cut = %v", n, lines, h, says)
			}
			if h > lines-1 {
				t.Fatalf("n=%d lines=%d: window %d overflows the terminal", n, lines, h)
			}
		}
	}
}

// A receipt never cuts its choice, and never squeezes out its question: the
// question yields space first, the choice survives whole. A summary line that
// ellipsizes half of what was picked names less than the picker above it did,
// and one that drops the question names nothing it answered (#361).
func TestReceiptNeverCutsTheChoiceIssue361(t *testing.T) {
	plain := uiTheme{}
	long := "https://a-very-long-endpoint.example.com:8443/v1/messages"
	got := plain.receiptLine("Provider — eps", long, 40)
	if !strings.Contains(got, long) || strings.Contains(got, "…") {
		t.Fatalf("receiptLine = %q, want the whole choice %q", got, long)
	}
	if !strings.HasPrefix(got, "  Provider — eps › ") {
		t.Fatalf("receiptLine = %q, want the question to lead when the choice outgrows the line", got)
	}
	// A 120-cell answer on a 100-column line: the question stays whole and the
	// answer wraps beneath it — cut or question-less are both wrong.
	if got, want := plain.receiptLine("Value: ", strings.Repeat("a", 120), 100), "  Value: › "+strings.Repeat("a", 120); got != want {
		t.Fatalf("receiptLine = %q, want %q", got, want)
	}
	// When the line cannot hold both, the title is what gives way — down to
	// receiptTitleFloor, no further.
	if got := plain.receiptLine("Provider — a-long-name", "eps", 20); got != "  Provider — … › eps" {
		t.Fatalf("narrow receiptLine = %q, want %q", got, "  Provider — … › eps")
	}
	// A choice that fits a line of its own but not beside the question breaks
	// onto it: no line outgrows the terminal, and the choice is whole.
	got = plain.receiptLine("A heading far wider than this pane", "A fairly long option label", 30)
	if want := "  A heading far wider than …" + " ›\n" + "  A fairly long option label"; got != want {
		t.Fatalf("narrow receiptLine = %q, want %q", got, want)
	}
	for _, line := range strings.Split(got, "\n") {
		if cells(line) > 30 {
			t.Fatalf("line %q is %d cells, want at most 30", line, cells(line))
		}
	}
	// A typed question ends in ": ": its space does not double the receipt's.
	if got := plain.receiptLine("Value: ", "an-answer", 100); got != "  Value: › an-answer" {
		t.Fatalf("receiptLine = %q, want %q", got, "  Value: › an-answer")
	}
}

// An answered search picker leaves one line that names the choice: the row's
// own label, not the description fzf padded onto it — the picker already
// showed the description, and the receipt cut it mid-phrase (#361).
func TestSearchReceiptNamesTheLabelNotTheDescriptionIssue361(t *testing.T) {
	dir := t.TempDir()
	// Pure builtins: PATH holds only this dir, so the fake must not call out
	// to head or cat. It answers fzf by printing stdin's first row.
	fzf := "#!/bin/sh\nIFS= read -r line\nprintf '%s\\n' \"$line\"\n"
	if err := os.WriteFile(filepath.Join(dir, "fzf"), []byte(fzf), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TERM", "xterm")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	v, rc := uiSelect("search", "Provider — eps", "custom\tCustom endpoint\tAnthropic or any gateway URL", "")
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	if rc != 0 || v != "custom" {
		t.Fatalf("uiSelect = (%q, %d), want (\"custom\", 0)", v, rc)
	}
	if n := strings.Count(string(b), "\n"); n != 1 {
		t.Fatalf("the answer is %d lines, want the one receipt line: %q", n, b)
	}
	if !strings.Contains(string(b), "Custom endpoint") || strings.Contains(string(b), "Anthropic or any gateway") {
		t.Fatalf("the receipt must name the label and not the description: %q", b)
	}
}

// typedLines is the height of an answered prompt's last line: "❯ " plus the
// echoed cells, wrapped at the terminal's width. Reaching the last column is
// not a wrap — the next character is — so the erase that folds the question
// counts every line the answer touched (#361).
func TestTypedLinesCountsTheWrap(t *testing.T) {
	for _, tc := range []struct{ echoed, columns, want int }{
		{0, 100, 1}, {97, 100, 1}, {98, 100, 1}, {99, 100, 2}, {198, 100, 2}, {199, 100, 3},
		{0, 80, 1}, {78, 80, 1}, {79, 80, 2},
	} {
		if got := typedLines(tc.echoed, tc.columns); got != tc.want {
			t.Errorf("typedLines(%d, %d) = %d, want %d", tc.echoed, tc.columns, got, tc.want)
		}
	}
}

// A search picker the person leaves (fzf answers Esc with exit 130) prints the
// one Cancelled line every picker answers with — not silence, not a usage line.
func TestSearchCancelPrintsTheOneCancelledLineIssue280(t *testing.T) {
	dir := t.TempDir()
	fzf := "#!/bin/sh\nexit 130\n"
	if err := os.WriteFile(filepath.Join(dir, "fzf"), []byte(fzf), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TERM", "xterm")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	v, rc := uiSelect("search", "Profile", "demo\tDemo profile", "labels")
	os.Stderr = old
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	if rc != 1 || v != "" {
		t.Fatalf("uiSelect = (%q, %d), want (\"\", 1)", v, rc)
	}
	if n := strings.Count(string(b), "Cancelled"); n != 1 {
		t.Fatalf("stderr printed Cancelled %d times, want 1: %q", n, b)
	}
	if line := strings.TrimRight(string(b), "\n"); line != "  Cancelled" {
		t.Fatalf("stderr = %q, want the one line \"  Cancelled\"", b)
	}
}
