package shim

import (
	"os"
	"path/filepath"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// Closed-day writes and adjacent-copy retirement exclude compaction. Today's
// ordinary stream does no locking: neither today nor yesterday can be compacted.
func lockClosedUsage(dirs []string) (*os.File, error) {
	now := time.Now().UTC()
	before := now.AddDate(0, 0, -1).Format(paths.DayLayout)
	today := now.Format(paths.DayLayout)
	for i, dir := range dirs {
		day := filepath.Base(dir)
		if !paths.IsDayName(day) || (day >= before && (i != 0 || day >= today)) {
			continue
		}
		base := usageBase()
		if err := os.MkdirAll(base, 0o700); err != nil {
			return nil, err
		}
		return paths.UsageStoreLock(base, true)
	}
	return nil, nil
}

func usageOwned(data []byte, responseID, turnID string) bool {
	d := parseDoc(data)
	if d == nil {
		return false
	}
	id, idOK := d.str("response_id")
	turn, turnOK := d.str("turn_id")
	return idOK && turnOK && id == responseID && turn == turnID
}
