package cli

import (
	"strings"
	"testing"
)

// #274: on a white background every role ccmorph paints with must keep at least
// 4.5:1 contrast, so the light palette is held to the bar on the light
// terminals people actually run — white and Solarized Light — not on a computed
// worst case nobody has.
func TestLightPaletteMeetsContrastOnLightBackgroundsIssue274(t *testing.T) {
	for role, hex := range map[string]string{
		"accent (OK mark, default star, headings)": lightPalette.accent,
		"focus (INFO mark, apikey/sub words)":      lightPalette.focus,
		"yellow (WARN mark, token word)":           lightPalette.yellow,
		"red (FAIL mark, ccmorph: prefix)":         lightPalette.red,
		"gray (SKIP mark, descriptions)":           lightPalette.gray,
		"dim (details on light terminals)":         lightPalette.dim,
	} {
		for _, bg := range []string{"ffffff", "fdf6e3"} {
			if c := contrastRatio(hex, bg); c < 4.5 {
				t.Errorf("%s #%s on #%s: %.2f < 4.5", role, hex, bg, c)
			}
		}
	}
	// The fzf border is a line, not text: 3:1 is the bar it must clear.
	for _, bg := range []string{"ffffff", "fdf6e3"} {
		if c := contrastRatio(lightPalette.border, bg); c < 3 {
			t.Errorf("border #%s on #%s: %.2f < 3", lightPalette.border, bg, c)
		}
	}
}

// The dark palette is the one ccmorph has always shipped; these exact values
// are what "a dark background looks as it does now" means.
func TestDarkPaletteIsUnchangedIssue274(t *testing.T) {
	want := palette{accent: "d77757", focus: "b1b9f9", yellow: "ffc107",
		red: "ff6b80", gray: "999999", dim: "", border: "888888"}
	if darkPalette != want {
		t.Errorf("dark palette changed to %+v", darkPalette)
	}
	// Dim on dark stays SGR 2, not a fixed colour: on a dark background every
	// terminal renders it against its own background.
	if got := darkPalette.dimEscape(); got != "\033[2m" {
		t.Errorf("dark dim escape %q, want SGR 2", got)
	}
	if got := lightPalette.dimEscape(); got != "\033[38;2;107;107;107m" {
		t.Errorf("light dim escape %q, want the fixed grey #6b6b6b", got)
	}
}

func TestFzfSpecFollowsThePaletteIssue274(t *testing.T) {
	for _, tc := range []struct{ name, spec string }{
		{"dark", darkPalette.fzfSpec()},
		{"light", lightPalette.fzfSpec()},
	} {
		if !strings.HasPrefix(tc.spec, "fg:-1,bg:-1,") {
			t.Errorf("%s fzf spec must keep the terminal's own text colour: %s", tc.name, tc.spec)
		}
	}
	for _, needle := range []string{"prompt:#d77757", "pointer:#b1b9f9", "border:#888888"} {
		if !strings.Contains(darkPalette.fzfSpec(), needle) {
			t.Errorf("dark fzf spec lost %s: %s", needle, darkPalette.fzfSpec())
		}
	}
	for _, needle := range []string{"prompt:#a0481d", "pointer:#4453b0", "border:#767676", "header:#666666"} {
		if !strings.Contains(lightPalette.fzfSpec(), needle) {
			t.Errorf("light fzf spec lost %s: %s", needle, lightPalette.fzfSpec())
		}
	}
}

// A terminal's answer to the background query: OSC 11, 1-4 hex digits per
// channel, BEL or ST terminator, possibly riding along after another reply.
func TestParseBackgroundReplyIssue274(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		r, g, b int
		ok      bool
	}{
		{"16-bit channels, ST terminator", "\x1b]11;rgb:ffff/ffff/ffff\x1b\\", 255, 255, 255, true},
		{"16-bit channels, BEL terminator", "\x1b]11;rgb:8080/7f7f/0000\x07", 128, 127, 0, true},
		{"8-bit channels", "\x1b]11;rgb:00/2b/36\x07", 0, 43, 54, true},
		{"one digit per channel", "\x1b]11;rgb:f/0/8\x07", 255, 0, 136, true},
		{"after a cursor report", "\x1b[1;1R\x1b]11;rgb:ffff/ffff/ffff\x07", 255, 255, 255, true},
		{"truncated", "\x1b]11;rgb:ffff/ff", 0, 0, 0, false},
		{"no colour payload", "\x1b]11;?\x07", 0, 0, 0, false},
		{"another sequence", "\x1b[0n", 0, 0, 0, false},
		{"empty", "", 0, 0, 0, false},
	} {
		r, g, b, ok := parseBackgroundReply(tc.reply)
		if ok != tc.ok || (ok && (r != tc.r || g != tc.g || b != tc.b)) {
			t.Errorf("%s: got (%d,%d,%d,%v), want (%d,%d,%d,%v)", tc.name, r, g, b, ok, tc.r, tc.g, tc.b, tc.ok)
		}
	}
}

// The probe owns the reply and nothing else: whatever it read around the reply
// is handed back to be pushed onto the input queue, in order, and an unfinished
// reply is not handed back as junk.
func TestSplitBackgroundReplyKeepsOtherInputIssue274(t *testing.T) {
	for _, tc := range []struct {
		name, got, rest string
		done, found     bool
	}{
		{"type-ahead before, ST", "ab\x1b]11;rgb:ffff/ffff/ffff\x1b\\", "ab", true, true},
		{"type-ahead around, BEL", "ab\x1b]11;rgb:00/00/00\x07cd", "abcd", true, true},
		{"reply alone", "\x1b]11;rgb:00/00/00\x07", "", true, true},
		{"reply with no colour", "x\x1b]11;?\x07y", "xy", true, false},
		{"ESC that is not an ST ends the reply", "\x1b]11;rgb:00/00/00\x1b[A", "\x1b[A", true, true},
		{"waiting for the ST", "\x1b]11;rgb:00/00/00\x1b", "\x1b]11;rgb:00/00/00\x1b", false, false},
		{"no reply yet", "abc", "abc", false, false},
	} {
		rest, _, _, _, done, found := splitBackgroundReply([]byte(tc.got))
		if string(rest) != tc.rest || done != tc.done || found != tc.found {
			t.Errorf("%s: got (%q,%v,%v), want (%q,%v,%v)", tc.name, rest, done, found, tc.rest, tc.done, tc.found)
		}
	}
	for _, tc := range []struct{ got, want string }{
		{"ab", "ab"},
		{"ab\x1b]11;rgb:ff", "ab"},
		{"ab\x1b]", "ab"},
		{"ab\x1b", "ab\x1b"},
	} {
		if got := string(withoutPartialReply([]byte(tc.got))); got != tc.want {
			t.Errorf("withoutPartialReply(%q) = %q, want %q", tc.got, got, tc.want)
		}
	}
}

// The background is light when its luminance says so: white and the common
// light themes are light, dark themes and mid-greys are not.
func TestLightBackgroundIssue274(t *testing.T) {
	for _, tc := range []struct {
		name    string
		r, g, b int
		light   bool
	}{
		{"white", 255, 255, 255, true},
		{"solarized light", 0xfd, 0xf6, 0xe3, true},
		{"solarized dark", 0x00, 0x2b, 0x36, false},
		{"mid grey", 0x80, 0x80, 0x80, false},
	} {
		if got := lightBackground(tc.r, tc.g, tc.b); got != tc.light {
			t.Errorf("%s: lightBackground(%d,%d,%d) = %v", tc.name, tc.r, tc.g, tc.b, got)
		}
	}
}

// COLORFGBG: the last field is the background; 0-6 and 8 are dark, 7 and 9-15
// light, anything else (default, empty) is no answer at all.
func TestFgbgLightIssue274(t *testing.T) {
	for _, tc := range []struct {
		fgbg  string
		light bool
		ok    bool
	}{
		{"15;0", false, true},
		{"0;15", true, true},
		{"0;7", true, true},
		{"0;9", true, true},
		{"0;default;15", true, true},
		{"15;0;0", false, true},
		{"0;8", false, true},
		{"0;default", false, false},
		{"default", false, false},
		{"", false, false},
	} {
		light, ok := fgbgLight(tc.fgbg)
		if ok != tc.ok || (ok && light != tc.light) {
			t.Errorf("fgbgLight(%q) = (%v,%v), want (%v,%v)", tc.fgbg, light, ok, tc.light, tc.ok)
		}
	}
}

// The order the palette is picked in: the forced theme, then the terminal's own
// answer, then COLORFGBG, and dark when nothing says otherwise.
func TestDecideLightOrderIssue274(t *testing.T) {
	silent := func(int) (bool, bool) { return false, false }
	for _, tc := range []struct {
		name  string
		theme string
		fgbg  string
		ask   func(int) (bool, bool)
		want  bool
	}{
		{"forced light wins over a dark answer", "light", "", func(int) (bool, bool) { return false, true }, true},
		{"forced dark wins over a light answer", "dark", "", func(int) (bool, bool) { return true, true }, false},
		{"forced light, any case or padding", " Light ", "", silent, true},
		{"the answer wins over COLORFGBG", "", "0;15", func(int) (bool, bool) { return false, true }, false},
		{"COLORFGBG when the terminal stays silent", "", "0;15", silent, true},
		{"dark when nothing says otherwise", "", "", silent, false},
	} {
		if got := decideLight(tc.theme, tc.fgbg, tc.ask, 1); got != tc.want {
			t.Errorf("%s: decideLight = %v", tc.name, got)
		}
	}
}
