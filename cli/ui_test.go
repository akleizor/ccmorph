package cli

import "testing"

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
