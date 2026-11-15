package paths

import (
	"os"
	"time"
)

// DayLayout is how the usage store names a day folder: one UTC date, the day
// of the receipt's own stamp (#227). The writer shards a row into its day's
// folder by formatting the stamp, and every reader — quota's month walk, the
// report's window — formats its window's bounds the same way, so a window
// compares folder names as strings: lexical order is date order.
const DayLayout = "2006-01-02"

// IsDayName is the one rule for a usage-store day folder's name: DayLayout,
// and a real calendar date. The writer only ever makes real dates, so a folder
// named 2026-02-30 — the shape of a day, but no day of any year — is not the
// store's, whichever reader asks (#546).
func IsDayName(name string) bool {
	_, err := time.Parse(DayLayout, name)
	return err == nil
}

// DayNames is a usage store's day folders in the window [first, last], in name
// order: the report's window walk. The one rule both readers share is
// IsDayName — quota's month read lists its own days, because its verdict needs
// the entries the listing itself could not read (#545), but it rules each name
// by the same leaf, so the two cannot disagree about which folders are day
// shards (#546): a folder that is not a directory, a name outside [first,
// last], or a name IsDayName rules out is not a day. A store that cannot be
// read is no days.
func DayNames(base, first, last string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if name < first || name > last || !IsDayName(name) {
			continue
		}
		days = append(days, name)
	}
	return days
}
