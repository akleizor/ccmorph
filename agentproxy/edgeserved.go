package agentproxy

import (
	"encoding/json"
	"path/filepath"
	"strconv"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// servedOutcome is the asked-vs-served count of this edge run's audited turns,
// kept in the state dir for the checks that read health (GH-977: #920's verdict
// reached only the debug log, so a miss on a normal start was audited and
// forgotten and the phone found the next one first). It counts what the verdict
// decided, never why: no key, no header, only the line the log already carries.
type servedOutcome struct {
	loaded   bool
	turns    int
	misses   int
	lastMiss map[string]any
}

func (e *Edge) servedPath() string {
	return filepath.Join(e.bridge(), "served."+strconv.Itoa(e.Parent)+".json")
}

// loadServed continues this very edge run's counters (a proxy that restarted
// behind the same edge keeps its record); a record written by any other start
// is no history of this one, so the counters begin at zero for it.
func (e *Edge) loadServed() {
	e.served.loaded = true
	b, err := paths.ReadRegularFile(e.servedPath()) // nonblocking: a FIFO under the name must not hold a turn's reply
	if err != nil {
		return
	}
	var rec struct {
		EdgeStarted *string        `json:"edge_started"`
		Turns       *int           `json:"turns"`
		Misses      *int           `json:"misses"`
		LastMiss    map[string]any `json:"last_miss"`
	}
	if json.Unmarshal(b, &rec) != nil || rec.Turns == nil || rec.Misses == nil {
		return
	}
	if (rec.EdgeStarted == nil) != (e.ParentStarted == "") || (rec.EdgeStarted != nil && *rec.EdgeStarted != e.ParentStarted) {
		return
	}
	e.served.turns, e.served.misses, e.served.lastMiss = *rec.Turns, *rec.Misses, rec.LastMiss
}

// CountServed records one audited session turn's verdict: turns and misses
// counted, the last miss kept with its line, the record replaced whole. The
// verdict never depends on the record: a state dir that refuses one is not an
// error the turn sees (the held record's lesson, read backwards), and the next
// turn's write carries the count the refused one missed.
func (e *Edge) CountServed(miss bool, line string) {
	if e.Inference == "" || e.Parent == 0 {
		return
	}
	e.heldMu.Lock()
	defer e.heldMu.Unlock()
	if e.stopped {
		return // what Stop leaves is final: no record for a proxy that is going
	}
	if !e.served.loaded {
		e.loadServed()
	}
	e.served.turns++
	if miss {
		e.served.misses++
		e.served.lastMiss = map[string]any{"at": time.Now().Unix(), "line": line}
	}
	var started any
	if e.ParentStarted != "" {
		started = e.ParentStarted
	}
	b, _ := json.Marshal(map[string]any{"edge": e.Parent, "edge_started": started,
		"turns": e.served.turns, "misses": e.served.misses, "last_miss": e.served.lastMiss})
	path := e.servedPath()
	_ = paths.AtomicWrite(paths.WriteIn{Path: path, Temp: filepath.Base(path) + ".*.tmp", Data: b, Mode: 0o600}) // the turn stands; the record missed one
}
