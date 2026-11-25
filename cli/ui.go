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

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/util"
)

// The terminal UI that lib/ccmorph-ui.zsh was: ROWS are ID<TAB>label[<TAB>description];
// `all` shows the ID as the label. Menus paint on stderr; only IDs go to stdout.

const uiMargin = "  "
const uiGap = 4

type uiTheme struct{ bold, dim, reset, accent, focus, gray, red string }

// palette is every colour ccmorph paints, in one place: each reads at 4:1 or
// better on a white and on a black terminal background, so a light theme no
// longer drops a kind word, a doctor mark or an error prefix to 1.6-3.2:1
// (#274, #636). TestPaletteReadsOnLightAndDarkTerminals pins the ratios.
var palette = struct{ accent, yellow, red, cyan, gray string }{
	accent: "c26242", yellow: "9b7608", red: "f3203e", cyan: "5a6cfb", gray: "7a7a7a",
}

var fzfTheme = "fg:-1,bg:-1,hl:#" + palette.cyan + ",fg+:#" + palette.cyan + ",bg+:-1,hl+:#" + palette.cyan +
	",pointer:#" + palette.cyan + ",marker:#" + palette.cyan + ",prompt:#" + palette.accent +
	",header:#" + palette.gray + ",info:#" + palette.gray + ",border:#" + palette.gray

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
	return uiTheme{"\033[1m", "\033[2m", "\033[0m", rgb(palette.accent), rgb(palette.cyan), rgb(palette.gray), rgb(palette.red)}
}

func uiErr(t uiTheme, msg string) { fmt.Fprintf(os.Stderr, "%sccmorph:%s %s\n", t.red, t.reset, msg) }

func noCntrl(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// fit is _ccm_ui_fit: cut to width cells with an ellipsis. The cut lands on a
// whole word when one fits before the width (#369), so a row never ends in
// half a word; a single long word still hard-cuts, and the space before the
// ellipsis stays.
func fit(s string, width int) string {
	s = noCntrl(s)
	if width < 1 {
		return ""
	}
	if ctxwindow.Cells(s) <= width {
		return s
	}
	n := ctxwindow.Cells(s)
	for n >= width {
		r, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
		n -= ctxwindow.Cells(string(r))
	}
	if i := strings.LastIndexByte(s, ' '); i > 0 {
		s = strings.TrimRight(s[:i], " ") + " "
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
		if c := ctxwindow.Cells(noCntrl(rowID(visible(r, display)))); c > w {
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
			pad = strings.Repeat(" ", max(0, width-ctxwindow.Cells(lab)+uiGap))
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

// list is ccm_ui_list: the numbered rows, painted on stderr. It returns the
// lines it printed, the count a fold over it erases.
func (t uiTheme) list(prompt string, rows []string, display string) int {
	_, columns := termSize()
	index := len(strconv.Itoa(len(rows)))
	rowWidth := columns - len(uiMargin) - index - 2
	width := min(labelWidth(rows, display), rowWidth)
	fmt.Fprintln(os.Stderr, t.heading(prompt, columns))
	fmt.Fprintln(os.Stderr)
	for i, r := range rows {
		fmt.Fprintf(os.Stderr, "%s%*d) %s\n", uiMargin, index, i+1, t.label(visible(r, display), width, false, rowWidth))
	}
	return len(rows) + 2 // heading, blank, rows
}

// numberWindow is how the numbered menu fits a screen h rows tall (#387):
// every row while the whole block fits — heading, blank, rows and the three
// prompt lines, plus the standing line a failed answer's message takes — else
// the first h-7, leaving room beside that chrome for the "N more" line and the
// line under it, so the title and the first rows stay on screen while the
// person chooses, between retries as at the first question. One row shows even
// on a screen too short for all the chrome: a window that showed nothing could
// not be answered.
func numberWindow(rows, h int, noted bool) (shown int, cut bool) {
	fit := 5
	if noted {
		fit = 6 // the message a failed answer leaves standing under the list
	}
	if rows+fit <= h {
		return rows, false
	}
	return max(1, h-7), true
}

// numberMode is where the numbered menu paints: a pipe takes data — every row,
// nothing under it; a dumb terminal is a screen that must not see cursor codes;
// a full terminal folds its answered rounds away (#361).
type numberMode int

const (
	numberPipe numberMode = iota
	numberDumb
	numberFold
)

// numberList paints the numbered menu's rows: no more than fit the screen, the
// rest named by a "N more" line (#387, #278), and — a long list being exactly
// where not knowing hurts — the one line under it saying fzf adds search and
// how to get it, the step this machine's own installer would name. A failed
// answer's NOTE takes that line's place, so a retry keeps the block's height
// and the title stays in view. It returns the lines it printed, the count the
// answer's fold erases.
func (t uiTheme) numberList(prompt string, rows []string, display string, mode numberMode, note string) int {
	shown := rows
	more := 0
	if mode != numberPipe {
		height, _ := termSize()
		if n, cut := numberWindow(len(rows), height, note != ""); cut {
			shown = rows[:n]
			more = 1
		}
	}
	lines := t.list(prompt, shown, display)
	if mode == numberPipe {
		return lines // data, not display: every row, no chrome
	}
	_, columns := termSize()
	extra := 0
	if more > 0 {
		if mode == numberFold {
			fmt.Fprintln(os.Stderr, t.moreLine("↓", len(rows)-len(shown), true))
		} else {
			// A dumb terminal sees no cursor codes: the same line, unarmed.
			fmt.Fprintln(os.Stderr, t.moreLine("↓", len(rows)-len(shown), false))
		}
		extra++
	}
	// The line under the list: the fzf hint, unless a failed answer left its
	// message, which takes the hint's place.
	below := ""
	if note != "" {
		below = note
	} else if more > 0 {
		below = "fzf adds search: " + util.FzfInstallStep()
	}
	if below != "" {
		fmt.Fprintln(os.Stderr, uiMargin+t.gray+fit(below, columns-len(uiMargin))+t.reset)
		extra++
	}
	return lines + extra
}

// numberPrompt is the numbered menu's question: a row number, or a typed name
// (#387) — the id, or any part of one — so a long list needs no counting. On a
// terminal uiInput adds Enter Submit · Esc Cancel, so the question names no
// second way out; q still cancels there (#636). A pipe sees only this line.
func numberPrompt(n int, terminal bool) string {
	if terminal {
		return fmt.Sprintf("Select [1-%d] or type a name: ", n)
	}
	return fmt.Sprintf("Select [1-%d] or type a name (q to cancel): ", n)
}

// numberMiss is what an answer that matches no row hears: both ways the menu
// takes, named together.
const numberMiss = "Please choose a number or part of a name."

// matchRows is the typed half of the numbered menu (#387): the rows whose id
// or visible text contains the fragment, case folded.
func matchRows(rows []string, fragment, display string) []string {
	needle := strings.ToLower(fragment)
	var out []string
	for _, r := range rows {
		if strings.Contains(strings.ToLower(rowID(r)), needle) ||
			strings.Contains(strings.ToLower(visible(r, display)), needle) {
			out = append(out, r)
		}
	}
	return out
}

func (t uiTheme) numbered(prompt string, rows []string, display string) (string, int) {
	if len(rows) == 0 {
		uiErr(t, "no available choices")
		return "", 1
	}
	// A dumb terminal is a screen too: the list must fit it and name the fzf
	// search it lacks — only the cursor codes are barred there (#387).
	mode := numberPipe
	if isTTY(2) {
		mode = numberDumb
		if os.Getenv("TERM") != "dumb" {
			mode = numberFold
		}
	}
	// Where keys can be read one at a time — a terminal on both streams —
	// the menu answers every key its prompt prints (#840); a pipe or a
	// stdin it cannot put in raw mode keeps the line loop below.
	if mode != numberPipe && isTTY(0) {
		if v, rc, ok := t.numberedKeys(prompt, rows, display, mode == numberDumb); ok {
			return v, rc
		}
		// raw mode refused this terminal before anything was printed: the
		// line loop answers it exactly as it always did.
	}
	lines := t.numberList(prompt, rows, display, mode, "")
	if mode != numberFold {
		// A pipe or a dumb terminal keeps the plain loop: no cursor codes, so
		// nothing folds — a dumb screen repaints the cut list on a miss instead.
		for {
			choice, rc := uiInput(numberPrompt(len(rows), isTTY(0)), false)
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
			if choice != "" {
				if matches := matchRows(rows, choice, display); len(matches) == 1 {
					return rowID(matches[0]), 0
				} else if len(matches) > 1 {
					rows = matches
					t.numberList(prompt, rows, display, mode, "")
					continue
				}
			}
			if mode == numberDumb {
				// A dumb terminal cannot erase the failed round, and a bare
				// message plus the next question push the title off a full
				// screen by the second miss: the cut list repaints under
				// them, the message in the hint's line, so the newest screen
				// opens on the title and the first rows again (#387).
				t.numberList(prompt, rows, display, mode, numberMiss)
				continue
			}
			fmt.Fprintln(os.Stderr, numberMiss)
		}
	}
	// On a terminal the answered list folds like every picker: one receipt
	// line names the choice, and the rows and the dead key hint do not stay
	// above it (#361). Each uiInput round has already folded itself to one
	// line, so one line per round is what adds to the list.
	for {
		choice, rc := uiInput(numberPrompt(len(rows), isTTY(0)), false)
		// uiInput folded its round to the line it left: a cancel leaves the one
		// Cancelled row, an answer the receipt — which is two rows when the
		// question and the answer do not share a line. The erase below counts
		// what is actually there (#449).
		if rc != 0 {
			lines++
		} else {
			_, columns := termSize()
			lines += t.receiptHeight(prompt, choice, columns)
		}
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
		if choice != "" {
			matches := matchRows(rows, choice, display)
			if len(matches) == 1 {
				eraseLines(lines)
				t.receipt(prompt, rowID(visible(matches[0], display)))
				return rowID(matches[0]), 0
			}
			if len(matches) > 1 {
				// Several rows carry the name: narrow the list to those and
				// ask again — the shorter question the answer made.
				eraseLines(lines)
				rows = matches
				lines = t.numberList(prompt, rows, display, numberFold, "")
				continue
			}
		}
		// The answer matched nothing. Leaving the message under the round would
		// grow the block past the screen on the second try, so the round folds
		// with the list and the message takes the hint's line under it: the
		// block keeps its height, and the title and the first rows stay in view
		// while the next question waits (#387).
		eraseLines(lines)
		lines = t.numberList(prompt, rows, display, numberFold, numberMiss)
	}
}

// numberKeysHint names every key the numbered menu answers, printed under the
// question: q, the arrows and j/k are promises the loop now keeps itself (#840).
const numberKeysHint = "Type to filter · j/k or ↑/↓ Move · Enter Select · q/Esc Cancel"

// numberedKeys is the numbered menu where keys can be read one at a time: the
// loop drives the same raw terminal the native menu and readKeyLine use, so a
// lone q or a bare Esc leaves once, arrows and j/k move a pointer down the
// list, typed letters edit into the same match the line loop matched (#387),
// and Enter takes what was typed — or what the pointer stands on. j/k move
// while nothing is typed, and the run is kept: a printable key after it, at
// any pace, makes it the first letters of a name, so a row whose own name
// starts with j or k stays typeable and no clock decides which (#840). And the
// pointer shows only while nothing is typed — while letters decide the answer,
// ❯ never stands on a row other than the one Enter submits (#840). The
// terminal is restored on every exit path. dumb names a terminal barred from
// cursor codes: nothing erases or refolds there, each round appends as plain
// text (#387). ok false means raw mode was refused before anything printed;
// the caller's line loop answers that terminal instead.
func (t uiTheme) numberedKeys(prompt string, rows []string, display string, dumb bool) (string, int, bool) {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	winch := make(chan os.Signal, 4)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	restore, err := rawTTY(os.Stdin)
	if err != nil {
		return "", 0, false
	}
	defer restore()
	k := readKeys()
	sel, start, note := -1, 0, ""
	var buf []byte
	// pending is the j/k run that moved the pointer while nothing was typed.
	// It is not echoed — the pointer shows what it did — but a printable key
	// after it, however late, makes it the first letters of a name (#840).
	var pending []byte
	// blockLines and echoLines count what is on screen: the list block (with
	// its "N more" and hint lines) and the question above the next key. A
	// fold terminal erases exactly that before every redraw.
	blockLines, echoLines := 0, 0
	painted, echoOpen := false, false
	// recount prices the echoed question in screen rows: its heading and
	// hint, plus the rows the ❯ line takes as the answer wraps (#361).
	recount := func() {
		_, columns := termSize()
		if dumb {
			echoLines = ctxwindow.Cells(numberPrompt(len(rows), false)+" ("+numberKeysHint+") ")/max(1, columns) + 1
			return
		}
		echoLines = 2 + typedLines(ctxwindow.Cells(string(buf)), columns)
	}
	// closeEcho ends the echoed question line before any fold or new round,
	// as every answered question did under uiInput.
	closeEcho := func() {
		if echoOpen {
			fmt.Fprintln(os.Stderr)
			echoOpen = false
		}
	}
	fold := func() {
		if painted && !dumb {
			eraseLines(blockLines + echoLines)
		}
	}
	// paint draws the whole round: heading, the windowed rows with the
	// pointer on the selected one, the one line under the list (a failed
	// answer's message, else the fzf hint when rows are cut — #387), and the
	// question with whatever has been typed.
	paint := func() {
		closeEcho()
		fold()
		painted = true
		height, columns := termSize()
		shown, cut := numberWindow(len(rows), height, note != "")
		if !cut {
			start = 0
		} else {
			// The window keeps the pointer in view by the shortest slide;
			// numbers stay global, so an answer names the row it always did.
			if sel >= 0 {
				if sel < start {
					start = sel
				}
				if sel >= start+shown {
					start = sel - shown + 1
				}
			}
			start = min(max(start, 0), max(len(rows)-shown, 0))
		}
		window := rows
		if cut {
			window = rows[start : start+shown]
		}
		// The number column is as wide as the largest number shown, exactly
		// as the whole-list window always padded it.
		index := len(strconv.Itoa(start + len(window)))
		rowWidth := columns - len(uiMargin) - index - 2
		width := min(labelWidth(window, display), rowWidth)
		fmt.Fprintln(os.Stderr, t.heading(prompt, columns))
		fmt.Fprintln(os.Stderr)
		for i, r := range window {
			n := start + i
			// The pointer shows only while nothing is typed: from the first
			// letter on, Enter submits the name, and a ❯ standing elsewhere
			// would promise an answer the key would not give (#840).
			focused := sel >= 0 && len(buf) == 0 && start+i == sel
			ptr := uiMargin
			if focused {
				ptr = "❯ "
			}
			fmt.Fprintf(os.Stderr, "%s%*d) %s\n", ptr, index, n+1, t.label(visible(r, display), width, focused, rowWidth))
		}
		blockLines = len(window) + 2
		if cut {
			above, below := start, len(rows)-start-shown
			more := fmt.Sprintf("↓ %d more", below)
			switch {
			case above > 0 && below > 0:
				more = fmt.Sprintf("↑ %d · ↓ %d more", above, below)
			case above > 0:
				more = fmt.Sprintf("↑ %d more", above)
			}
			if dumb {
				fmt.Fprintf(os.Stderr, "%s%s\n", uiMargin, more)
			} else {
				fmt.Fprintln(os.Stderr, uiMargin+t.gray+more+t.reset)
			}
			blockLines++
		}
		below := note
		if below == "" && cut {
			below = "fzf adds search: " + util.FzfInstallStep()
		}
		if below != "" {
			fmt.Fprintln(os.Stderr, uiMargin+t.gray+fit(below, columns-len(uiMargin))+t.reset)
			blockLines++
		}
		// The question names the list as numberPrompt always did: the rows a
		// narrowed round leaves renumber it ("the shorter question the answer
		// made"). The block above keeps the menu's own title.
		question := numberPrompt(len(rows), false)
		if dumb {
			fmt.Fprint(os.Stderr, question+" ("+numberKeysHint+") ")
		} else {
			fmt.Fprint(os.Stderr, t.heading(question, columns)+"\n"+uiMargin+t.gray+fit(numberKeysHint, columns-len(uiMargin))+t.reset+"\n"+t.focus+"❯"+t.reset+" ")
			if len(buf) > 0 {
				fmt.Fprint(os.Stderr, string(buf))
			}
		}
		echoOpen = true
		recount()
	}
	// priced re-prices the echoed question after an edit: back to an empty
	// buffer, a pointer a move left over is the selection again, and only a
	// repaint brings it back (#840).
	priced := func() {
		if sel >= 0 && len(buf) == 0 {
			paint()
		} else {
			recount()
		}
	}
	// leave and pick are the loop's only endings, so every terminal — fold
	// or dumb — answers a leave with exactly one Cancelled line (#840) and
	// an answer with the one receipt line every picker leaves (#361).
	leave := func() (string, int, bool) {
		closeEcho()
		fold()
		t.cancelled()
		return "", 1, true
	}
	pick := func(r string) (string, int, bool) {
		closeEcho()
		fold()
		if !dumb {
			t.receipt(prompt, rowID(visible(r, display)))
		}
		return rowID(r), 0, true
	}

	paint()
	for {
		c, s, ok := k.next(0, sigs, winch)
		if c == keyResize {
			paint()
			continue
		}
		if s != nil || !ok {
			// A signal or a closed terminal leaves as Esc does: one Cancelled
			// line and status 1, the answer the line loop gave.
			return leave()
		}
		// Only another j/k extends the run and only a printable key takes it
		// into a name; any other key ends it (#840).
		run := pending
		pending = nil
		switch {
		case c == '\r' || c == '\n':
			if len(buf) == 0 {
				if sel >= 0 {
					return pick(rows[sel])
				}
				note = numberMiss
				paint()
				continue
			}
			answer := string(buf)
			buf = nil
			if n, err := strconv.Atoi(answer); err == nil && n >= 1 && n <= len(rows) && answer == strconv.Itoa(n) {
				return pick(rows[n-1])
			}
			matches := matchRows(rows, answer, display)
			switch {
			case len(matches) == 1:
				return pick(matches[0])
			case len(matches) > 1:
				// Several rows carry the name: narrow and renumber (#387).
				rows, sel, start, note = matches, -1, 0, ""
				paint()
			default:
				note = numberMiss
				paint()
			}
		case c == 3 || c == 4:
			return leave()
		case c == 27:
			// A bare Esc is the leave; an arrow, Home or End moves the
			// pointer — every terminal's spelling, as the native menu reads
			// them (#278, #388).
			n, s, ok := k.next(80*time.Millisecond, sigs, nil)
			if s != nil || !ok || (n != '[' && n != 'O') {
				return leave()
			}
			tail, done, s := k.seqTail(sigs)
			if s != nil {
				return leave()
			}
			if !done && tail == "" {
				return leave()
			}
			if len(buf) > 0 {
				// The typed name decides what Enter submits; moving now would
				// only paint a second selection the key will not take (#840).
				continue
			}
			switch tail {
			case "A":
				sel = max(sel-1, 0)
			case "B":
				sel = min(sel+1, len(rows)-1)
			case "H", "1~", "7~":
				sel = 0
			case "F", "4~", "8~":
				sel = len(rows) - 1
			default:
				continue
			}
			paint()
		case len(buf) == 0 && (c == 'q' || c == 'Q'):
			// The leave the prompt prints. Alone it leaves at once (#840);
			// after other letters it is ordinary input, so a name carrying
			// one stays typeable.
			return leave()
		case len(buf) == 0 && (c == 'j' || c == 'k'):
			// j/k move while nothing is typed, and are kept: j2 is a model,
			// not a move and a row number, however long the hand pauses
			// between the two keys (#840).
			pending = append(run, byte(c))
			if c == 'j' {
				sel = min(sel+1, len(rows)-1)
			} else {
				sel = max(sel-1, 0)
			}
			paint()
		case c == 127 || c == 8:
			if len(buf) > 0 {
				_, n := utf8.DecodeLastRune(buf)
				fmt.Fprint(os.Stderr, strings.Repeat("\b \b", ctxwindow.Cells(string(buf[len(buf)-n:]))))
				buf = buf[:len(buf)-n]
				priced()
			}
		case c == 21:
			fmt.Fprint(os.Stderr, strings.Repeat("\b \b", ctxwindow.Cells(string(buf))))
			buf = nil
			priced()
		case c == 23:
			full := string(buf)
			rest := strings.TrimRight(full, " \t")
			if i := strings.LastIndexAny(rest, " \t"); i >= 0 {
				rest = rest[:i+1]
			} else {
				rest = ""
			}
			fmt.Fprint(os.Stderr, strings.Repeat("\b \b", ctxwindow.Cells(full)-ctxwindow.Cells(rest)))
			buf = []byte(rest)
			priced()
		case c < 32:
		default:
			if len(buf) == 0 && len(run) > 0 {
				// The j/k run was a name's first letters: the name takes them
				// back, and the repaint folds the pointer away so the screen
				// shows only what Enter submits (#840).
				buf = append(append([]byte{}, run...), byte(c))
				paint()
				break
			}
			typing := len(buf) == 0
			buf = append(buf, byte(c))
			if typing && sel >= 0 {
				// The first letter after a move: repaint folds the pointer
				// away, so the screen Enter answers shows only the name
				// it will submit (#840).
				paint()
			} else {
				fmt.Fprint(os.Stderr, string(byte(c)))
				recount()
			}
		}
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
// in is the terminal; a test hands it a reader of its own.
type keys struct{ in io.Reader }

func readKeys() *keys { return &keys{in: os.Stdin} }

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
		n, err := k.in.Read(b)
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

// seqTail reads the rest of an escape sequence whose [ or O introducer was
// just consumed: the bytes through its final one. Xterm's arrows, Home and
// End end in a letter; the Home and End tmux, screen and rxvt send end in
// digits and ~ (#388). done false is a sequence that never finished inside
// Esc's own 80ms window — an empty tail is then the leave a bare Esc asks.
// A termination signal answers s: the sequence's reader hands it back, so
// the menu ends with the signal's own exit status instead of waiting out a
// key its sender already interrupted.
func (k *keys) seqTail(stop <-chan os.Signal) (tail string, done bool, s os.Signal) {
	var b []byte
	for len(b) < 16 {
		n, sig, ok := k.next(80*time.Millisecond, stop, nil)
		if !ok {
			return string(b), false, sig
		}
		b = append(b, byte(n))
		if n == '~' || unicode.IsLetter(rune(n)) {
			return string(b), true, nil
		}
	}
	return string(b), false, nil
}

// native is _ccm_ui_native: an inline ↑/↓ menu on the terminal.
func (t uiTheme) native(prompt string, rows []string, display string) (string, int) {
	if len(rows) == 0 {
		uiErr(t, "no available choices")
		return "", 1
	}
	if !isTTY(0) || !isTTY(2) {
		// Real values first, and only real values — the menus' @-records are
		// not values the direct command takes — so the refusal ends at its
		// help line (#840).
		if rows := publicRows(rows); len(rows) > 0 {
			fmt.Fprintln(os.Stderr, strings.Join(rows, "\n"))
		}
		uiErr(t, "selection needs a terminal; pass the value directly")
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
				b.WriteString(t.moreLine("↑", first, true) + "\n")
			}
			for i := first; i < last; i++ {
				ptr := " "
				if i == selected {
					ptr = t.focus + "❯" + t.reset
				}
				b.WriteString("\033[2K" + ptr + " " + t.label(visible(rows[i], display), lw, i == selected, rowWidth) + "\n")
			}
			if hidden {
				b.WriteString(t.moreLine("↓", len(rows)-last, true) + "\n")
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
				// A signal read inside the sequence's windows is not part of
				// the key: it ends the menu with its own exit status, the
				// same answer the wait at the top of the loop would give.
				n, s, ok := k.next(80*time.Millisecond, sigs, nil)
				if s != nil {
					rc = sigRC(s)
					return
				}
				if !ok || (n != '[' && n != 'O') {
					return
				}
				tail, done, s := k.seqTail(sigs)
				if s != nil {
					rc = sigRC(s)
					return
				}
				if !done && tail == "" {
					return
				}
				// ↑/↓ stop at the ends as they always did; Home/End put either
				// end one key away (#278), in every terminal's spelling of
				// them: xterm and application mode end the key with a letter,
				// tmux, screen and rxvt with digits and ~ (#388).
				switch tail {
				case "A":
					selected = max(selected-1, 0)
				case "B":
					selected = min(selected+1, len(rows)-1)
				case "H", "1~", "7~":
					selected = 0
				case "F", "4~", "8~":
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
func (t uiTheme) moreLine(arrow string, cut int, armed bool) string {
	if cut < 1 {
		return "\033[2K"
	}
	words := arrow + " " + strconv.Itoa(cut) + " more"
	if !armed {
		// A dumb terminal has no cursor codes to clear with: the same line,
		// plain (#636).
		return uiMargin + words
	}
	return "\033[2K" + uiMargin + t.gray + words + t.reset
}

// receipt is _ccm_ui_receipt: Title › choice.
func (t uiTheme) receipt(title, choice string) {
	_, columns := termSize()
	fmt.Fprintln(os.Stderr, t.receiptLine(title, choice, columns))
}

// receiptHeight is the rows an answered question leaves: the one receipt line,
// or the two it folds to when the question and the choice cannot share a line.
// An empty answer leaves the question's heading alone: one row. The numbered
// menu erases a round before it redraws, and a two-row receipt counted as one
// leaves the list's heading above the redrawn block (#449).
func (t uiTheme) receiptHeight(title, choice string, columns int) int {
	if choice == "" {
		return 1
	}
	rows := 0
	for _, line := range strings.Split(t.receiptLine(title, choice, columns), "\n") {
		rows += max(1, (ctxwindow.Cells(line)+columns-1)/columns)
	}
	return max(1, rows)
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
	w := columns - ctxwindow.Cells(choice) - 3
	if w-len(uiMargin) < receiptTitleFloor {
		if len(uiMargin)+ctxwindow.Cells(choice) <= columns {
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
func fzfArgs(version string, n, lines, columns int, prompt string, theme []string) []string {
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
	// A terminal too narrow for the whole header keeps the two keys that end
	// the picker: fzf cut the line from the right, so at 56 columns Esc Cancel
	// went first (#636). Four columns are fzf's border and padding.
	if columns > 0 && ctxwindow.Cells(header)+4 > columns {
		if strings.HasPrefix(header, "More rows") {
			header = "More rows than fit · Esc Cancel"
		} else {
			header = "Enter Select · Esc Cancel"
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

// ttyUsable reports whether a terminal answers for this process: /dev/tty
// opens exactly when one is there — for a foreground call with piped stdin
// as for stdin itself being the terminal, the two states where a person
// could press keys and fzf has always drawn and read them (#725). A script,
// pipe or CI job with no controlling terminal gets an error here. The open
// never reads or writes, so it cannot steal input or block a caller.
func ttyUsable() bool {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false
	}
	tty.Close()
	return true
}

// pickerHasTerminal reports whether this process has a terminal that can
// carry the picker: stdin's own, or the controlling one. It is a variable so
// the tests of the person's own paths — the answer's receipt, fzf's Esc
// cancel — can run in a `go test` process, which has no terminal of its own.
// The no-terminal refusal is tested against the real probe, in a session with
// no controlling terminal at all (#725).
var pickerHasTerminal = func() bool { return isTTY(0) || ttyUsable() }

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
		lines, columns := termSize()
		args := fzfArgs(fzfVersion(fzf), len(rows), lines, columns, prompt, theme)
		cmd := exec.Command(fzf, args...)
		cmd.Env = append(os.Environ(), "FZF_DEFAULT_OPTS=", "FZF_DEFAULT_OPTS_FILE=")
		cmd.Stdin = strings.NewReader(strings.Join(rendered, "\n") + "\n")
		if !pickerHasTerminal() {
			// No terminal answers this call (GH-725): a script, a pipe or a CI
			// job. Real fzf would draw nowhere and wait for keys no script can
			// send, so it is not launched at all, and an answer produced
			// without a terminal is not a person's choice — none is taken.
			// The missing terminal is named instead, with the rows and the
			// direct command; "Cancelled" must never stand in for a hang.
			t.refuseNoTerminal(rows, true)
			return "", 2
		}
		// A terminal answers this call — stdin's own or the controlling one —
		// so the person's picker runs exactly as ever, bound by no clock: a
		// key may take as long as the hand reading the list.
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
	// Neither stdin nor /dev/tty answers this call, and there is no fzf that
	// could answer without one (or a dumb terminal that forbids it).
	t.refuseNoTerminal(rows, false)
	// rc 2, not a cancel: no picker ran, so the caller needs the direct
	// command, not a Cancelled line.
	return "", 2
}

// publicRows drops the menus' internal records — @keep, @custom and any other
// @-prefixed sentinel — from a list being printed for someone who cannot
// pick. Those IDs are the commands' own bookkeeping, never values a direct
// command takes, and after a named refusal they read as a dump of internals
// (#840).
func publicRows(rows []string) []string {
	var out []string
	for _, r := range rows {
		if !strings.HasPrefix(rowID(r), "@") {
			out = append(out, r)
		}
	}
	return out
}

// refuseNoTerminal prints what there was to choose from and then the named
// cause, so every no-terminal refusal reads the same and ends on the one line
// to act on — a help line with a listing under it reads as an unfinished
// sentence (#840). fzfNamed tells a caller who already has fzf that what is
// missing is the terminal it cannot reach — naming the tool would send them
// after what they hold (#725). The menus' @-records stay out of the listing:
// they are bookkeeping, not values the direct command takes (#840).
func (t uiTheme) refuseNoTerminal(rows []string, fzfNamed bool) {
	if rows := publicRows(rows); len(rows) > 0 {
		fmt.Fprintln(os.Stderr, strings.Join(rows, "\n"))
	}
	if fzfNamed {
		uiErr(t, "selection needs a terminal; pass the value directly")
	} else {
		uiErr(t, "selection needs a terminal or fzf; pass the value directly")
	}
}

// uiInput is ccm_ui_input: one line, hidden when secret. rc 1 cancelled or EOF.
func uiInput(prompt string, secret bool) (string, int) {
	t := themeFor(2)
	w := os.Stderr
	if !isTTY(0) {
		fmt.Fprint(w, prompt)
		line, ok := readLine(os.Stdin)
		// A piped answer is not echoed, so each prompt ends its own line:
		// otherwise the next prompt or note ran on after this one (#636).
		fmt.Fprintln(w)
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
		eraseLines(2 + typedLines(ctxwindow.Cells(echoed), columns))
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
			fmt.Fprint(w, strings.Repeat("\b \b", ctxwindow.Cells(string(removed))))
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
			answer, ok = l.promptDefault(a[1], a[2], a[3], f["type"], f["base_url"], nil)
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
