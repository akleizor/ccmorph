package cli

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"testing"
)

// luminance is WCAG 2's relative luminance of a six-digit hex colour.
func luminance(hex string) float64 {
	ch := func(i int) float64 {
		v, _ := strconv.ParseUint(hex[i:i+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(0) + 0.7152*ch(2) + 0.0722*ch(4)
}

// #274 (moved to #636): colours chosen for dark backgrounds fell to 1.6-3.2:1
// on a light terminal. Every colour ccmorph paints reads at 4:1 or better on
// white and on black, and nothing paints from outside the palette.
func TestPaletteReadsOnLightAndDarkTerminals(t *testing.T) {
	for name, hex := range map[string]string{"accent": palette.accent, "yellow": palette.yellow,
		"red": palette.red, "cyan": palette.cyan, "gray": palette.gray} {
		l := luminance(hex)
		if onWhite, onBlack := 1.05/(l+0.05), (l+0.05)/0.05; onWhite < 4 || onBlack < 4 {
			t.Errorf("%s #%s reads %.2f:1 on white and %.2f:1 on black, want 4:1 on both", name, hex, onWhite, onBlack)
		}
	}
	literal := regexp.MustCompile(`"#?[0-9a-fA-F]{6}"|#[0-9a-fA-F]{6}\b`)
	for _, f := range []string{"ui.go", "launch.go", "doctor.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := regexp.MustCompile(`(?s)var palette = struct.*?\n}\n`).ReplaceAll(b, nil)
		if m := literal.FindAll(src, -1); len(m) > 0 {
			t.Errorf("%s paints a colour outside the palette: %q", f, m)
		}
	}
}
