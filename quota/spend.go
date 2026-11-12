package quota

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// costLimit matches shim/sidecar.go: a price at or past it is damage, not a
// bill, and is never counted.
const costLimit = (1 << 53) / 100.0

// tokenLimit is the matching bound for a token count: a count at or past it is
// damage, not use, and is never summed.
const tokenLimit = 1 << 53

type receipt struct {
	Complete bool   `json:"complete"`
	Profile  string `json:"profile"`
	// ResponseID and TurnID are the row's identity. They are any, not string,
	// so a row whose id is not a string still decodes and still counts.
	ResponseID any          `json:"response_id"`
	TurnID     any          `json:"turn_id"`
	Timestamp  string       `json:"timestamp"`
	Usage      receiptUsage `json:"usage"`
}

// receiptUsage is what a receipt's usage carries that this view counts: the
// cost a gateway billed (or a list price estimated for it) and the tokens the
// response served, which stay countable when no price was known.
type receiptUsage struct {
	Cost          *float64 `json:"cost"`
	EstimatedCost *float64 `json:"estimated_cost"`
	// Tokens arrive as the shim's whitelisted buckets; a float spelling is as
	// readable as an integer one, so both decode here.
	InputTokens              *float64 `json:"input_tokens"`
	CacheCreationInputTokens *float64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *float64 `json:"cache_read_input_tokens"`
	OutputTokens             *float64 `json:"output_tokens"`
}

// tokens is every token bucket the response carried: the use a profile served,
// visible whether or not any price was known for it. A bucket that is not a
// plain non-negative count below tokenLimit is not use and adds nothing.
func (u receiptUsage) tokens() int64 {
	var n int64
	for _, v := range []*float64{u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens, u.OutputTokens} {
		if v != nil && *v >= 0 && *v < tokenLimit {
			n += int64(*v)
		}
	}
	return n
}

func billable(v *float64) bool {
	return v != nil && !math.IsInf(*v, 0) && !math.IsNaN(*v) && *v >= 0 && *v < costLimit
}

// spendEntry is a gateway or budget profile's row. It reads the receipts the
// profile's own turns wrote; it never makes a request, so it costs nothing.
func spendEntry(name, budgetRaw string) Entry {
	e := Entry{Profile: name, Kind: "gateway", Windows: []Window{}}
	s := &Spend{}
	var budget *float64
	if v, err := strconv.ParseFloat(budgetRaw, 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		budget = &v
		s.BudgetUSD = budget
		e.Kind = "budget"
	}
	e.Spend = s

	today := now()
	todayStart := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.Local)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.Local)
	t := monthTallies(filepath.Join(paths.StateDir(), "usage"), today, monthStart, todayStart)[name]
	s.TodayUSD, s.MonthUSD, s.Estimated = t.TodayUSD, t.MonthUSD, t.Estimated
	s.TodayTurns, s.MonthTurns, s.UnpricedTurns = t.TodayTurns, t.MonthTurns, t.UnpricedTurns
	s.TodayTokens, s.MonthTokens = t.TodayTokens, t.MonthTokens
	if budget != nil {
		resets := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.Local).Unix()
		used := s.MonthUSD / *budget
		e.Windows = []Window{{ID: "month", Label: "month", Used: used, ResetsAt: &resets}}
	}
	return e
}

const dayLayout = "2006-01-02"

// tally is the counts one profile's receipts add up to.
type tally struct {
	TodayUSD, MonthUSD                    float64
	Estimated                             bool
	TodayTurns, MonthTurns, UnpricedTurns int
	TodayTokens, MonthTokens              int64
}

// spendRow is what one receipt adds to the month: nil for a receipt that never
// counts (unreadable, incomplete, or with no stamp).
type spendRow struct {
	Profile   string  `json:"p"`
	Stamp     string  `json:"t"`
	Key       string  `json:"k,omitempty"`
	Tokens    int64   `json:"n,omitempty"`
	Cost      float64 `json:"c,omitempty"`
	Priced    bool    `json:"pr,omitempty"`
	Estimated bool    `json:"e,omitempty"`
}

func readRow(path string) *spendRow {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var r receipt
	if json.Unmarshal(raw, &r) != nil || !r.Complete {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, r.Timestamp); err != nil {
		return nil
	}
	out := &spendRow{Profile: r.Profile, Stamp: r.Timestamp, Tokens: r.Usage.tokens()}
	// A response the store holds under two names (a flat copy and its day
	// folder's) is one turn: it counts once, by the identity share/usage-export
	// keys on (#298). A row with no id always counts.
	if id, ok := r.ResponseID.(string); ok && id != "" {
		out.Key = id + "\x00" + r.Timestamp
		if turn, ok := r.TurnID.(string); ok && turn != "" {
			out.Key += "\x00" + turn
		}
	}
	switch {
	case billable(r.Usage.Cost):
		out.Cost, out.Priced = *r.Usage.Cost, true
	case billable(r.Usage.EstimatedCost):
		out.Cost, out.Priced, out.Estimated = *r.Usage.EstimatedCost, true, true
	}
	return out
}

// addRows adds every row stamped this month to its profile's tally, each
// response once.
func addRows(rows []*spendRow, monthStart, todayStart time.Time, counted map[string]bool, out map[string]tally) {
	for _, r := range rows {
		if r == nil {
			continue
		}
		ts, _ := time.Parse(time.RFC3339, r.Stamp)
		if ts.Before(monthStart) {
			continue
		}
		if r.Key != "" {
			if counted[r.Key] {
				continue
			}
			counted[r.Key] = true
		}
		// Every completed response is a turn the profile served, and its tokens
		// are use a missing price must not hide (#234).
		t := out[r.Profile]
		todayTurn := !ts.Before(todayStart)
		t.MonthTurns++
		t.MonthTokens += r.Tokens
		if todayTurn {
			t.TodayTurns++
			t.TodayTokens += r.Tokens
		}
		if r.Priced {
			t.Estimated = t.Estimated || r.Estimated
			t.MonthUSD += r.Cost
			if todayTurn {
				t.TodayUSD += r.Cost
			}
		} else {
			// No price was known for this turn: its cost is unknown, not zero.
			t.UnpricedTurns++
		}
		out[r.Profile] = t
	}
}

// tallyMemo holds one scan per process: a quota call reads every gateway
// profile, and each would otherwise re-read the whole month.
var tallyMemo struct {
	sync.Mutex
	key string
	by  map[string]tally
}

// monthTallies is every profile's tally for the month. The receipts sit in the
// flat store and in day folders named by UTC day, compared by name — stepping
// local midnights as instants lands after now east of UTC and skips today's
// folder. A row always sits in a folder at or after its stamp's UTC day, so an
// earlier folder holds only rows the month window itself excludes; the stamp
// still decides. A folder whose whole UTC day ended before today's local start
// can hold no turn of today, so its rows are kept and reused until the folder
// changes (a late move links rows into it).
func monthTallies(usageDir string, today, monthStart, todayStart time.Time) map[string]tally {
	key := usageDir + "\x00" + monthStart.String() + "\x00" + todayStart.String()
	tallyMemo.Lock()
	defer tallyMemo.Unlock()
	if tallyMemo.key == key {
		return tallyMemo.by
	}
	first, last := monthStart.UTC().Format(dayLayout), today.UTC().Format(dayLayout)
	openFrom := todayStart.UTC().Format(dayLayout)
	out := map[string]tally{}
	// The flat receipts are read before any day folder is, and the order is the
	// point: a move relocating a row links its day-folder name first and removes
	// the flat name only after, so whichever moment between the two reads the
	// move lands in, one of the reads holds the row. A day folder read first can
	// serve rows cached from before the link while the flat name is already
	// gone, and the row reaches no tally at all.
	flat, _ := filepath.Glob(filepath.Join(usageDir, "*.json"))
	flatRows := make([]*spendRow, 0, len(flat))
	for _, p := range flat {
		flatRows = append(flatRows, readRow(p))
	}
	var rows []*spendRow
	entries, _ := os.ReadDir(usageDir)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name < first || name > last {
			continue
		}
		if _, err := time.Parse(dayLayout, name); err != nil {
			continue // a folder that is not a day shard
		}
		dir := filepath.Join(usageDir, name)
		if name < openFrom {
			rows = append(rows, closedDay(dir, name, monthStart)...)
			continue
		}
		rows = append(rows, openDay(dir, name)...)
	}
	// The day folders' copies come first and the flat ones last, and one
	// identity spans them all: a response the store holds under two names — a
	// flat copy and its day folder's, or copies in two day folders, the move
	// placing by mtime day while the writers place by stamp day — is one turn,
	// counted once (#298, #514). The top level is always read, as the move's
	// contract says: it leaves a flat row in place when its mtime day's folder
	// already holds another row under its name, and a filesystem without
	// hardlinks keeps every row flat.
	addRows(append(rows, flatRows...), monthStart, todayStart, map[string]bool{}, out)
	tallyMemo.key, tallyMemo.by = key, out
	return out
}

// dayCache is a kept day folder's rows of the month and the folder state they
// were read at. The rows, not a tally, are what it keeps: a response can sit in
// two day folders, so a day's numbers cannot be summed on their own — the month
// merge counts each response once across every folder and the flat store
// (#514). A cache from before that (a tally) carries no rows and is counted
// again.
type dayCache struct {
	MTime int64       `json:"mtime"`
	Month int64       `json:"month"`
	Rows  []*spendRow `json:"rows"`
}

// closedDay is a past day folder's rows of the month, read from its cache while
// the folder's mtime is the one they were read at. The mtime is taken before
// the files are read, so a row linked in during the read changes it and the
// next call counts again.
func closedDay(dir, name string, monthStart time.Time) []*spendRow {
	info, err := os.Stat(dir)
	if err != nil {
		return nil
	}
	cachePath := filepath.Join(cacheDir(), "spend", name+".json")
	var c dayCache
	if b, err := os.ReadFile(cachePath); err == nil && json.Unmarshal(b, &c) == nil &&
		c.MTime == info.ModTime().UnixNano() && c.Month == monthStart.Unix() && c.Rows != nil {
		return c.Rows
	}
	// A past day still changes (a hook re-reading an old transcript writes into
	// its stamp's day), so the recount goes through the per-file index: only the
	// receipts that changed are read.
	c = dayCache{MTime: info.ModTime().UnixNano(), Month: monthStart.Unix(), Rows: []*spendRow{}}
	for _, r := range openDay(dir, name) {
		if r == nil {
			continue
		}
		if ts, _ := time.Parse(time.RFC3339, r.Stamp); !ts.Before(monthStart) {
			c.Rows = append(c.Rows, r)
		}
	}
	writeAtomic(cachePath, c)
	return c.Rows
}

// seenFile is one receipt of an open day as it was last read.
type seenFile struct {
	Size  int64     `json:"s"`
	MTime int64     `json:"m"`
	Row   *spendRow `json:"r"`
}

// openDay is the rows of a day folder still being written. Each receipt is
// read once and kept with its size and mtime; a later call reads only the
// receipts that are new or changed since, and drops the ones that are gone.
func openDay(dir, name string) []*spendRow {
	cachePath := filepath.Join(cacheDir(), "spend", name+".open.json")
	var old map[string]seenFile
	if b, err := os.ReadFile(cachePath); err == nil {
		_ = json.Unmarshal(b, &old)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	seen := make(map[string]seenFile, len(entries))
	changed := false
	rows := make([]*spendRow, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		f, ok := old[n]
		if !ok || f.Size != info.Size() || f.MTime != info.ModTime().UnixNano() {
			f = seenFile{Size: info.Size(), MTime: info.ModTime().UnixNano(), Row: readRow(filepath.Join(dir, n))}
			changed = true
		}
		delete(old, n)
		seen[n] = f
		rows = append(rows, f.Row)
	}
	if changed || len(old) > 0 {
		writeAtomic(cachePath, seen)
	}
	return rows
}

// writeAtomic replaces path with v's JSON, or leaves it as it was.
func writeAtomic(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	_, werr := f.Write(b)
	if cerr := f.Close(); werr != nil || cerr != nil || os.Rename(f.Name(), path) != nil {
		os.Remove(f.Name())
	}
}
