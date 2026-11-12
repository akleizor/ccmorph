// Package argparse holds the flag-spelling rules two of the small argparse
// parsers share: ctxwindow's probe and settingscompose. The adapter keeps its
// own variant — its options match by exact name and its value-taking options
// swallow the next argument unconditionally, so the shared prefix resolution
// and value lookahead would change its errors — as it keeps its own truthy
// (#423).
package argparse

import (
	"strings"

	"github.com/akleizor/ccmorph/ordjson"
)

// Matches is every option in opts that name equals or prefixes, in opts
// order. An exact hit is the whole answer: a longer option that also starts
// with name loses to name's own entry, and the scan stops there.
func Matches(name string, opts []string) []string {
	var match []string
	for _, f := range opts {
		if f == name {
			return []string{f}
		}
		if strings.HasPrefix(f, name) {
			match = append(match, f)
		}
	}
	return match
}

// OptionLike reports whether arg reads as an option rather than a value: a
// dash with something after it, unless it spells a number (a negative value).
func OptionLike(arg string) bool {
	return strings.HasPrefix(arg, "-") && len(arg) > 1 && !ordjson.IsNumber(arg)
}
