package util

import (
	"io"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/pycompat"
)

// listingHasUnsized: listing-has-unsized <profile> exits 0 when the listing on
// stdin names a chat model the profile has no size, pin or recent miss for, so
// the sizer is worth starting; 1 when every model is already known.
func listingHasUnsized(args []string, in io.Reader, _, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	var fields map[string]string
	if pycompat.Catch(func() { _, fields = ctxwindow.ReadConf(ctxwindow.ProfilePath(args[0])) }) != nil {
		return 0
	}
	doc, err := pycompat.Parse(readAll(in))
	if err != nil {
		return 0
	}
	top, _ := doc.(*pycompat.Obj)
	if top == nil {
		return 0
	}
	data, _ := top.Get("data")
	known := ctxwindow.DiscoveredValues(fields)
	misses := ctxwindow.DiscoveredMisses(fields)
	pins := ctxwindow.ManualValues(fields)
	for _, row := range ctxwindow.ListingRows(data) {
		id, _ := row.Get("id")
		s, _ := id.(string)
		if _, pinned := pins[s]; ctxwindow.IsExcluded(fields, s) || known.Has(s) || misses.Has(s) || pinned {
			continue
		}
		return 0
	}
	return 1
}
