package textwidth

import "testing"

// Cells answers the terminal's width for the scripts a model id, a profile
// name or a report sentence can carry (#557). Each case is one rule: CJK
// ideographs, kana and Hangul draw two cells, and so does a fullwidth latin
// while its halfwidth forms draw one (the halfwidth voiced mark is a letter
// of its own, not a combining mark); a joined sequence counts its parts, so
// the family of two adults is four and the rainbow flag three (its white
// flag is a neutral rune, its rainbow a wide one); a decomposed accent or
// voiced kana combines onto the rune before it. The watch is a
// Wide pictograph both old tables missed; the ideographic half fill space
// and the Hangul single dot tone mark are the narrow neighbours the old
// catch-alls swallowed; the Hangul filler is invisible but wide; a regional
// indicator pair renders as a flag yet takes one cell each; a plane-2 CJK
// extension ideograph is two; the ambiguous em dash and middle dot stay one.
func TestCellsCountsRepresentativeCodePoints(t *testing.T) {
	cases := []struct {
		text string
		want int
		note string
	}{
		{"中", 2, "a CJK ideograph"},
		{"モデル", 6, "katakana"},
		{"한글", 4, "Hangul syllables"},
		{"Ａ", 2, "a fullwidth latin"},
		{"ｱﾙﾌｧ", 4, "halfwidth katakana"},
		{"a\U0001F468‍\U0001F469b", 6, "a joined family between two letters"},
		{"\U0001F3F3️‍\U0001F308", 3, "the rainbow flag: a neutral white flag, a variation selector, a joiner, a wide rainbow"},
		{"é", 1, "a decomposed acute accent"},
		{"が", 2, "a decomposed voiced kana"},
		{"⌚", 2, "the watch, a Wide pictograph"},
		{"〿", 1, "the ideographic half fill space"},
		{"〮", 1, "the Hangul single dot tone mark"},
		{"ㅤ", 2, "the Hangul filler, invisible but wide"},
		{"\U0001F1E6\U0001F1E9", 2, "regional indicators, one cell each"},
		{"\U00020000", 2, "a plane-2 CJK extension ideograph"},
		{"—", 1, "the ambiguous em dash"},
		{"·", 1, "the ambiguous middle dot"},
	}
	for _, tc := range cases {
		if got := Cells(tc.text); got != tc.want {
			t.Errorf("%s: Cells(%q) = %d, want %d", tc.note, tc.text, got, tc.want)
		}
	}
}

// The table Cells reads must stay a well-formed maximal range table: sorted,
// disjoint, split at the right planes, and every range bounded by code
// points that are not wide — so no range hides a neighbour it could merge
// with, and no gap between ranges is silently wide.
func TestTheWideTableIsSortedDisjointAndMaximal(t *testing.T) {
	var ranges [][2]rune
	for _, r := range eastAsianWide.R16 {
		if r.Hi > 0xFFFF {
			t.Fatalf("R16 range {0x%X, 0x%X} reaches past the BMP", r.Lo, r.Hi)
		}
		ranges = append(ranges, [2]rune{rune(r.Lo), rune(r.Hi)})
	}
	for _, r := range eastAsianWide.R32 {
		if r.Lo <= 0xFFFF {
			t.Fatalf("R32 range {0x%X, 0x%X} starts inside the BMP", r.Lo, r.Hi)
		}
		ranges = append(ranges, [2]rune{rune(r.Lo), rune(r.Hi)})
	}
	for i, rg := range ranges {
		if rg[0] > rg[1] {
			t.Errorf("range %d {0x%X, 0x%X} ends before it starts", i, rg[0], rg[1])
			continue
		}
		if i > 0 && rg[0] <= ranges[i-1][1] {
			t.Errorf("range {0x%X, 0x%X} overlaps or abuts {0x%X, 0x%X}: merge them",
				rg[0], rg[1], ranges[i-1][0], ranges[i-1][1])
		}
		// A range's own endpoints are wide; the code points on either side
		// are not (a combining neighbour counts zero, anything else one).
		for _, r := range []rune{rg[0], rg[1]} {
			if got := Cells(string(r)); got != 2 {
				t.Errorf("U+%04X inside {0x%X, 0x%X} counts %d, want 2", r, rg[0], rg[1], got)
			}
		}
		for _, r := range []rune{rg[0] - 1, rg[1] + 1} {
			if r < 0 || r > 0x10FFFF {
				continue
			}
			if got := Cells(string(r)); got > 1 {
				t.Errorf("U+%04X outside {0x%X, 0x%X} counts %d: the range is not maximal", r, rg[0], rg[1], got)
			}
		}
	}
}
