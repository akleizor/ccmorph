package shim

import (
	"os"
	"path/filepath"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// lockClosedUsage takes the store lock when a write can reach a day the
// compactor may fold into its archive. now is the instant the pass acts on:
// its closed-day boundary is paths.UsageOpenFrom's, the one readers and the
// compactor compute from the same instant, so no day is both compactable and
// still being written. An open day's ordinary stream does no locking.
func lockClosedUsage(now time.Time, dirs []string) (*os.File, error) {
	openFrom := paths.UsageOpenFrom(now)
	for _, dir := range dirs {
		day := filepath.Base(dir)
		if !paths.IsDayName(day) || day >= openFrom {
			continue
		}
		base := usageBase()
		if err := os.MkdirAll(base, 0o700); err != nil {
			return nil, err
		}
		// A hook that queues behind a compaction's exclusive hold across its
		// I/O can freeze a Stop for minutes with no message; the store's own
		// bound refuses instead, and the cursor stays on the unwritten row
		// (INTENT 8, #654).
		return paths.UsageStoreLockWithin(base, true, paths.UsageLockWait)
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
