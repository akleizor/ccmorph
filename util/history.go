package util

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/ordjson"
)

// mergeHistory shares the prompt history at from — the profile's own file, or
// a link at the user's own history on another disk — into the shared history
// at to (#177). It holds one machine-wide lock while it works, so two
// profiles merging at the same time cannot each rewrite what the other just
// read, and it APPENDS the records the shared file lacks instead of rewriting
// it: the file's inode never changes, so a session appending by pathname —
// claude writes that way, through the very links setup made — cannot lose a
// line to the merge. The appended records are deduplicated, carry their own
// timestamp order, and a line no reader can order is kept as spelled. The
// source is left intact when it is a link; a real file is removed only after
// every record is verified present, so a failure keeps the profile on its own
// history whole. The caller re-points the profile at to once this reports
// success.
func mergeHistory(from, to string) bool {
	src := from
	if isLink(from) {
		r, err := filepath.EvalSymlinks(from)
		if err != nil {
			return false
		}
		src = r
	}
	own, ok := readHistory(src)
	if !ok {
		return false
	}
	if len(own) == 0 {
		// Nothing to merge: the profile's file or link simply goes.
		return os.Remove(from) == nil
	}
	unlock, locked := historyLock(to)
	defer unlock()
	if !locked {
		return false
	}
	// Re-read under the lock: whatever another profile's merge appended is
	// already here, and the dedup below keeps the two merges from doubling it.
	shared, ok := readHistory(to)
	if !ok {
		return false
	}
	have := make(map[string]bool, len(shared))
	for _, l := range shared {
		have[l] = true
	}
	type hline struct {
		text    string
		ts      int64
		ordered bool
	}
	var missing []hline
	for _, l := range own {
		if l == "" || have[l] {
			continue
		}
		ts, ordered := historyTime(l)
		missing = append(missing, hline{l, ts, ordered})
	}
	if len(missing) == 0 {
		// The shared file already holds every record; the profile's file goes.
		return os.Remove(from) == nil
	}
	sort.SliceStable(missing, func(i, j int) bool {
		if missing[i].ordered != missing[j].ordered {
			return missing[i].ordered
		}
		return missing[i].ts < missing[j].ts
	})
	var out strings.Builder
	for _, l := range missing {
		out.WriteString(l.text)
	}
	// Append, never rewrite: an O_APPEND write lands on the live inode
	// whatever else holds it open.
	f, err := os.OpenFile(to, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return false
	}
	_, werr := f.WriteString(out.String())
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return false
	}
	// Verify from the file itself that every record landed.
	got, ok := readHistory(to)
	if !ok {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, l := range got {
		seen[l] = true
	}
	for _, l := range missing {
		if !seen[l.text] {
			return false
		}
	}
	return os.Remove(from) == nil
}

// historyLock takes the one machine-wide lock every shared-history merge
// holds — beside the shared file, so any ccmorph, any profile, any launch
// serializes through it — and returns its release with whether it was taken
// at all (#177). It blocks: the merge it guards is file I/O only and brief.
func historyLock(to string) (func(), bool) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(to), ".ccm-history.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return func() {}, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return func() {}, false
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true
}

// readHistory reads a prompt-history file as its lines, newline included; a
// missing file is no history, an unreadable one is a failure.
func readHistory(p string) ([]string, bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, true
		}
		return nil, false
	}
	lines := strings.SplitAfter(string(b), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines, true
}

// historyTime is a prompt record's timestamp — the field Claude Code orders
// history by — false when the line is not a record a reader can order.
func historyTime(line string) (int64, bool) {
	v, err := ordjson.Parse([]byte(line))
	if err != nil {
		return 0, false
	}
	n, ok := getKey(asObj(v), "timestamp").(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}
