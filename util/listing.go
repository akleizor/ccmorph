package util

import (
	"encoding/json"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// ListingHasUnsized reports whether the /v1/models listing names a chat model the
// profile (its conf file) has no size, pin or recent miss for, so the sizer is
// worth starting. A conf or listing that cannot be read says yes: the sizer
// decides.
func ListingHasUnsized(conf, listing string) bool {
	_, fields, err := ctxwindow.ReadConf(conf)
	if err != nil {
		return true
	}
	var top map[string]any
	if json.Unmarshal([]byte(listing), &top) != nil || top == nil {
		return true
	}
	data := top["data"]
	known := ctxwindow.DiscoveredValues(fields)
	misses := ctxwindow.DiscoveredMisses(fields)
	pins := ctxwindow.ManualValues(fields)
	for _, row := range ctxwindow.ListingRows(data) {
		s, _ := row["id"].(string)
		_, pinned := pins[s]
		_, isKnown := known[s]
		_, missed := misses[s]
		if ctxwindow.IsExcluded(fields, s) || isKnown || missed || pinned {
			continue
		}
		return true
	}
	return false
}
