package quota

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akleizor/ccmorph/util"
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

// spendCacheVersion is the shape of the files under quota/spend. A file from an
// earlier shape is rebuilt; the per-file index #536 kept beside a day's rows is
// migrated once, so an upgrade does not rescan the month. An index older than
// that carries no content key for an id-less row, so it is read again. Version
// 3 added the closed snapshot's validity and the retryable read: a version 2
// file may have remembered an unreadable receipt, or an open day's rewrite, as
// the day's answer, so it is rebuilt (#538).
const spendCacheVersion = 3

// tally is the counts one profile's receipts add up to.
type tally struct {
	TodayUSD, MonthUSD                    float64
	Estimated                             bool
	TodayTurns, MonthTurns, UnpricedTurns int
	TodayTokens, MonthTokens              int64
}

// spendRow is what one receipt adds to the month: nil for a receipt that never
// counts (unreadable, incomplete, or with no stamp). When is the stamp parsed
// once, in Unix seconds — both month edges are whole-second local midnights, so
// flooring a stamp cannot move it across one; Key is the response identity,
// hashed so the month's dedupe index costs bytes, not rows (#538).
type spendRow struct {
	Profile   string  `json:"p"`
	When      int64   `json:"w"`
	Key       string  `json:"k,omitempty"`
	Tokens    int64   `json:"n,omitempty"`
	Cost      float64 `json:"c,omitempty"`
	Priced    bool    `json:"pr,omitempty"`
	Estimated bool    `json:"e,omitempty"`
}

// keyID is a response's identity, hashed: response id, stamp and turn as the
// store spells them, one 128-bit value a month can keep millions of. Equal
// identities are byte-identical copies of one row (a hardlink the move made, or
// the same turn written to two day folders), never two different rows.
type keyID [16]byte

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

func parseKey(s string) (keyID, bool) {
	var k keyID
	if len(s) != 32 {
		return k, false
	}
	if _, err := hex.Decode(k[:], []byte(s)); err != nil {
		return k, false
	}
	return k, true
}

// keyIndex is a sorted run of the identities a closed month already counted.
// Stored as one blob and searched in place: an unchanged month costs no map and
// no row decode on the read path. A key that will not parse counts as no key,
// never as a dropped turn.
type keyIndex []keyID

func (ix keyIndex) has(k keyID) bool {
	i := sort.Search(len(ix), func(i int) bool { return bytes.Compare(ix[i][:], k[:]) >= 0 })
	return i < len(ix) && ix[i] == k
}

func encodeKeyIndex(seen map[keyID]struct{}) string {
	keys := make([]keyID, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i][:], keys[j][:]) < 0 })
	b := make([]byte, 0, len(keys)*len(keyID{}))
	for _, k := range keys {
		b = append(b, k[:]...)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func decodeKeyIndex(s string) keyIndex {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	ix := make(keyIndex, 0, len(b)/len(keyID{}))
	for len(b) >= len(keyID{}) {
		var k keyID
		copy(k[:], b[:len(k)])
		ix = append(ix, k)
		b = b[len(k):]
	}
	return ix
}

// readRow parses one receipt. A nil row is a receipt that never counts:
// unreadable JSON, incomplete, or a stamp that does not parse. ok is false only
// when the file could not be read at all; that may pass, so the caller must not
// remember the miss (#538).
func readRow(path string) (*spendRow, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return decodeRow(raw), true
}

func decodeRow(raw []byte) *spendRow {
	countRows(1)
	var r receipt
	if json.Unmarshal(raw, &r) != nil || !r.Complete {
		return nil
	}
	ts, err := time.Parse(time.RFC3339, r.Timestamp)
	if err != nil {
		return nil
	}
	out := &spendRow{Profile: r.Profile, When: ts.Unix(), Tokens: r.Usage.tokens()}
	// A response the store holds under two names (a flat copy and its day
	// folder's) is one turn: it counts once, by the identity share/usage-export
	// keys on (#298). A row with no id is named by its whole receipt — the
	// move's two names are hardlinks to one file, so every field the reader
	// counts matches under both, while two rows that differ in any of them,
	// such as their turn or their token buckets, stay distinct (#536). The id
	// key itself is util.ResponseKey's, the one spelling the writer and every
	// reader build (#539), hashed here so the month's dedupe index costs bytes,
	// not rows.
	if id, ok := r.ResponseID.(string); ok && id != "" {
		turn, _ := r.TurnID.(string)
		out.Key = hashKey(util.ResponseKey(id, r.Timestamp, turn))
	} else {
		out.Key = hashKey(r.contentKey())
	}
	switch {
	case billable(r.Usage.Cost):
		out.Cost, out.Priced = *r.Usage.Cost, true
	case billable(r.Usage.EstimatedCost):
		out.Cost, out.Priced, out.Estimated = *r.Usage.EstimatedCost, true, true
	}
	return out
}

// contentKey names a row the store carries no usable response id for by the
// receipt it wrote. Marshaling the decoded receipt gives one canonical
// spelling of every field the reader counts, so the move's two names share a
// key while any difference between two rows — turn, token bucket, price —
// keeps them apart. The prefix keeps the name out of the id keys' namespace.
func (r *receipt) contentKey() string {
	b, _ := json.Marshal(r) // a decoded receipt is plain JSON values: no error
	sum := sha256.Sum256(b)
	return "row\x00" + hex.EncodeToString(sum[:])
}

// addRow adds one row already known to be this month's and not yet counted; the
// month filter and the identity belong to the caller.
func addRow(r *spendRow, todayUnix int64, out map[string]tally) {
	t := out[r.Profile]
	todayTurn := r.When >= todayUnix
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

// mergeRows adds this month's rows to out, each response once: one a closed day
// already counted (base) or one seen earlier in this merge adds nothing. The
// rows arrive day folders first and the flat store last, so a copy left under
// both names counts where the day folder holds it (#298, #514).
func mergeRows(rows []*spendRow, monthUnix, todayUnix int64, base keyIndex, seen map[keyID]struct{}, out map[string]tally) {
	for _, r := range rows {
		if r == nil || r.When < monthUnix {
			continue
		}
		if k, ok := parseKey(r.Key); ok {
			if base.has(k) {
				continue
			}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
		}
		addRow(r, todayUnix, out)
	}
}

// tallyMemo holds one scan per process: a quota call reads every gateway
// profile, and each would otherwise re-read the whole month.
var tallyMemo struct {
	sync.Mutex
	key string
	by  map[string]tally
}

// beforeFlat runs just before a rebuild reads the flat receipts' rows, after
// the memo check has listed and sampled the day folders: the moment a move must
// not be lost in. A test arms it to land a move there (#538); it is nil in
// production.
var beforeFlat func()

// rowsDecoded, when set, is told how many receipt rows a read decoded: one per
// receipt parsed, and every row of a cache that keeps rows (a day cache, the
// flat index, a pre-#538 .open.json) when it is loaded. A test arms it to bound
// what an unchanged store costs (#538); it is nil in production.
var rowsDecoded func(n int)

func countRows(n int) {
	if rowsDecoded != nil {
		rowsDecoded(n)
	}
}

// seenFile is one receipt as it was last read, with the file state it had then.
type seenFile struct {
	Size  int64     `json:"s"`
	MTime int64     `json:"m"`
	Row   *spendRow `json:"r"`
}

// dayFile is one day folder's receipts by name: the one store a day keeps. The
// per-file index a closed day used to keep beside its rows (.open.json) is this
// map now, so a closed day no longer costs two copies of itself on disk (#538).
// MTime is the folder's mtime taken before the files were read. While it is
// unchanged and Valid, a closed day is answered without statting a receipt.
// Valid is set only by a closed read that read every receipt: an open day's
// snapshot is not trusted when the folder closes, because a writer can rewrite
// a receipt in place without changing the folder's mtime (#538).
type dayFile struct {
	V     int                 `json:"v"`
	MTime int64               `json:"mtime"`
	Valid bool                `json:"valid,omitempty"`
	Files map[string]seenFile `json:"files"`
}

// dayMark is one closed day as a read saw it.
type dayMark struct {
	Name  string `json:"n"`
	MTime int64  `json:"m"`
}

// fileMark is one receipt file's state, for deciding an unchanged store.
type fileMark struct {
	Name  string `json:"n"`
	Size  int64  `json:"s"`
	MTime int64  `json:"m"`
}

// monthMemo is the answer the last read of this store left, with the state that
// answer was read at: while every mark still matches, the tallies are returned
// without decoding a row. It holds no rows, so it costs what its marks cost and
// nothing per receipt beyond that.
type monthMemo struct {
	V     int                   `json:"v"`
	Dir   string                `json:"dir"`
	Month int64                 `json:"month"`
	Today int64                 `json:"today"`
	Days  []dayMark             `json:"days"`
	Open  map[string][]fileMark `json:"open"`
	Flat  []fileMark            `json:"flat"`
	Tally map[string]tally      `json:"tally"`
}

// monthBase is the closed days' part of a month: their tallies and the sorted
// identities they counted, kept so a day that changes costs its rows and not
// the month's. Valid while the closed-day marks are the ones it was built at.
type monthBase struct {
	V     int              `json:"v"`
	Dir   string           `json:"dir"`
	Month int64            `json:"month"`
	Days  []dayMark        `json:"days"`
	By    map[string]tally `json:"by"`
	Keys  string           `json:"keys"`
}

func spendCacheDir() string { return filepath.Join(cacheDir(), "spend") }

func dayCachePath(name string) string { return filepath.Join(spendCacheDir(), name+".json") }

func oldOpenPath(name string) string { return filepath.Join(spendCacheDir(), name+".open.json") }

func flatCachePath() string { return filepath.Join(spendCacheDir(), "flat.json") }

func monthMemoPath() string { return filepath.Join(spendCacheDir(), "month.json") }

func monthBasePath() string { return filepath.Join(spendCacheDir(), "base.json") }

func loadDayFile(name string) dayFile {
	var c dayFile
	if b, err := os.ReadFile(dayCachePath(name)); err == nil && json.Unmarshal(b, &c) == nil {
		countRows(len(c.Files))
		if c.V == spendCacheVersion && c.Files != nil {
			return c
		}
	}
	return dayFile{V: spendCacheVersion}
}

// legacyRow is spendRow's shape before the stamp was parsed and the identity
// hashed into the cache; only the migration from .open.json reads it.
type legacyRow struct {
	Profile   string  `json:"p"`
	Stamp     string  `json:"t"`
	Key       string  `json:"k,omitempty"`
	Tokens    int64   `json:"n,omitempty"`
	Cost      float64 `json:"c,omitempty"`
	Priced    bool    `json:"pr,omitempty"`
	Estimated bool    `json:"e,omitempty"`
}

func (r *legacyRow) row() *spendRow {
	if r == nil {
		return nil
	}
	ts, err := time.Parse(time.RFC3339, r.Stamp)
	if err != nil {
		return nil
	}
	out := &spendRow{Profile: r.Profile, When: ts.Unix(), Tokens: r.Tokens,
		Cost: r.Cost, Priced: r.Priced, Estimated: r.Estimated}
	if r.Key != "" {
		out.Key = hashKey(r.Key)
	}
	return out
}

// legacyCacheV is the per-file index version #536 wrote, the last shape whose
// row keys were plain strings. A pre-#536 entry has no version, and only the
// rows it gave an id key can be trusted from it; an id-less row there has no
// key and must be named by its receipt again.
const legacyCacheV = 1

type legacyFile struct {
	V     int        `json:"v"`
	Size  int64      `json:"s"`
	MTime int64      `json:"m"`
	Row   *legacyRow `json:"r"`
}

// legacyDayFiles is the per-file index a build before #538 kept for a day
// folder still open then, converted to this shape. It saves the first read after
// an upgrade a full rescan of the month.
func legacyDayFiles(name string) map[string]seenFile {
	b, err := os.ReadFile(oldOpenPath(name))
	if err != nil {
		return nil
	}
	var old map[string]legacyFile
	if json.Unmarshal(b, &old) != nil {
		return nil
	}
	countRows(len(old))
	out := make(map[string]seenFile, len(old))
	for n, f := range old {
		switch {
		case f.V == legacyCacheV && f.Row != nil:
		case f.V == 0 && f.Row != nil && f.Row.Key != "":
			// #536's predecessor gave an id-carrying row its key; that key is
			// the one this reader hashes.
		default:
			// A nil row may be a read that failed, not an absent one: read it
			// again rather than trust the miss (#538).
			continue
		}
		out[n] = seenFile{Size: f.Size, MTime: f.MTime, Row: f.Row.row()}
	}
	return out
}

// indexDay stats every receipt in a day folder and reads only the ones new or
// changed since old; it returns the map it read them at, whether anything was
// read or dropped, and whether a receipt could not be read. A file it could not
// read is left out of the map and partial is true, so neither a closed day nor
// the flat index remembers the miss as absence: the next read tries it again
// (#538). It is the one index a day folder has now, open or closed: a closed
// folder reuses it whole while its mtime and validity are unchanged, and an open
// folder consults it file by file so an in-place rewrite is a change even though
// the folder's mtime is not.
func indexDay(dir string, old map[string]seenFile) (map[string]seenFile, bool, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false, true
	}
	seen := make(map[string]seenFile, len(entries))
	changed, partial := false, false
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			changed, partial = true, true
			continue
		}
		f, ok := old[n]
		if !ok || f.Size != info.Size() || f.MTime != info.ModTime().UnixNano() {
			row, read := readRow(filepath.Join(dir, n))
			if !read {
				changed, partial = true, true
				continue // not remembered: a read that failed may pass next time
			}
			f = seenFile{Size: info.Size(), MTime: info.ModTime().UnixNano(), Row: row}
			changed = true
		}
		seen[n] = f
	}
	if len(seen) != len(old) {
		changed = true // a name the last read saw is gone
	}
	return seen, changed, partial
}

func rowsOf(files map[string]seenFile) []*spendRow {
	rows := make([]*spendRow, 0, len(files))
	for _, f := range files {
		rows = append(rows, f.Row)
	}
	return rows
}

// marksOf is a files map's state in name order, the form a memo compares.
func marksOf(files map[string]seenFile) []fileMark {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	marks := make([]fileMark, 0, len(names))
	for _, n := range names {
		f := files[n]
		marks = append(marks, fileMark{Name: n, Size: f.Size, MTime: f.MTime})
	}
	return marks
}

// closedDay is a past day folder's rows and the folder state they were read at.
// The mtime is taken before the files are read, so a row linked in during the
// read changes it and the next call reads again. Only a closed read that read
// every receipt validates that state: an open day's snapshot is indexed again
// when the day closes, so a receipt rewritten in place after the last open read
// is not kept at its old price (#538). Two caches from before #538 are read
// once: the old day cache (a row list) is ignored, and the old per-file index
// (.open.json) seeds the one index this day keeps now.
func closedDay(usageDir, name string) ([]*spendRow, dayMark, bool) {
	dir := filepath.Join(usageDir, name)
	info, err := os.Stat(dir)
	if err != nil {
		return nil, dayMark{Name: name}, false
	}
	mtime := info.ModTime().UnixNano()
	mark := dayMark{Name: name, MTime: mtime}
	c := loadDayFile(name)
	if c.Files != nil && c.Valid && c.MTime == mtime {
		return rowsOf(c.Files), mark, true
	}
	files := c.Files
	if files == nil {
		files = legacyDayFiles(name)
	}
	files, _, partial := indexDay(dir, files)
	if files == nil {
		return nil, mark, false
	}
	// A read that failed one receipt leaves the snapshot unvalidated, so the
	// next closed read stats the files again instead of trusting it.
	writeAtomic(dayCachePath(name), dayFile{V: spendCacheVersion, MTime: mtime, Valid: !partial, Files: files})
	os.Remove(oldOpenPath(name))
	return rowsOf(files), mark, !partial
}

// openDay is a day folder still being written: every receipt is statted, only
// the ones new or changed since the last read are parsed, and the ones gone
// drop out. The folder's mtime is taken before the files are read so the cache
// is ready for the day the folder closes, but the snapshot stays unvalidated:
// only a closed read may trust the folder mtime alone (#538).
func openDay(usageDir, name string) ([]*spendRow, []fileMark) {
	dir := filepath.Join(usageDir, name)
	mtime := int64(0)
	if info, err := os.Stat(dir); err == nil {
		mtime = info.ModTime().UnixNano()
	}
	c := loadDayFile(name)
	files := c.Files
	if files == nil {
		files = legacyDayFiles(name)
	}
	files, changed, _ := indexDay(dir, files)
	if files == nil {
		return nil, nil
	}
	if changed || c.Files == nil || c.MTime != mtime || c.Valid {
		writeAtomic(dayCachePath(name), dayFile{V: spendCacheVersion, MTime: mtime, Files: files})
	}
	os.Remove(oldOpenPath(name))
	return rowsOf(files), marksOf(files)
}

// flatFile is the flat store's per-file index: what the top level held at the
// last read and the state each receipt was read at.
type flatFile struct {
	V     int                 `json:"v"`
	Files map[string]seenFile `json:"files"`
}

func loadFlat() map[string]seenFile {
	var c flatFile
	if b, err := os.ReadFile(flatCachePath()); err == nil && json.Unmarshal(b, &c) == nil {
		countRows(len(c.Files))
		if c.V == spendCacheVersion && c.Files != nil {
			return c.Files
		}
	}
	return nil
}

// flatDay is the flat receipts a pre-shard store, or a move that could not
// rename, left at the top level: each read once and kept with its size and
// mtime, like a day folder's. It reads the directory its own turn, and returns
// the marks it read them at.
func flatDay(usageDir string) ([]*spendRow, []fileMark) {
	old := loadFlat()
	files, changed, _ := indexDay(usageDir, old)
	if files == nil {
		return nil, nil
	}
	if changed || old == nil {
		writeAtomic(flatCachePath(), flatFile{V: spendCacheVersion, Files: files})
	}
	return rowsOf(files), marksOf(files)
}

// dayFileMarks is a day folder's receipt states in name order, without reading
// a row: the check an unchanged store is validated against.
func dayFileMarks(dir string) ([]fileMark, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	marks := make([]fileMark, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, false
		}
		marks = append(marks, fileMark{Name: n, Size: info.Size(), MTime: info.ModTime().UnixNano()})
	}
	sort.Slice(marks, func(i, j int) bool { return marks[i].Name < marks[j].Name })
	return marks, true
}

// listDays is the month window's day folders as they are now: the closed ones
// with their folder mtime, in name order, and the open ones by name. Only a
// folder named for a day in [first, last] is opened. It is called after the
// flat store's read, never before: a move may make a folder (#538).
func listDays(usageDir, first, last, openFrom string) ([]dayMark, []string) {
	entries, _ := os.ReadDir(usageDir)
	var days []dayMark
	var open []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || name < first || name > last {
			continue
		}
		if _, err := time.Parse(dayLayout, name); err != nil {
			continue // a folder that is not a day shard
		}
		if name >= openFrom {
			open = append(open, name)
			continue
		}
		info, err := os.Stat(filepath.Join(usageDir, name))
		if err != nil {
			continue
		}
		days = append(days, dayMark{Name: name, MTime: info.ModTime().UnixNano()})
	}
	return days, open
}

// openMarks is every open day's receipt states, by folder name.
func openMarks(usageDir string, names []string) (map[string][]fileMark, bool) {
	out := make(map[string][]fileMark, len(names))
	for _, name := range names {
		marks, ok := dayFileMarks(filepath.Join(usageDir, name))
		if !ok {
			return nil, false
		}
		out[name] = marks
	}
	return out, true
}

func sameDayMarks(a, b []dayMark) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameFileMarks(a, b []fileMark) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameOpenMarks(a, b map[string][]fileMark) bool {
	if len(a) != len(b) {
		return false
	}
	for name, marks := range a {
		other, ok := b[name]
		if !ok || !sameFileMarks(marks, other) {
			return false
		}
	}
	return true
}

func memoMatches(m *monthMemo, dir string, month, today int64, days []dayMark, open map[string][]fileMark, flat []fileMark) bool {
	return m.V == spendCacheVersion && m.Dir == dir && m.Month == month && m.Today == today &&
		m.Tally != nil && sameDayMarks(m.Days, days) && sameOpenMarks(m.Open, open) &&
		sameFileMarks(m.Flat, flat)
}

// closedBase is every closed day's rows merged, with the identities they
// counted and the day states they were read at. It is rebuilt only when a
// closed day's state changed: otherwise the base file answers without opening
// a day cache. complete is false when a receipt could not be read; such a base
// is answered from but not kept, so the next read tries again (#538).
func closedBase(usageDir string, days []dayMark, monthUnix, todayUnix int64) (map[string]tally, []dayMark, string, bool) {
	var b monthBase
	if raw, err := os.ReadFile(monthBasePath()); err == nil && json.Unmarshal(raw, &b) == nil &&
		b.V == spendCacheVersion && b.Dir == usageDir && b.Month == monthUnix &&
		b.By != nil && sameDayMarks(b.Days, days) {
		return b.By, b.Days, b.Keys, true
	}
	by := map[string]tally{}
	seen := map[keyID]struct{}{}
	marks := make([]dayMark, 0, len(days))
	complete := true
	for _, d := range days {
		rows, mark, ok := closedDay(usageDir, d.Name)
		complete = complete && ok
		marks = append(marks, mark)
		for _, r := range rows {
			if r == nil || r.When < monthUnix {
				continue
			}
			if k, ok := parseKey(r.Key); ok {
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
			}
			addRow(r, todayUnix, by)
		}
	}
	keys := encodeKeyIndex(seen)
	if complete {
		writeAtomic(monthBasePath(), monthBase{V: spendCacheVersion, Dir: usageDir, Month: monthUnix,
			Days: marks, By: by, Keys: keys})
	}
	return by, marks, keys, complete
}

// sweepSpendCache retires the files the reader no longer uses. The per-file
// index every closed day used to keep beside its rows is gone, and so is a day
// cache outside the month window the reader can ask for — a past month's copies
// would otherwise hold their bytes forever (#538).
func sweepSpendCache(first, last string) {
	entries, err := os.ReadDir(spendCacheDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(n, ".open.json") {
			os.Remove(filepath.Join(spendCacheDir(), n))
			continue
		}
		if !strings.HasSuffix(n, ".json") {
			continue
		}
		day := strings.TrimSuffix(n, ".json")
		if _, err := time.Parse(dayLayout, day); err != nil {
			continue
		}
		if day < first || day > last {
			os.Remove(filepath.Join(spendCacheDir(), n))
		}
	}
}

// monthTallies is every profile's tally for the month. The receipts sit in the
// flat store and in day folders named by UTC day, compared by name — stepping
// local midnights as instants lands after now east of UTC and skips today's
// folder. A row always sits in a folder at or after its stamp's UTC day, so an
// earlier folder holds only rows the month window itself excludes; the stamp
// still decides. A folder whose whole UTC day ended before today's local start
// can hold no turn of today, so its rows are kept and reused until the folder
// changes (a late move links rows into it).
//
// The read is bounded by what changed (#538):
//   - the month memo is checked before any row is decoded: the flat receipts'
//     states are sampled first, then the day folders listed and the closed
//     ones' mtimes taken, then today's receipts statted. The check stats files
//     and decodes neither a receipt nor a cache that keeps rows;
//   - a rebuild reads the flat receipts' rows before it lists the day folders
//     again and samples their mtimes. A move relocating a row links its
//     day-folder name first (making the folder if it must) and removes the flat
//     name only after, so whichever moment between the two reads the move lands
//     in, one of the reads holds the row. The check's marks are not reused:
//     taken before the flat read, they could keep a base built before the link
//     while the flat name is already gone;
//   - a closed day is one cache (its per-file index) reused whole while the
//     folder's mtime is the one it was read at, that mtime taken before the
//     files are read, and only after a closed read validated it by reading
//     every receipt. A receipt that could not be read is never remembered as
//     absent, so the next read retries it;
//   - today's folder is statted per file, so a writer's in-place rewrite is a
//     change even though the folder's mtime is not;
//   - the flat store has the same per-file index, so an unchanged flat receipt
//     is not parsed on every run;
//   - the month's answer is kept with the marks it was read at. While every
//     mark still matches, the tallies return without decoding a row: an
//     unchanged store costs what its marks cost, not the month's size.
//
// The day folders read are the month window's; nothing outside it is opened.
func monthTallies(usageDir string, today, monthStart, todayStart time.Time) map[string]tally {
	key := usageDir + "\x00" + monthStart.String() + "\x00" + todayStart.String()
	tallyMemo.Lock()
	defer tallyMemo.Unlock()
	if tallyMemo.key == key {
		return tallyMemo.by
	}
	first, last := monthStart.UTC().Format(dayLayout), today.UTC().Format(dayLayout)
	openFrom := todayStart.UTC().Format(dayLayout)
	monthUnix, todayUnix := monthStart.Unix(), todayStart.Unix()

	// The memo check decodes no row: the flat receipts' states are sampled
	// first, then the day folders are listed and the closed ones' mtimes taken,
	// then today's receipts statted.
	flatMarks, flatOK := dayFileMarks(usageDir)
	days, openNames := listDays(usageDir, first, last, openFrom)
	openState, openOK := openMarks(usageDir, openNames)
	if flatOK && openOK {
		var m monthMemo
		if raw, err := os.ReadFile(monthMemoPath()); err == nil && json.Unmarshal(raw, &m) == nil &&
			memoMatches(&m, usageDir, monthUnix, todayUnix, days, openState, flatMarks) {
			sweepSpendCache(first, last)
			tallyMemo.key, tallyMemo.by = key, m.Tally
			return m.Tally
		}
	}

	// A changed store reads the flat rows first, then lists the day folders
	// again: the marks the check took predate the flat read, and a base kept at
	// them could predate a move's link while its flat name is already gone.
	if beforeFlat != nil {
		beforeFlat()
	}
	flatRows, flatState := flatDay(usageDir)
	days, openNames = listDays(usageDir, first, last, openFrom)
	by, baseDays, keys, complete := closedBase(usageDir, days, monthUnix, todayUnix)
	var openRows []*spendRow
	openState = make(map[string][]fileMark, len(openNames))
	for _, name := range openNames {
		rows, marks := openDay(usageDir, name)
		openRows = append(openRows, rows...)
		openState[name] = marks
	}
	out := make(map[string]tally, len(by))
	for name, t := range by {
		out[name] = t
	}
	seen := map[keyID]struct{}{}
	base := decodeKeyIndex(keys)
	mergeRows(openRows, monthUnix, todayUnix, base, seen, out)
	mergeRows(flatRows, monthUnix, todayUnix, base, seen, out)

	// A closed day that could not be read leaves this answer incomplete. It is
	// still the answer to this call, but neither cache is kept, so the next
	// read retries the receipt instead of remembering it absent (#538).
	if complete {
		writeAtomic(monthMemoPath(), monthMemo{V: spendCacheVersion, Dir: usageDir, Month: monthUnix,
			Today: todayUnix, Days: baseDays, Open: openState, Flat: flatState, Tally: out})
		tallyMemo.key, tallyMemo.by = key, out
	}
	sweepSpendCache(first, last)
	return out
}

// writeAtomic replaces path with v's JSON, or leaves it as it was.
func writeAtomic(path string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	// NoChmod: the cache keeps the temp's own mode, as this write always did.
	_ = paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".tmp-*", Data: b, NoChmod: true, Mkdir: 0o700})
}
