package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// The terminal UI that lib/ccmorph-ui.zsh was: ROWS are ID<TAB>label[<TAB>description];
// `all` shows the ID as the label. Menus paint on stderr; only IDs go to stdout.

const uiMargin = "  "
const uiGap = 4

type uiTheme struct{ bold, dim, reset, accent, focus, gray, red string }

var fzfTheme = "fg:-1,bg:-1,hl:#b1b9f9,fg+:#b1b9f9,bg+:-1,hl+:#b1b9f9,pointer:#b1b9f9,marker:#b1b9f9,prompt:#d77757,header:#999999,info:#999999,border:#888888"

func rgb(hex string) string {
	r, _ := strconv.ParseUint(hex[0:2], 16, 8)
	g, _ := strconv.ParseUint(hex[2:4], 16, 8)
	b, _ := strconv.ParseUint(hex[4:6], 16, 8)
	return fmt.Sprintf("\033[38;2;%d;%d;%dm", r, g, b)
}

// themeFor is _ccm_ui_theme for one stream.
func themeFor(fd int) uiTheme {
	if !isTTY(fd) || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return uiTheme{}
	}
	return uiTheme{"\033[1m", "\033[2m", "\033[0m", rgb("d77757"), rgb("b1b9f9"), rgb("999999"), rgb("ff6b80")}
}

func uiErr(t uiTheme, msg string) { fmt.Fprintf(os.Stderr, "%sccmorph:%s %s\n", t.red, t.reset, msg) }

// cells is the terminal width of s.
// shortcut: wide ranges are CJK/Hangul/fullwidth/emoji only, no full East Asian Width table; add one if a picker misaligns.
func cells(s string) int {
	n := 0
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d:
		case r >= 0x1100 && r <= 0x115f, r >= 0x2e80 && r <= 0xa4cf, r >= 0xac00 && r <= 0xd7a3,
			r >= 0xf900 && r <= 0xfaff, r >= 0xfe30 && r <= 0xfe4f, r >= 0xff00 && r <= 0xff60,
			r >= 0xffe0 && r <= 0xffe6, r >= 0x1f300 && r <= 0x1faff, r >= 0x20000 && r <= 0x3fffd:
			n += 2
		default:
			n++
		}
	}
	return n
}

func noCntrl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// fit is _ccm_ui_fit: cut to width cells with an ellipsis.
func fit(s string, width int) string {
	s = noCntrl(s)
	if width < 1 {
		return ""
	}
	if cells(s) <= width {
		return s
	}
	for cells(s) >= width {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s + "…"
}

func visible(row, display string) string {
	if display == "labels" {
		if i := strings.IndexByte(row, '\t'); i >= 0 {
			return row[i+1:]
		}
	}
	return row
}

func rowID(row string) string {
	if i := strings.IndexByte(row, '\t'); i >= 0 {
		return row[:i]
	}
	return row
}

func labelWidth(rows []string, display string) int {
	w := 0
	for _, r := range rows {
		if c := cells(noCntrl(rowID(visible(r, display)))); c > w {
			w = c
		}
	}
	return w
}

// typedLines is how many terminal lines an answered prompt's last line
// occupies: "❯ " plus ECHOED cells, wrapping at COLUMNS. Reaching the last
// column is not a wrap — the next character is — so folding the question
// away erases every line the answer touched (#361).
func typedLines(echoed, columns int) int {
	return (1+echoed)/columns + 1
}

// termSize is _ccm_ui_size: rows, columns of stderr's terminal.
func termSize() (int, int) {
	if !isTTY(2) {
		return 24, 32767
	}
	r, c := winsize(2)
	if r < 1 {
		r = 24
	}
	if c < 1 {
		c = 80
	}
	return r, c
}

func (t uiTheme) heading(title string, columns int) string {
	return uiMargin + t.accent + t.bold + fit(title, columns-len(uiMargin)) + t.reset
}

// label is _ccm_ui_label: ROW is label[<TAB>description].
func (t uiTheme) label(row string, width int, focused bool, rowWidth int) string {
	lab, detail, has := strings.Cut(row, "\t")
	lab = fit(lab, width)
	pad := ""
	if has {
		if rowWidth > 0 {
			detail = fit(detail, rowWidth-width-uiGap)
		}
		if detail != "" {
			pad = strings.Repeat(" ", max(0, width-cells(lab)+uiGap))
		}
	}
	out := lab
	if focused {
		out = t.focus + lab + t.reset
	}
	if has && detail != "" {
		out += pad + t.gray + noCntrl(detail) + t.reset
	}
	return out
}

func splitRows(rows string) []string {
	var out []string
	for _, r := range strings.Split(rows, "\n") {
		if r != "" {
			out = append(out, r)
		}
	}
	return out
}

// cancelled is the one line every picker answers with when the person leaves it
// without choosing, so Esc means the same thing in every command.
func (t uiTheme) cancelled() {
	fmt.Fprintln(os.Stderr, uiMargin+t.gray+"Cancelled"+t.reset)
}

// eraseLines folds away the last n printed lines: cursor up n, column 0, erase
// to the end of the screen. A screen that scrolled past its top erases from
// the top, so the fold still ends one line.
func eraseLines(n int) {
	if n > 0 {
		fmt.Fprintf(os.Stderr, "\033[%dA\r\033[J", n)
	}
}

// uiSelect is ccm_ui_select: the chosen ID, or rc 1 cancelled, 2 invalid or no
// terminal to pick with, 128+n signal.
func uiSelect(mode, title, rows, display string) (string, int) {
	if display == "" {
		display = "labels"
	}
	t := themeFor(2)
	list := splitRows(rows)
	for _, r := range list {
		id := rowID(r)
		if id == "" || strings.IndexFunc(id, unicode.IsControl) >= 0 {
			uiErr(t, "invalid choice ID")
			return "", 2
		}
	}
	switch mode {
	case "native":
		return t.native(title, list, display)
	case "search":
		if display == "all" {
			for i, r := range list {
				list[i] = rowID(r) + "\t" + r
			}
		}
		return t.search(title, list)
	}
	uiErr(t, "unknown UI mode: "+mode)
	return "", 2
}

// list is ccm_ui_list: the numbered rows, painted on stderr.
func (t uiTheme) list(prompt string, rows []string, display string) {
	_, columns := termSize()
	index := len(strconv.Itoa(len(rows)))
	rowWidth := columns - len(uiMargin) - index - 2
	width := min(labelWidth(rows, display), rowWidth)
	fmt.Fprintln(os.Stderr, t.heading(prompt, columns))
	fmt.Fprintln(os.Stderr)
	for i, r := range rows {
		fmt.Fprintf(os.Stderr, "%s%*d) %s\n", uiMargin, index, i+1, t.label(visible(r, display), width, false, rowWidth))
	}
}

func (t uiTheme) numbered(prompt string, rows []string, display string) (string, int) {
	if len(rows) == 0 {
		uiErr(t, "no available choices")
		return "", 1
	}
	t.list(prompt, rows, display)
	if !(isTTY(2) && os.Getenv("TERM") != "dumb") {
		// A pipe or a dumb terminal keeps the plain list: no cursor codes.
		for {
			choice, rc := uiInput(fmt.Sprintf("Select [1-%d] (q to cancel): ", len(rows)), false)
			if rc != 0 || choice == "q" || choice == "Q" {
				if rc == 0 {
					// uiInput has already answered its own cancels; q is a
					// line only this menu knows means leave.
					t.cancelled()
				}
				return "", 1
			}
			if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(rows) && choice == strconv.Itoa(n) {
				return rowID(rows[n-1]), 0
			}
			fmt.Fprintln(os.Stderr, "Please choose one of the numbered options.")
		}
	}
	// On a terminal the answered list folds like every picker: one receipt
	// line names the choice, and the rows and the dead key hint do not stay
	// above it (#361). Each uiInput round has already folded itself to one
	// line, so one line per round and per reprompt is what adds to the list.
	lines := len(rows) + 2 // heading, blank, rows
	for {
		choice, rc := uiInput(fmt.Sprintf("Select [1-%d] (q to cancel): ", len(rows)), false)
		lines++
		if rc != 0 || choice == "q" || choice == "Q" {
			// A leave answers with the one Cancelled line, not the list above
			// it: uiInput's own Cancelled is erased with the rest.
			eraseLines(lines)
			t.cancelled()
			return "", 1
		}
		if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(rows) && choice == strconv.Itoa(n) {
			eraseLines(lines)
			t.receipt(prompt, rowID(visible(rows[n-1], display)))
			return rowID(rows[n-1]), 0
		}
		fmt.Fprintln(os.Stderr, "Please choose one of the numbered options.")
		lines++
	}
}

// rawTTY puts the terminal in -echo -icanon -isig min 0 time 1 (reads return within 0.1s) and returns its restore.
func rawTTY(f *os.File) (func(), error) {
	old, err := ioctlTermios(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	raw := old
	raw.Lflag &^= syscall.ECHO | syscall.ICANON | syscall.ISIG
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 0, 1
	if err := setTermios(int(f.Fd()), &raw); err != nil {
		return nil, err
	}
	return func() { setTermios(int(f.Fd()), &old) }, nil
}

// keys reads the raw terminal on stdin synchronously: rawTTY's VTIME makes
// each read return within 0.1s, so signals and resizes are seen between reads
// and no reader is left behind to take a key meant for fzf or the next prompt.
type keys struct{}

func readKeys() *keys { return &keys{} }

const keyResize = -2

// next waits for one key; timeout 0 waits forever. ok false is EOF or timeout;
// a resize answers keyResize.
func (k *keys) next(timeout time.Duration, stop, winch <-chan os.Signal) (int, os.Signal, bool) {
	start := time.Now()
	b := make([]byte, 1)
	for {
		select {
		case s := <-stop:
			return 0, s, false
		case <-winch:
			return keyResize, nil, true
		default:
		}
		n, err := os.Stdin.Read(b)
		if n == 1 {
			return int(b[0]), nil, true
		}
		if err != nil && err != io.EOF {
			return 0, nil, false
		}
		if timeout > 0 && time.Since(start) >= timeout {
			return 0, nil, false
		}
	}
}

func sigRC(s os.Signal) int {
	switch s {
	case syscall.SIGINT:
		return 130
	case syscall.SIGTERM:
		return 143
	case syscall.SIGHUP:
		return 129
	}
	return 1
}

// native is _ccm_ui_native: an inline ↑/↓ menu on the terminal.
func (t uiTheme) native(prompt string, rows []string, display string) (string, int) {
	if len(rows) == 0 {
		uiErr(t, "no available choices")
		return "", 1
	}
	if !isTTY(0) || !isTTY(2) {
		uiErr(t, "selection needs a terminal; pass the value directly")
		fmt.Fprintln(os.Stderr, strings.Join(rows, "\n"))
		// rc 2, not a cancel: no picker ran, so the caller needs the direct
		// command, not a Cancelled line.
		return "", 2
	}
	if os.Getenv("TERM") == "dumb" {
		return t.numbered(prompt, rows, display)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(sigs)
	defer signal.Stop(winch)
	restore, err := rawTTY(os.Stdin)
	if err != nil {
		return "", 1
	}
	k := readKeys()
	width := labelWidth(rows, display)
	selected, drawn, result, rc := 0, 0, 1, 0
	var heading string
	var height, rowWidth, lw int
	var hidden, cut bool
	resize := func() {
		r, columns := termSize()
		height = max(1, r-5)
		// Rows the window cuts off are named by an "N more" line at each edge.
		// They come out of the item window, so the menu still fits the terminal.
		// Below eight rows there is no room for the edge lines, so the footer
		// carries the position instead (see counted): a cut list never stops
		// silently (#278).
		cut = len(rows) > height
		hidden = cut && r >= 8
		if hidden {
			height = max(1, r-7)
		}
		rowWidth = columns - len(uiMargin)
		lw = max(1, min(width, rowWidth-uiGap-1))
		heading = t.heading(prompt, columns)
	}
	resize()
	w := os.Stderr
	fmt.Fprint(w, "\033[?25l\033[?7l")
	func() {
		defer func() {
			restore()
			if drawn > 0 {
				fmt.Fprintf(w, "\033[%dA\r\033[J", drawn)
			}
			fmt.Fprint(w, t.reset+"\033[?7h\033[?25h")
		}()
		for {
			if drawn > 0 {
				fmt.Fprintf(w, "\033[%dA\r", drawn)
			}
			first := 0
			if selected >= height {
				first = selected - height + 1
			}
			last := min(first+height, len(rows))
			var b bytes.Buffer
			b.WriteString("\033[2K" + heading + "\n\033[2K\n")
			if hidden {
				b.WriteString(t.moreLine("↑", first) + "\n")
			}
			for i := first; i < last; i++ {
				ptr := " "
				if i == selected {
					ptr = t.focus + "❯" + t.reset
				}
				b.WriteString("\033[2K" + ptr + " " + t.label(visible(rows[i], display), lw, i == selected, rowWidth) + "\n")
			}
			if hidden {
				b.WriteString(t.moreLine("↓", len(rows)-last) + "\n")
			}
			foot := "↑/↓ Move · Home/End Jump · Enter Select · Esc Cancel"
			if cut && !hidden {
				foot = fmt.Sprintf("%d/%d · %s", selected+1, len(rows), foot)
			}
			b.WriteString("\033[2K\n\033[2K" + uiMargin + t.gray + foot + t.reset + "\n")
			w.Write(b.Bytes())
			drawn = last - first + 4
			if hidden {
				drawn += 2
			}
			c, s, ok := k.next(0, sigs, winch)
			if c == keyResize {
				resize()
				continue
			}
			if s != nil {
				rc = sigRC(s)
				return
			}
			if !ok {
				return
			}
			switch c {
			case '\r', '\n':
				result = 0
				return
			case 3:
				rc = 130
				return
			case 4:
				return
			case 27:
				n, _, ok := k.next(80*time.Millisecond, sigs, nil)
				if !ok || (n != '[' && n != 'O') {
					return
				}
				n, _, ok = k.next(80*time.Millisecond, sigs, nil)
				if !ok {
					return
				}
				// ↑/↓ stop at the ends as they always did; Home/End put either end
				// one key away (#278).
				switch n {
				case 'A':
					selected = max(selected-1, 0)
				case 'B':
					selected = min(selected+1, len(rows)-1)
				case 'H':
					selected = 0
				case 'F':
					selected = len(rows) - 1
				}
			}
		}
	}()
	if rc != 0 {
		return "", rc
	}
	if result != 0 {
		t.cancelled()
		return "", 1
	}
	t.receipt(prompt, rowID(visible(rows[selected], display)))
	return rowID(rows[selected]), 0
}

// moreLine is one edge of a native menu's window: how many rows sit beyond it.
// The line is written even when that side cuts nothing, so the menu keeps one
// shape while it scrolls.
func (t uiTheme) moreLine(arrow string, cut int) string {
	if cut < 1 {
		return "\033[2K"
	}
	return "\033[2K" + uiMargin + t.gray + arrow + " " + strconv.Itoa(cut) + " more" + t.reset
}

// receipt is _ccm_ui_receipt: Title › choice.
func (t uiTheme) receipt(title, choice string) {
	_, columns := termSize()
	fmt.Fprintln(os.Stderr, t.receiptLine(title, choice, columns))
}

// receiptTitleFloor is the least of the question a one-line receipt keeps: an
// answer that would squeeze its question below this wraps beneath the whole
// question instead (#361).
const receiptTitleFloor = 12

// receiptLine is the one line a picker leaves behind: Title › choice, fitted
// to COLUMNS. The choice is what the person picked, so it is never cut — the
// question yields space first, down to receiptTitleFloor. An answer that
// cannot share the line even then keeps its question whole: a choice that fits
// a line of its own breaks onto it, a longer one wraps beneath the question. No
// answer erases the question it answers (#361). A question ends in
// ": " and the receipt adds its own space before ›, so the title's does not
// double it.
func (t uiTheme) receiptLine(title, choice string, columns int) string {
	choice = noCntrl(choice)
	title = strings.TrimRight(title, " ")
	w := columns - cells(choice) - 3
	if w-len(uiMargin) < receiptTitleFloor {
		if len(uiMargin)+cells(choice) <= columns {
			// The choice fits a line of its own: the line breaks before it, so
			// no line outgrows the terminal and the choice is still whole.
			return t.heading(title, columns-2) + " " + t.focus + "›" + t.reset + "\n" + uiMargin + t.gray + choice + t.reset
		}
		w = columns
	}
	return t.heading(title, w) + " " + t.focus + "›" + t.reset + " " + t.gray + choice + t.reset
}

// fzfChrome is the lines an fzf window spends on itself besides the rows: the
// top and bottom border, the prompt, the info line and the header.
const fzfChrome = 5

// fzfArgs is the fzf command line for a version, a list of n rows and a
// terminal of lines rows. --cycle wraps the ends on every version; 0.36+ adds
// the scrollbar and the Home/End jump, 0.71+ lets the window grow to the list.
// An older fzf has neither, so ccmorph sizes its window itself: the whole list
// when the terminal allows it, otherwise the header says rows are cut. A list
// never stops silently at the window's edge (#278).
func fzfArgs(version string, n, lines int, prompt string, theme []string) []string {
	height := "45%"
	if fzfAtLeast(version, 71) {
		height = "~-1"
	}
	header := "↑/↓ Move · Type to filter · Enter Select · Esc Cancel"
	if fzfAtLeast(version, 36) {
		header = "↑/↓ Move · Home/End Jump · Type to filter · Enter Select · Esc Cancel"
	} else {
		room := max(1, lines-1)
		height = strconv.Itoa(min(n+fzfChrome, room))
		if n+fzfChrome > room {
			header = "More rows than fit here: ↑/↓ scroll · Type to filter · Enter Select · Esc Cancel"
		}
	}
	args := append([]string{"--height", height, "--reverse", "--border=rounded", "--pointer=❯", "--marker=✓",
		"--no-multi", "--no-print-query", "--ansi", "--cycle"}, theme...)
	if fzfAtLeast(version, 36) {
		args = append(args, "--scrollbar", "--bind", "home:first,end:last")
	}
	return append(args, "--delimiter", "\t", "--with-nth=2..", "--prompt", prompt+"> ", "--header", header)
}

// fzfVersion is the installed fzf's --version line, empty when it cannot run.
func fzfVersion(bin string) string {
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

var fzfVersionRE = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

// fzfAtLeast reports whether a --version line names MINOR of 0.MINOR or a later
// major. The scrollbar and the first/last jump binds arrived in 0.36, the
// adaptive ~-window in 0.71; an older or unreadable version keeps the menu
// working without them.
func fzfAtLeast(version string, minor int) bool {
	m := fzfVersionRE.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return major > 0 || min >= minor
}

// search is _ccm_ui_search: fzf when present, else the numbered list.
func (t uiTheme) search(prompt string, rows []string) (string, int) {
	if len(rows) == 0 {
		uiErr(t, "no available choices")
		return "", 1
	}
	fzf, ferr := exec.LookPath("fzf")
	if os.Getenv("TERM") != "dumb" && ferr == nil {
		width := labelWidth(rows, "labels")
		var rendered []string
		for _, r := range rows {
			rendered = append(rendered, rowID(r)+"\t"+t.label(visible(r, "labels"), width, false, 0))
		}
		theme := []string{"--color", fzfTheme}
		if os.Getenv("NO_COLOR") != "" {
			theme = []string{"--no-color"}
		}
		// A list longer than its window must be reachable end to end and say
		// that rows are hidden: --cycle wraps the ends together, and a new
		// enough fzf adds the scrollbar and the Home/End jump. Its window also
		// grows to the list itself, so a list that fits the terminal is never
		// cut at all (#278).
		lines, _ := termSize()
		args := fzfArgs(fzfVersion(fzf), len(rows), lines, prompt, theme)
		cmd := exec.Command(fzf, args...)
		cmd.Env = append(os.Environ(), "FZF_DEFAULT_OPTS=", "FZF_DEFAULT_OPTS_FILE=")
		cmd.Stdin = strings.NewReader(strings.Join(rendered, "\n") + "\n")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		choice := strings.TrimRight(string(out), "\n")
		if err != nil || choice == "" {
			// fzf answers Esc with exit 130 and no choice: a leave, like any picker.
			t.cancelled()
			return "", 1
		}
		if i := strings.IndexByte(choice, '\n'); i >= 0 {
			choice = choice[:i]
		}
		value := rowID(choice)
		for _, r := range rows {
			if rowID(r) == value {
				// The receipt names the row's own label, not the line fzf
				// printed back — that one carries the description again, and
				// it is the picker's job to have shown it (#361).
				t.receipt(prompt, rowID(visible(r, "labels")))
				return value, 0
			}
		}
		uiErr(t, "invalid selection")
		return "", 1
	}
	if isTTY(0) {
		return t.numbered(prompt, rows, "labels")
	}
	uiErr(t, "selection needs a terminal or fzf; pass the value directly")
	fmt.Fprintln(os.Stderr, strings.Join(rows, "\n"))
	// rc 2, not a cancel: no picker ran, so the caller needs the direct
	// command, not a Cancelled line.
	return "", 2
}

// uiInput is ccm_ui_input: one line, hidden when secret. rc 1 cancelled or EOF.
func uiInput(prompt string, secret bool) (string, int) {
	t := themeFor(2)
	w := os.Stderr
	if !isTTY(0) {
		fmt.Fprint(w, prompt)
		line, ok := readLine(os.Stdin)
		if secret {
			fmt.Fprintln(w)
		}
		if !ok && line == "" {
			return "", 1
		}
		return line, 0
	}
	// Signals and raw mode before the prompt: a key typed the moment it shows
	// must not reach a cooked terminal or kill the process unrestored.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	restore, err := rawTTY(os.Stdin)
	if err != nil {
		return "", 1
	}
	_, columns := termSize()
	fold := isTTY(2) && os.Getenv("TERM") != "dumb"
	if fold {
		// The hint is fitted like the heading, so the block is always two
		// lines plus the answer: the fold's erase counts what was printed.
		fmt.Fprint(w, t.heading(prompt, columns)+"\n"+uiMargin+t.gray+fit("Enter Submit · Esc Cancel", columns-len(uiMargin))+t.reset+"\n"+t.focus+"❯"+t.reset+" ")
	} else {
		fmt.Fprint(w, prompt+" (Enter Submit · Esc Cancel) ")
	}
	value, rc := readKeyLine(readKeys(), sigs, secret, w)
	restore()
	fmt.Fprintln(w)
	if fold {
		// An answered question leaves one line, like an answered picker: the
		// question with its answer — or without it, when the answer is secret
		// or empty. The hint and the typed line fold away (#361). A secret
		// echoed nothing, so its answer line is the ❯ alone.
		echoed := ""
		if !secret {
			echoed = value
		}
		eraseLines(2 + typedLines(cells(echoed), columns))
	}
	if rc != 0 {
		t.cancelled()
		return "", rc
	}
	if fold {
		if secret || value == "" {
			fmt.Fprintln(w, t.heading(prompt, columns))
		} else {
			t.receipt(prompt, value)
		}
	}
	return value, 0
}

// readLine reads one line byte by byte, so nothing after it is consumed.
func readLine(f *os.File) (string, bool) {
	var b []byte
	c := make([]byte, 1)
	for {
		n, err := f.Read(c)
		if n == 0 || err != nil {
			return string(b), false
		}
		if c[0] == '\n' {
			return string(b), true
		}
		b = append(b, c[0])
	}
}

// readKeyLine is _ccm_ui_readline: Esc cancels at once, ^U/^W/DEL edit. A
// cancel still returns what was typed, so the caller can erase the echoed
// line; callers ignore the value when rc is not 0.
func readKeyLine(k *keys, sigs <-chan os.Signal, secret bool, w *os.File) (string, int) {
	var value []byte
	erase := func(removed []byte) {
		if !secret {
			fmt.Fprint(w, strings.Repeat("\b \b", cells(string(removed))))
		}
	}
	for {
		c, s, ok := k.next(0, sigs, nil)
		if s != nil {
			return string(value), sigRC(s)
		}
		if !ok {
			return string(value), 1
		}
		switch {
		case c == '\r' || c == '\n':
			return string(value), 0
		case c == 3:
			return string(value), 130
		case c == 4:
			return string(value), 1
		case c == 27:
			n, _, ok := k.next(80*time.Millisecond, sigs, nil)
			if !ok || (n != '[' && n != 'O') {
				return string(value), 1
			}
			for {
				n, _, ok = k.next(80*time.Millisecond, sigs, nil)
				if !ok || unicode.IsLetter(rune(n)) || n == '~' {
					break
				}
			}
		case c == 127 || c == 8:
			if len(value) > 0 {
				_, n := utf8.DecodeLastRune(value)
				erase(value[len(value)-n:])
				value = value[:len(value)-n]
			}
		case c == 21:
			erase(value)
			value = nil
		case c == 23:
			rest := bytes.TrimRight(value, " \t")
			if i := bytes.LastIndexAny(rest, " \t"); i >= 0 {
				rest = rest[:i+1]
			} else {
				rest = nil
			}
			erase(value[len(rest):])
			value = append([]byte(nil), rest...)
		case c < 32:
		default:
			value = append(value, byte(c))
			if !secret {
				w.Write([]byte{byte(c)})
			}
		}
	}
}

// uiConfirm is ccm_ui_confirm: 0 yes, 1 no, 2 cancelled; defaults to No.
func uiConfirm(prompt string) int {
	var answer string
	if isTTY(0) && isTTY(2) && os.Getenv("TERM") != "dumb" {
		a, rc := uiSelect("native", prompt, "n\tNo\ny\tYes", "")
		if rc != 0 {
			return 2
		}
		answer = a
	} else {
		a, rc := uiInput(prompt+" [y/N] ", false)
		if rc != 0 {
			return 2
		}
		answer = a
	}
	if a := strings.ToLower(answer); a == "y" || a == "yes" {
		return 0
	}
	return 1
}

// uiVerb exposes the menus to the tests: select MODE TITLE ROWS [DISPLAY],
// numbered|list TITLE ROWS [DISPLAY], input PROMPT [secret], confirm PROMPT,
// rows|pick SLOT PROFILE, default SLOT PROFILE CURRENT. The answer goes to
// stdout, so it is never wired to a real credential prompt.
func (l *launcher) uiVerb(args []string) int {
	a := append(args, "", "", "", "", "")
	answer, rc := "", 0
	switch a[0] {
	case "select":
		answer, rc = uiSelect(a[1], a[2], a[3], a[4])
	case "numbered":
		answer, rc = themeFor(2).numbered(a[1], splitRows(a[2]), a[3])
	case "list":
		themeFor(2).list(a[1], splitRows(a[2]), a[3])
		return 0
	case "input":
		answer, rc = uiInput(a[1], a[2] == "secret")
	case "confirm":
		return uiConfirm(a[1])
	case "rows", "pick", "default":
		var ok bool
		switch a[0] {
		case "rows":
			answer, ok = l.valueRows(a[1], a[2])
		case "pick":
			answer, ok = l.pickValue(a[1], a[2])
		default:
			f, _ := l.readRaw(a[2])
			answer, ok = l.promptDefault(a[1], a[2], a[3], f["type"], nil)
		}
		if !ok {
			rc = 1
		}
	default:
		return 2
	}
	if rc == 0 {
		fmt.Println(answer)
	}
	return rc
}
