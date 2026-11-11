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
