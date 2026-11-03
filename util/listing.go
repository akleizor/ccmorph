package util

import (
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/pycompat"
)

// ListingHasUnsized reports whether the /v1/models listing names a chat model the
// profile (its conf file) has no size, pin or recent miss for, so the sizer is
// worth starting. A conf or listing that cannot be read says yes: the sizer
// decides.
func ListingHasUnsized(conf, listing string) bool {
	var fields map[string]string
	if pycompat.Catch(func() { _, fields = ctxwindow.ReadConf(conf) }) != nil {
		return true
	}
	doc, err := pycompat.Parse([]byte(listing))
	if err != nil {
		return true
	}
	top, _ := doc.(*pycompat.Obj)
	if top == nil {
		return true
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
		return true
	}
	return false
}
