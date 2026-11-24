package cli

import (
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// One palette per terminal background (#274). The dark set is the fixed Claude
// Code roles ccmorph has always painted with; the light set is those roles
// darkened to at least 4.5:1 on white, so a pale terminal keeps every
// distinction the dark one has. dim is "" (SGR 2) on dark terminals, where
// every terminal dims against its own background, and a fixed grey on light
// ones, where a terminal is free to render SGR 2 below any readable contrast.
type palette struct {
	accent, focus, yellow, red, gray, dim, border string
}

var darkPalette = palette{accent: "d77757", focus: "b1b9f9", yellow: "ffc107",
	red: "ff6b80", gray: "999999", dim: "", border: "888888"}

var lightPalette = palette{accent: "a0481d", focus: "4453b0", yellow: "8a6100",
	red: "b02550", gray: "666666", dim: "6b6b6b", border: "767676"}

// dimEscape is how dim text is painted under this palette.
func (p palette) dimEscape() string {
	if p.dim == "" {
		return "\033[2m"
	}
	return rgb(p.dim)
}

// hex is one role's colour, by the name the doctor marks use.
func (p palette) hex(role string) string {
	switch role {
	case "accent":
		return p.accent
	case "focus":
		return p.focus
	case "yellow":
		return p.yellow
	case "red":
		return p.red
	default:
		return p.gray
	}
}

// fzfSpec is the --color string for fzf: fg and bg stay the terminal's own
// (-1), so the parts fzf paints plain stay readable on either background.
func (p palette) fzfSpec() string {
	return "fg:-1,bg:-1,hl:#" + p.focus + ",fg+:#" + p.focus + ",bg+:-1,hl+:#" + p.focus +
		",pointer:#" + p.focus + ",marker:#" + p.focus + ",prompt:#" + p.accent +
		",header:#" + p.gray + ",info:#" + p.gray + ",border:#" + p.border
}

// backgroundQueryTimeout bounds the wait for a terminal's answer: a replying
// terminal answers within a few milliseconds, and one that never answers must
// not tax every command for long.
const backgroundQueryTimeout = 100 * time.Millisecond

// backgroundQueryMax bounds how much input one probe will hold while it waits.
const backgroundQueryMax = 256

var (
	lightOnce  sync.Once
	lightKnown bool
)

// lightTerminal reports whether the terminal behind out paints on a light
// background, so its colours can be darkened to stay readable (#274). Decided
// once per process: CCMORPH_THEME forces it, else the terminal's own answer to
// the background query, else COLORFGBG, else dark — the palette a terminal
// that says nothing has always gotten.
func lightTerminal(out *os.File) bool {
	lightOnce.Do(func() {
		lightKnown = decideLight(os.Getenv("CCMORPH_THEME"), os.Getenv("COLORFGBG"),
			backgroundLight, int(out.Fd()))
	})
	return lightKnown
}

// themePalette is the palette for one output stream.
func themePalette(out *os.File) palette {
	if lightTerminal(out) {
		return lightPalette
	}
	return darkPalette
}

// decideLight is the order the palette is picked in, apart from any terminal:
// the forced theme wins, then the terminal's own answer, then COLORFGBG.
func decideLight(theme, fgbg string, ask func(fd int) (light, ok bool), fd int) bool {
	switch strings.ToLower(strings.TrimSpace(theme)) {
	case "light":
		return true
	case "dark":
		return false
	}
	if light, ok := ask(fd); ok {
		return light
	}
	light, ok := fgbgLight(fgbg)
	return ok && light
}

// backgroundLight asks the terminal behind fd for its background colour and
// reports whether that colour is light. It asks only when stdin is this
// process's foreground terminal, in raw mode so the reply is never echoed, and
// only through queryBackground, which owns the reply and nothing else.
//
// Every key the probe read goes back on the input queue after the terminal
// mode is restored, so it is echoed and acted on (a ^C interrupts, a ^Z
// stops) exactly as if it had been typed then. A signal that arrives meanwhile
// restores the mode first and then takes its default action, as it would have
// without the probe.
func backgroundLight(fd int) (light, ok bool) {
	if !isTTY(0) || !isTTY(fd) || !foregroundTTY(0) || inputPending(0) != 0 {
		return false, false
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	restore, err := rawTTY(os.Stdin)
	if err != nil {
		signal.Stop(sigs)
		return false, false
	}
	out := os.Stdout
	if fd == 2 {
		out = os.Stderr
	}
	probe := &termProbe{out: out, sigs: sigs}
	res := queryBackground(probe, backgroundQueryTimeout)
	restore()
	// The keys go back in the restored mode, so they echo and act as typed
	// keys do. The marker already went back through this ioctl, and TIOCSTI
	// refuses a caller, never a byte, so these go back too. sigs still
	// listens: a ^C among them flushes what came before it, as a typed one
	// would, and is caught below once every later key is back.
	_ = pushInput(0, res.giveBack)
	signal.Stop(sigs)
	if probe.sig == nil {
		select {
		case s := <-sigs:
			probe.sig = s
		default:
		}
	}
	if probe.sig != nil {
		die(restore, probe.sig)
	}
	return res.light, res.ok
}

// termProbe is the real terminal behind queryBackground: stdin, in raw mode,
// and the stream the question goes out on.
type termProbe struct {
	out  *os.File
	sigs <-chan os.Signal
	sig  os.Signal // the first signal that arrived while the probe ran
}

func (t *termProbe) Pending() int        { return inputPending(0) }
func (t *termProbe) Push(b []byte) error { return pushInput(0, b) }

func (t *termProbe) Ask() error {
	_, err := t.out.Write([]byte("\033]11;?\033\\"))
	return err
}

// Next reads one byte; rawTTY makes a silent read return within 0.1s. The
// first signal ends the wait; after it, Next only reads.
func (t *termProbe) Next() (byte, bool) {
	if t.sig == nil {
		select {
		case s := <-t.sigs:
			t.sig = s
			return 0, false
		default:
		}
	}
	b := make([]byte, 1)
	n, _ := os.Stdin.Read(b)
	return b[0], n == 1
}

// probeMarker is the byte the probe puts on the terminal's input queue before
// it asks: reading it back means everything typed before the question is out.
const probeMarker = 0x1f

// probeDrainMax bounds how many queued bytes the probe moves behind the ones it
// gives back; no terminal input queue holds more.
const probeDrainMax = 1 << 16

// probeTTY is the terminal as the background probe sees it.
type probeTTY interface {
	Pending() int        // bytes waiting on the input queue; -1 when unknown
	Push(b []byte) error // put b on the input queue as if typed
	Ask() error          // send the OSC 11 question
	Next() (byte, bool)  // the next input byte; false on silence, EOF or a signal
}

// probeResult is what one probe learned and what it owes the input queue.
type probeResult struct {
	light, ok bool
	giveBack  []byte
}

// queryBackground asks t for its background colour and owns only the reply.
// It reads nothing it could not give back: it gives up while keys already
// wait, or when the queue refuses one byte. Before it asks it pushes
// probeMarker and reads up to it, so keys typed earlier stay first. Then it
// reads the reply and, last, everything still waiting behind it. giveBack is
// every byte read except the reply and the marker, in the order typed: one
// push of it, after the mode is restored, rebuilds the queue as typed.
func queryBackground(t probeTTY, timeout time.Duration) (res probeResult) {
	if t.Pending() != 0 || t.Push([]byte{probeMarker}) != nil {
		return res
	}
	deadline := time.Now().Add(timeout)
	var pre, got []byte
	marked, done := false, false
	for !marked && len(pre) < backgroundQueryMax && time.Now().Before(deadline) {
		c, ok := t.Next()
		if !ok {
			break
		}
		if c == probeMarker {
			marked = true
		} else {
			pre = append(pre, c)
		}
	}
	if marked && t.Ask() == nil {
		for !done && len(got) < backgroundQueryMax && time.Now().Before(deadline) {
			c, ok := t.Next()
			if !ok {
				break
			}
			got = append(got, c)
			if rest, r, g, b, complete, found := splitBackgroundReply(got); complete {
				got, done, res.ok, res.light = rest, true, found, found && lightBackground(r, g, b)
			}
		}
	}
	if !done {
		got = withoutPartialReply(got)
	}
	back := append(pre, got...)
	for n, misses := 0, 0; n < probeDrainMax && misses < 2 && t.Pending() > 0; n++ {
		c, ok := t.Next()
		if !ok {
			misses++ // a signal: the keys behind it still go back in order
			continue
		}
		if c == probeMarker && !marked {
			marked = true
			continue
		}
		back = append(back, c)
	}
	res.giveBack = back
	return res
}

// die puts the terminal back and dies by the signal, as the command would have
// without the probe.
func die(restore func(), s os.Signal) {
	restore()
	signal.Reset(s)
	if sig, ok := s.(syscall.Signal); ok {
		syscall.Kill(os.Getpid(), sig)
	}
	os.Exit(1)
}

// splitBackgroundReply finds a complete OSC 11 reply (BEL or ST terminated) in
// what the probe has read. done is whether one is there; found whether its
// colour parsed; rest is everything read that is not the reply.
func splitBackgroundReply(got []byte) (rest []byte, r, g, b int, done, found bool) {
	s := string(got)
	i := strings.Index(s, "\x1b]11;")
	if i < 0 {
		return got, 0, 0, 0, false, false
	}
	body := s[i+5:]
	k := strings.IndexAny(body, "\x07\x1b")
	if k < 0 {
		return got, 0, 0, 0, false, false
	}
	tl := 1 // BEL, or an ESC that is not an ST: the reply ends there
	if body[k] == '\x1b' {
		if k+1 == len(body) {
			return got, 0, 0, 0, false, false // an ESC that may yet be an ST
		}
		if body[k+1] == '\\' {
			tl = 2
		} else {
			tl = 0
		}
	}
	end := i + 5 + k + tl
	r, g, b, found = parseBackgroundReply(s[i : i+5+k+1])
	rest = append(append([]byte{}, got[:i]...), got[end:]...)
	return rest, r, g, b, true, found
}

// withoutPartialReply is what a probe that heard no complete reply gives back:
// everything read except an unfinished answer, which would only print as junk.
func withoutPartialReply(got []byte) []byte {
	s := string(got)
	if i := strings.Index(s, "\x1b]11;"); i >= 0 {
		return got[:i]
	}
	for _, p := range []string{"\x1b]11", "\x1b]1", "\x1b]"} {
		if strings.HasSuffix(s, p) {
			return got[:len(got)-len(p)]
		}
	}
	return got
}

// parseBackgroundReply reads a terminal's answer to the background query: OSC
// 11 with 1-4 hex digits per channel, BEL or ST terminated, possibly after
// another reply the terminal sent first. ok is false when no complete colour
// is there.
func parseBackgroundReply(reply string) (r, g, b int, ok bool) {
	i := strings.Index(reply, "\x1b]11;")
	if i < 0 || !strings.HasPrefix(reply[i+5:], "rgb:") {
		return 0, 0, 0, false
	}
	rest := reply[i+9:]
	end := strings.IndexAny(rest, "\x07\x1b")
	if end < 0 {
		return 0, 0, 0, false
	}
	parts := strings.Split(rest[:end], "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var v [3]int
	for j, p := range parts {
		if len(p) < 1 || len(p) > 4 {
			return 0, 0, 0, false
		}
		n, err := strconv.ParseUint(p, 16, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		max := uint64(1)<<(4*len(p)) - 1
		v[j] = int(math.Round(float64(n) * 255 / float64(max)))
	}
	return v[0], v[1], v[2], true
}

// lightBackground is whether an 8-bit background colour is light: the popular
// light themes sit above 0.9 relative luminance and the dark ones below 0.2,
// so one half is the empty middle where neither palette would be readable.
func lightBackground(r, g, b int) bool {
	return relativeLuminance(r, g, b) >= 0.5
}

// relativeLuminance is WCAG 2.x's L for one 8-bit colour.
func relativeLuminance(r, g, b int) float64 {
	f := func(c int) float64 {
		v := float64(c) / 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*f(r) + 0.7152*f(g) + 0.0722*f(b)
}

// contrastRatio is WCAG 2.x's contrast between two hex colours: the ratio both
// palettes are held to (#274).
func contrastRatio(a, b string) float64 {
	channels := func(hex string) (int, int, int) {
		r, _ := strconv.ParseUint(hex[0:2], 16, 8)
		g, _ := strconv.ParseUint(hex[2:4], 16, 8)
		b, _ := strconv.ParseUint(hex[4:6], 16, 8)
		return int(r), int(g), int(b)
	}
	ar, ag, ab := channels(a)
	br, bg, bb := channels(b)
	la, lb := relativeLuminance(ar, ag, ab), relativeLuminance(br, bg, bb)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// fgbgLight reads COLORFGBG ("fg;bg" or "fg;default;bg"): the last field is
// the background — dark as palette colour 0-6 or 8, light as 7 or 9-15.
// Anything else, "default" or a name or nothing, is no answer.
func fgbgLight(fgbg string) (light, ok bool) {
	fields := strings.Split(fgbg, ";")
	n, err := strconv.Atoi(fields[len(fields)-1])
	if err != nil || n < 0 || n > 15 {
		return false, false
	}
	return n == 7 || n >= 9, true
}
