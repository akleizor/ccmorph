package cli

import (
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
