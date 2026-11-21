package cli

import (
	"strings"
	"testing"
)

// #358: the fold erases every row the value line took, and no more.
func TestValueRowsCountsEveryRowTheValueTook(t *testing.T) {
	for _, c := range []struct {
		value   string
		columns int
		want    int
	}{
		{"high", 100, 1},
		{"", 80, 1},
		{strings.Repeat("a", 100), 100, 1},
		{strings.Repeat("a", 101), 100, 2},
		{strings.Repeat("a", 200), 100, 2},
		{strings.Repeat("a", 201), 100, 3},
		{"picker-alpha", 10, 2},
	} {
		if got := valueRows(c.value, c.columns); got != c.want {
			t.Errorf("valueRows(%d cells, %d columns) = %d, want %d", len(c.value), c.columns, got, c.want)
		}
	}
}

// #788: --json reads the same before or after the profile. A leading flag moves
// behind the profile slot; every other shape is left exactly as it came.
func TestJSONLastMovesTheFlagBehindTheProfile(t *testing.T) {
	for _, c := range []struct {
		args  []string
		want  []string
		moved bool
	}{
		{nil, nil, false},
		{[]string{"work"}, []string{"work"}, false},
		{[]string{"work", "--json"}, []string{"work", "--json"}, false},
		{[]string{"model", "base_url"}, []string{"model", "base_url"}, false},
		{[]string{"--json"}, []string{"", "--json"}, true},
		{[]string{"--json", "work"}, []string{"work", "--json"}, true},
	} {
		got, moved := jsonLast(c.args)
		if moved != c.moved || strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("jsonLast(%q) = %q, %v; want %q, %v", c.args, got, moved, c.want, c.moved)
		}
	}
}
