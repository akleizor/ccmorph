package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
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

// uiSelect is ccm_ui_select: the chosen ID, or rc 1 cancelled, 2 invalid, 128+n signal.
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
	for {
		choice, rc := uiInput(fmt.Sprintf("Select [1-%d] (q to cancel): ", len(rows)), false)
		if rc != 0 || choice == "q" || choice == "Q" {
			return "", 1
		}
		if n, err := strconv.Atoi(choice); err == nil && n >= 1 && n <= len(rows) && choice == strconv.Itoa(n) {
			return rowID(rows[n-1]), 0
		}
		fmt.Fprintln(os.Stderr, "Please choose one of the numbered options.")
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
		return "", 1
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
	resize := func() {
		r, columns := termSize()
		height = max(1, r-5)
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
			for i := first; i < last; i++ {
				ptr := " "
				if i == selected {
					ptr = t.focus + "❯" + t.reset
				}
				b.WriteString("\033[2K" + ptr + " " + t.label(visible(rows[i], display), lw, i == selected, rowWidth) + "\n")
			}
			b.WriteString("\033[2K\n\033[2K" + uiMargin + t.gray + "↑/↓ Move · Enter Select · Esc Cancel" + t.reset + "\n")
			w.Write(b.Bytes())
			drawn = last - first + 4
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
				if n == 'A' && selected > 0 {
					selected--
				} else if n == 'B' && selected < len(rows)-1 {
					selected++
				}
			}
		}
	}()
	if rc != 0 {
		return "", rc
	}
	if result != 0 {
		fmt.Fprintln(w, uiMargin+t.gray+"Cancelled"+t.reset)
		return "", 1
	}
	t.receipt(prompt, rowID(visible(rows[selected], display)))
	return rowID(rows[selected]), 0
}

// receipt is _ccm_ui_receipt: Title › choice.
func (t uiTheme) receipt(title, choice string) {
	_, columns := termSize()
	choice = fit(choice, columns/2)
	fmt.Fprintln(os.Stderr, t.heading(title, columns-cells(choice)-3)+" "+t.focus+"›"+t.reset+" "+t.gray+choice+t.reset)
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
		args := append([]string{"--height", "45%", "--reverse", "--border=rounded", "--pointer=❯", "--marker=✓",
			"--no-multi", "--no-print-query", "--ansi"}, theme...)
		args = append(args, "--delimiter", "\t", "--with-nth=2..", "--prompt", prompt+"> ",
			"--header", "↑/↓ Move · Type to filter · Enter Select · Esc Cancel")
		cmd := exec.Command(fzf, args...)
		cmd.Env = append(os.Environ(), "FZF_DEFAULT_OPTS=", "FZF_DEFAULT_OPTS_FILE=")
		cmd.Stdin = strings.NewReader(strings.Join(rendered, "\n") + "\n")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		choice := strings.TrimRight(string(out), "\n")
		if err != nil || choice == "" {
			return "", 1
		}
		if i := strings.IndexByte(choice, '\n'); i >= 0 {
			choice = choice[:i]
		}
		value := rowID(choice)
		for _, r := range rows {
			if rowID(r) == value {
				t.receipt(prompt, rowID(visible(choice, "labels")))
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
	return "", 1
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
	if isTTY(2) && os.Getenv("TERM") != "dumb" {
		_, columns := termSize()
		fmt.Fprint(w, t.heading(prompt, columns)+"\n"+uiMargin+t.gray+"Enter Submit · Esc Cancel"+t.reset+"\n"+t.focus+"❯"+t.reset+" ")
	} else {
		fmt.Fprint(w, prompt+" (Enter Submit · Esc Cancel) ")
	}
	value, rc := readKeyLine(readKeys(), sigs, secret, w)
	restore()
	fmt.Fprintln(w)
	if rc != 0 {
		fmt.Fprintln(w, uiMargin+t.gray+"Cancelled"+t.reset)
		return "", rc
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

// readKeyLine is _ccm_ui_readline: Esc cancels at once, ^U/^W/DEL edit.
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
			return "", sigRC(s)
		}
		if !ok {
			return "", 1
		}
		switch {
		case c == '\r' || c == '\n':
			return string(value), 0
		case c == 3:
			return "", 130
		case c == 4:
			return "", 1
		case c == 27:
			n, _, ok := k.next(80*time.Millisecond, sigs, nil)
			if !ok || (n != '[' && n != 'O') {
				return "", 1
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
