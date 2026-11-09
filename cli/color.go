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
// reports whether that colour is light. The question goes out only when stdin
// can carry the answer, and it is asked in raw mode so the reply is never
// echoed onto the screen; a terminal that does not answer in time says
// nothing, and the caller falls back to COLORFGBG.
//
// The probe owns only the reply: every other byte it reads (type-ahead, a key
// pressed while the question was out) goes back on the input queue before the
// terminal mode is restored, and a signal that arrives meanwhile restores the
// mode and then takes its default action, as it would have without the probe.
func backgroundLight(fd int) (light, ok bool) {
	if !isTTY(0) || !isTTY(fd) {
		return false, false
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	restore, err := rawTTY(os.Stdin)
	if err != nil {
		return false, false
	}
	defer restore()
	out := os.Stdout
	if fd == 2 {
		out = os.Stderr
	}
	if _, err := out.Write([]byte("\033]11;?\033\\")); err != nil {
		return false, false
	}
	var got []byte
	one := make([]byte, 1)
	for start := time.Now(); time.Since(start) < backgroundQueryTimeout && len(got) < backgroundQueryMax; {
		select {
		case s := <-sigs:
			die(restore, s)
		default:
		}
		n, err := os.Stdin.Read(one)
		if n > 0 {
			if one[0] == 3 {
				// A ^C typed while the question is out must not be swallowed
				// by raw mode.
				die(restore, syscall.SIGINT)
			}
			got = append(got, one[0])
			if rest, r, g, b, done, found := splitBackgroundReply(got); done {
				pushInput(0, rest)
				return lightBackground(r, g, b), found
			}
		}
		if err != nil {
			break
		}
	}
	pushInput(0, withoutPartialReply(got))
	return false, false
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
