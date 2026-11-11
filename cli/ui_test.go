package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// numberWindow is how the numbered menu fits the screen (#387): the whole
// list while heading, blank, rows and the three prompt lines fit; else the
// first rows that leave room for the "N more" line and the fzf hint beside
// that chrome, so the title and the first rows stay in view. One row shows
// even where the screen is too short for all the chrome. Once a failed answer
// has left its message standing under the list, that line counts against the
// fit too: a list that filled the screen cuts rather than push the title off.
func TestNumberWindowFitsTheScreenIssue387(t *testing.T) {
	for _, tc := range []struct {
		rows, height int
		noted        bool
		shown        int
		cut          bool
	}{
		{260, 24, false, 17, true}, // the issue's gateway: 17 rows, 245 more
		{260, 24, true, 17, true},  // the message takes the hint's line, not a row
		{20, 24, false, 17, true},  // one row past fitting cuts, not the whole list
		{19, 24, false, 19, false}, // 19 rows + 5 chrome lines fit a 24-line screen
		{19, 24, true, 17, true},   // beside the standing message they do not
		{18, 24, true, 18, false},  // 18 rows still fit with the message
		{2, 24, false, 2, false},
		{2, 24, true, 2, false},
		{1, 24, false, 1, false},
		{260, 8, false, 1, true}, // a short screen still shows one row
		{260, 8, true, 1, true},
		{2, 6, false, 1, true},
	} {
		shown, cut := numberWindow(tc.rows, tc.height, tc.noted)
		if shown != tc.shown || cut != tc.cut {
			t.Fatalf("numberWindow(%d, %d, %v) = (%d, %v), want (%d, %v)",
				tc.rows, tc.height, tc.noted, shown, cut, tc.shown, tc.cut)
		}
	}
}

// numberList paints a list longer than the screen cut to it, with the "N more"
// line naming the cut and the fzf hint naming the search fzf would add and
// this machine's own step to get it (#387). A dumb terminal gets the same cut
// and hint as plain text, and a failed answer's message takes the hint's line,
// so the retry keeps the block's height. A piped stderr still reports 24 rows
// to termSize, so the cut renders headless too.
func TestNumberListCutsToTheScreenAndNamesTheFzfStepIssue387(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "brew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	rows := []string{"@keep\tKeep native-model"}
	for n := 1; n <= 260; n++ {
		rows = append(rows, fmt.Sprintf("openai/model-%d-instruct\topenai/model-%d-instruct\tfake", n, n))
	}
	rows = append(rows, "@custom\tEnter a custom model ID…")

	render := func(rs []string, mode numberMode, note string) (string, int) {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stderr
		os.Stderr = w
		lines := uiTheme{}.numberList("Default model — many", rs, "labels", mode, note)
		os.Stderr = old
		w.Close()
		b, _ := io.ReadAll(r)
		r.Close()
		return string(b), lines
	}
	cutRender := func(rs []string, mode numberMode, note string) (string, int) {
		out, lines := render(rs, mode, note)
		if lines != 21 {
			t.Fatalf("numberList(%v, %q) reported %d lines, want 21", mode, note, lines)
		}
		if got := strings.Count(out, "\n"); got != 21 {
			t.Fatalf("numberList(%v, %q) painted %d lines, want 21", mode, note, got)
		}
		for _, want := range []string{
			"  Default model — many\n",      // the title leads the block
			"  2) openai/model-1-instruct",  // the catalogue starts in view
			" 17) openai/model-16-instruct", // the last row that fits
		} {
			if !strings.Contains(out, want) {
				t.Fatalf("cut render missing %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "model-17-instruct") {
			t.Fatalf("cut render shows a row past the window:\n%s", out)
		}
		return out, lines
	}
	// A terminal: heading, blank, 17 rows, the more line, the hint — 21 lines,
	// three left for the question below them.
	out, _ := cutRender(rows, numberFold, "")
	for _, want := range []string{"  ↓ 245 more\n", "  fzf adds search: brew install fzf\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("fold render missing %q:\n%s", want, out)
		}
	}
	// A dumb terminal is a screen without cursor codes: the same cut, the same
	// hint, not one escape (#387).
	out, _ = cutRender(rows, numberDumb, "")
	for _, want := range []string{"  ↓ 245 more\n", "  fzf adds search: brew install fzf\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dumb render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("dumb render must carry no cursor codes:\n%s", out)
	}
	// A dumb terminal cannot erase a failed round: its retry repaints the same
	// cut block, the message in the hint's line, still without one escape (#387).
	out, _ = cutRender(rows, numberDumb, numberMiss)
	if !strings.Contains(out, "  Please choose a number or part of a name.\n") ||
		strings.Contains(out, "fzf adds search") || strings.Contains(out, "\x1b") {
		t.Fatalf("dumb retry render must give the message the hint's line, plain:\n%s", out)
	}
	// A failed answer's message takes the hint's line, so the block a retry
	// repaints is as tall as the one it answered (#387).
	out, _ = cutRender(rows, numberFold, numberMiss)
	if !strings.Contains(out, "  Please choose a number or part of a name.\n") {
		t.Fatalf("retry render missing the message:\n%s", out)
	}
	if strings.Contains(out, "fzf adds search") {
		t.Fatalf("retry render must give the message the hint's line:\n%s", out)
	}
	// A list that fit the screen whole cuts rather than overflow beside the
	// message: 19 rows fit alone, not with the standing line.
	out, lines := render(rows[:19], numberFold, "")
	if lines != 21 || strings.Contains(out, "more\n") || strings.Contains(out, "fzf adds search") {
		t.Fatalf("19 rows fit alone: %d lines\n%s", lines, out)
	}
	out, _ = cutRender(rows[:19], numberFold, numberMiss)
	if !strings.Contains(out, "  ↓ 2 more\n") {
		t.Fatalf("19 rows beside the message must cut by two:\n%s", out)
	}
	// Without a screen the list is data, not display: every row, no cut, no hint.
	out, lines = render(rows, numberPipe, "")
	if lines != len(rows)+2 {
		t.Fatalf("plain numberList reported %d lines, want %d", lines, len(rows)+2)
	}
	if !strings.Contains(out, "262) Enter a custom model ID…") || strings.Contains(out, "more\n") || strings.Contains(out, "fzf adds search") {
		t.Fatalf("plain render must show every row and no cut or hint:\n%s", out[:2000])
	}
}

// tailReader hands out one byte per Read, then reads nothing, as a raw
// terminal's VTIME read does. After its first byte it raises sig on stop:
// a leave that lands between a sequence's bytes.
type tailReader struct {
	data []byte
	stop chan os.Signal
	sig  os.Signal
}

func (r *tailReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		time.Sleep(time.Millisecond)
		return 0, nil
	}
	p[0], r.data = r.data[0], r.data[1:]
	if r.sig != nil {
		r.stop <- r.sig
		r.sig = nil
	}
	return 1, nil
}

func TestSeqTailReadsEachHomeEndSpellingIssue388(t *testing.T) {
	for _, c := range []struct {
		in   string
		done bool
	}{
		{"H", true}, {"F", true}, {"A", true}, {"B", true},
		{"1~", true}, {"4~", true}, {"7~", true}, {"8~", true},
		{"", false},   // a bare Esc: the leave it asks for
		{"12", false}, // never finished: ignored
	} {
		stop := make(chan os.Signal, 1)
		k := &keys{in: &tailReader{data: []byte(c.in), stop: stop}}
		tail, done, s := k.seqTail(stop)
		if tail != c.in || done != c.done || s != nil {
			t.Errorf("seqTail(%q) = %q, %v, %v; want %q, %v, nil", c.in, tail, done, s, c.in, c.done)
		}
	}
}

func TestSeqTailHandsBackASignalMidSequenceIssue388(t *testing.T) {
	stop := make(chan os.Signal, 1)
	k := &keys{in: &tailReader{data: []byte("1"), stop: stop, sig: syscall.SIGTERM}}
	tail, done, s := k.seqTail(stop)
	if tail != "1" || done || s != syscall.SIGTERM {
		t.Fatalf("seqTail = %q, %v, %v; want \"1\", false, terminated", tail, done, s)
	}
}
