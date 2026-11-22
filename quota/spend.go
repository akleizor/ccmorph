package quota

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/shim"
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
// profile's own turns wrote; it never makes a request, so it costs nothing. A
// store the read could not see whole is not an empty month: the entry carries
// the reason and no spend — a row that names the store instead of answering a
// healthy zero, the truthful reason of INTENT 8 — and the run fails (#545).
// When only receipts could not be read, the entry carries the reason naming
// them beside the spend of every other receipt, the total the report and the
// export give the same store, and the run still fails (#654).
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
	tallies, err := monthTallies(filepath.Join(paths.StateDir(), "usage"), today, monthStart, todayStart)
	var gaps *unreadableReceipts
	if errors.As(err, &gaps) {
		// Only receipts could not be read: the spend is every other receipt's,
		// the total report and the export give, and the reason names the
		// receipts it leaves out, so it never reads as the whole month (#654).
		e.Error = err.Error()
		s.UnreadableReceipts = len(gaps.paths)
	} else if err != nil {
		e.Spend = nil
		if errors.Is(err, paths.ErrUsageStoreBusy) {
			// The store is held by a compaction or a relocation: refusing names
			// the reason instead of freezing the statusline behind it (INTENT 8).
			e.Error = "usage store busy — retry after compaction finishes"
		} else {
			e.Error = err.Error()
		}
		return e
	}
	t := tallies[name]
	s.TodayUSD, s.MonthUSD, s.Estimated = t.TodayUSD, t.MonthUSD, t.Estimated
	s.TodayTurns, s.MonthTurns, s.UnpricedTurns = t.TodayTurns, t.MonthTurns, t.UnpricedTurns
	s.TodayTokens, s.MonthTokens = t.TodayTokens, t.MonthTokens
	if budget != nil {
		resets := time.Date(today.Year(), today.Month()+1, 1, 0, 0, 0, 0, time.Local).Unix()
		win := Window{ID: "month", Label: "month", ResetsAt: &resets}
		// Every turn this month unpriced means the month's cost is unknown, so
		// the window carries no share — nil, not 0, the picture of no spend
		// (#301). The decision lives here, where the spend is known, and every
		// reader of the window gets the same answer (#795).
		if !s.AllUnpriced() {
			used := s.MonthUSD / *budget
			win.Used = &used
		}
		e.Windows = []Window{win}
	}
	return e
}

// spendCacheVersion is the shape of the files under quota/spend. A file from an
// earlier shape is rebuilt; the per-file index #536 kept beside a day's rows is
// migrated once, so an upgrade does not rescan the month. An index older than
// that carries no content key for an id-less row, so it is read again. Version
// 3 added the closed snapshot's validity and the retryable read: a version 2
// file may have remembered an unreadable receipt, or an open day's rewrite, as
// the day's answer, so it is rebuilt (#538). Version 4 binds archived days to
// their contents and metadata, including any live overlay. Version 5 marks an
// archived day by the archive pair's stat identity plus its leftover folder's
// receipt marks, so an unchanged pass hashes no row (#559, #654).
const spendCacheVersion = 5

// A quota read is a statusline's hot path, so the one closed day it may compact
// is held to a budget (INTENT 8, #654): a day folder holding more than
// autoCompactMaxFiles receipts, or more than autoCompactMaxBytes of them, or an
// archive and index larger than autoCompactMaxBytes, is left whole for
// `ccmorph-core usage-compact`, having cost at most the cap to find out; and no
// step of the compaction starts once autoCompactWait has passed. The read
// itself still answers.
var (
	autoCompactWait           = 500 * time.Millisecond
	autoCompactMaxFiles       = 2000
	autoCompactMaxBytes int64 = 2 << 20
)

// autoCompact is the read path's maintenance: shim.CompactUsage held to the
// budget above, with its deadline on the wall clock, never the fake one.
func autoCompact(base string, at time.Time, limit int) (int, error) {
	return shim.CompactUsageWithin(base, at, limit, shim.CompactBudget{
		Deadline: time.Now().Add(autoCompactWait),
		MaxFiles: autoCompactMaxFiles,
		MaxBytes: autoCompactMaxBytes,
	})
}

// Tests of the live-folder cache disable maintenance; archive tests use the real compactor.
var compactUsage = autoCompact

// logf is the package's one diagnostic line sink: stderr, where a statusline
// read never prints it but a person running the CLI sees it. Tests swap it.
var logf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ccmorph quota: "+format+"\n", args...)
}

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

// less is the one ordering of an identity: encodeKeyIndex sorts a cache's keys
// by it, and has searches for the first key not less than the one asked about,
// so the order a cache is written in and the membership test that reads it
// cannot disagree (#546).
func (k keyID) less(o keyID) bool { return bytes.Compare(k[:], o[:]) < 0 }

func (ix keyIndex) has(k keyID) bool {
	i := sort.Search(len(ix), func(i int) bool { return !ix[i].less(k) })
	return i < len(ix) && ix[i] == k
}

func encodeKeyIndex(seen map[keyID]struct{}) string {
	keys := make([]keyID, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
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
// unreadable JSON, incomplete, or a stamp that does not parse. A non-nil error
// is the file's own read failing, and the caller classifies it: a path that is
// gone is the move's relocation, not a receipt that could not be read, while
// any other failure may pass, so the caller must not remember the miss (#538,
// #552).
func readRow(path string) (*spendRow, error) {
	raw, err := paths.ReadRegularFile(path) // never waits on a FIFO under a receipt's name (#654)
	if err != nil {
		return nil, err
	}
	return decodeRow(raw), nil
}

func decodeRow(raw []byte) *spendRow {
	countRows(1)
	var r receipt
	// A NaN/Infinity token is a price no writer stores, but the row around it
	// is a real turn: the token reads as null, the row counts unpriced, and
	// billable still refuses it — the reading report gives the same row
	// ("unknown spend", never a skip) and the export gives it ("unpriced") (#654).
	if json.Unmarshal(paths.NonFiniteToNull(raw), &r) != nil || !r.Complete {
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

// errUsageStoreUnreadable is the month's refusal when a surface of the store
// could not be read whole (#545): the view says so and carries no spend,
// rather than answering a healthy zero.
var errUsageStoreUnreadable = errors.New("usage store not readable — cost unknown; check permissions")

// unreadableReceipts is a month whose every surface could be read and only
// some receipts could not: the tallies beside it are every other receipt's,
// the total the report and the export give the same store (#654), and paths
// names each receipt the read skipped. The run still fails (#545).
type unreadableReceipts struct{ paths []string }

func (u *unreadableReceipts) Error() string {
	n := len(u.paths)
	more := ""
	if n > 1 {
		more = fmt.Sprintf(" and %d more", n-1)
	}
	return fmt.Sprintf("usage store not readable — %d %s skipped, cost unknown; check permissions: %s%s",
		n, plural(int64(n), "receipt"), u.paths[0], more)
}

// receiptGapMonth reads a month the cached read could not see whole through the
// report's own reader (shim.ReadUsageWindow), so the receipts it skips and the
// total of the rest are the ones report and share/usage-export give (#654):
// skipped names each receipt still there that could not be read. ok is false
// when a surface could not be read, which stays the refusal (#545). Nothing it
// reads is cached: the next read tries the skipped receipts again (#538).
func receiptGapMonth(usageDir, first, last string, monthUnix, todayUnix int64) (out map[string]tally, skipped []string, ok bool) {
	raw, err := shim.ReadUsageWindow(usageDir, first, last, func(path string, _ error) {
		skipped = append(skipped, path)
	})
	if err != nil {
		return nil, nil, false
	}
	rows := make([]*spendRow, 0, len(raw))
	for _, data := range raw {
		rows = append(rows, decodeRow(data))
	}
	out = map[string]tally{}
	mergeRows(rows, monthUnix, todayUnix, nil, map[keyID]struct{}{}, out)
	return out, skipped, true
}

// monthFlight is one in-flight month read the profiles of a quota call share:
// every caller waits on done and reads the leader's verdict, so a statusline
// pays one scan and the memo's mutex is held for no I/O (INTENT 8).
type monthFlight struct {
	done chan struct{}
	by   map[string]tally
	err  error
}

// tallyMemo holds one scan per process: a quota call reads every gateway
// profile, and each would otherwise re-read the whole month. ok is the read's
// verdict: false when any surface of the store could not be read (#545).
var tallyMemo struct {
	sync.Mutex
	key      string
	ok       bool
	state    *monthMemo
	inflight map[string]*monthFlight
}

// beforeFlat runs just before a rebuild reads the flat receipts' rows, after
// the memo check has listed and sampled the day folders: the moment a move must
// not be lost in. A test arms it to land a move there (#538); it is nil in
// production.
var beforeFlat func()

// receiptsListed runs just after a folder's receipts were listed and before
// the listing stats them: the earliest moment a move's remove can land between
// the two. A test arms it to land a move there (#552); it is nil in production.
var receiptsListed func(dir string)

// receiptsMarked runs after a rebuild's listing took a folder's marks — every
// receipt named and statted — and before it reads their rows: the moment a
// move's remove lands between the listing and the reads. A test arms it to
// land a move there (#552); it is nil in production.
var receiptsMarked func(dir string)

// rowsDecoded, when set, is told how many receipt rows a read decoded: one per
// receipt parsed, and every row of a cache that keeps rows (a day cache, the
// flat index, a pre-#538 .open.json) when it is loaded. A test arms it to bound
// what an unchanged store costs (#538); it is nil in production.
var rowsDecoded func(n int)

// daysSwept, when set, runs each time a read asks the month window's closed
// days whether their folders and receipts can be read — daysReadable, the sweep
// that costs a listing and an access(2) per receipt. A test counts it, so a
// read pays one sweep and not one per cache layer (#559); it is nil in
// production.
var daysSwept func()

func countRows(n int) {
	if rowsDecoded != nil {
		rowsDecoded(n)
	}
}

// archiveReads, when set, counts the archive generations this pass loaded from
// disk — never a memo hit. A test arms it to bound what a pass pays: at most
// one load per archived day on a cold pass, none on an unchanged one (#559,
// #654). It is nil in production, where paths.UsageArchiveLoaded stays nil.
var archiveReads func()

// seenFile is one receipt as it was last read, with the file state it had then.
type seenFile struct {
	Size    int64     `json:"s"`
	MTime   int64     `json:"m"`
	Dev     uint64    `json:"dv,omitempty"`
	Ino     uint64    `json:"i,omitempty"`
	CTime   int64     `json:"c,omitempty"`
	Content string    `json:"d,omitempty"`
	Row     *spendRow `json:"r"`
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
	V       int                 `json:"v"`
	MTime   int64               `json:"mtime"`
	Archive string              `json:"archive,omitempty"`
	Valid   bool                `json:"valid,omitempty"`
	Files   map[string]seenFile `json:"files"`
}

// dayMark binds a closed day to its folder mtime and verified archive/overlay content.
type dayMark struct {
	Name    string `json:"n"`
	MTime   int64  `json:"m"`
	Archive string `json:"a,omitempty"`
}

// fileMark is one receipt file's state, for deciding an unchanged store.
type fileMark struct {
	Name    string `json:"n"`
	Size    int64  `json:"s"`
	MTime   int64  `json:"m"`
	Content string `json:"d,omitempty"`
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

// spendCache is a cache file under quota/spend: what decoding it costs, in
// receipt rows, and the version its decoded body carries.
type spendCache interface {
	rows() int
	version() int
}

func (c dayFile) rows() int  { return len(c.Files) }
func (c flatFile) rows() int { return len(c.Files) }

// The month's own caches keep tallies, not rows: they decode for free.
func (c monthBase) rows() int { return 0 }
func (c monthMemo) rows() int { return 0 }

func (c dayFile) version() int   { return c.V }
func (c flatFile) version() int  { return c.V }
func (c monthBase) version() int { return c.V }
func (c monthMemo) version() int { return c.V }

// cacheVersion is the head's "v" alone. The scan reads the tokens one object
// key at a time and skips each value it does not ask about whole, so it stops
// at the version — the first key every cache this writer produces — and never
// approaches the body's cost: an accepted cache is walked once, by the full
// decode that follows, where the head's own decode used to walk it a second
// time (#559), and a whole file of a future version's body is walked never
// (#449). Its answer fast-rejects only: what it accepts, loadCache still
// decides by the version the body decoded as (#581). ok is false for anything
// but a whole number under "v" at the object's top level.
func cacheVersion(b []byte) (int, bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return 0, false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, false
		}
		val, err := dec.Token()
		if err != nil {
			return 0, false
		}
		if _, nested := val.(json.Delim); nested {
			// A value this scan does not read — a day's files, a month's marks —
			// is skipped to its matching close, so the body stays unread.
			for depth := 1; depth > 0; {
				t, err := dec.Token()
				if err != nil {
					return 0, false
				}
				switch t {
				case json.Delim('{'), json.Delim('['):
					depth++
				case json.Delim('}'), json.Delim(']'):
					depth--
				}
			}
			continue
		}
		if key == "v" {
			switch n := val.(type) {
			case int64:
				return int(n), true
			case float64:
				if v := int64(n); float64(v) == n {
					return int(v), true
				}
			}
			return 0, false // a fraction, or a number no int holds
		}
	}
	return 0, false // no version: not a file this reader's writer produced
}

// loadCache is the one preamble every spend cache read goes through: read the
// file, read its version alone, and only for this shape's version decode the
// body and count the receipts it keeps, so a whole file of a future version
// never reaches the decode (#449). The peek may only fast-reject: it answers
// the first top-level "v" and the decoder the last, and the decoder does not
// care which case spelled the key, so bytes the two read differently are
// decided by the version that decoded — a body that decoded as another
// version's is refused as if it had never decoded, and contributes nothing
// (#581, restoring #449's guarantee the peek alone could not hold). Any miss —
// no file, bytes that do not decode, a version this reader did not write — is
// the zero cache, never a partial one. What else makes a decoded cache this
// reader's answer — a day's marks, a month's window — is the caller's to check
// (#546).
func loadCache[T spendCache](path string) (T, bool) {
	var zero T
	b, err := os.ReadFile(path)
	if err != nil {
		return zero, false
	}
	if v, ok := cacheVersion(b); !ok || v != spendCacheVersion {
		return zero, false
	}
	var c T
	if json.Unmarshal(b, &c) != nil {
		return zero, false
	}
	if c.version() != spendCacheVersion {
		return zero, false
	}
	countRows(c.rows())
	return c, true
}

func loadDayFile(name string) dayFile {
	if c, ok := loadCache[dayFile](dayCachePath(name)); ok && c.Files != nil {
		return c
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

// accessRead and accessSearch are access(2)'s R_OK and X_OK, the same on darwin
// and linux; package syscall names neither.
const (
	accessRead   = 0x4
	accessSearch = 0x1
)

// canRead is whether this process may read path now — a folder also searched,
// so the receipts it lists can be reached. It opens and decodes nothing: a mode
// change moves no size or mtime the caches compare, so it is asked on its own
// (#545). A path that is gone is not an unreadable one: a move relocates rows,
// and the listing's own checks own a vanished name.
func canRead(path string, mode uint32) bool {
	err := syscall.Access(path, mode)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// dirEntries is a directory's listing, with a directory that is not there
// counted as empty: a store nothing has written yet is an empty store, not an
// unreadable one. ok is false only when the listing itself could not be read,
// which is a surface the month must not answer as absence (#545). A folder that
// lists but cannot be searched (mode 0400) names receipts no read can reach,
// and is not readable either.
func dirEntries(dir string) ([]os.DirEntry, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, true
		}
		return nil, false
	}
	if !canRead(dir, accessRead|accessSearch) {
		return nil, false
	}
	return entries, true
}

// receiptMarks is a day folder's receipts with the state a cache compares, the
// one listing every read of a day's files takes (#546): each receipt's name,
// size and mtime, and whether the whole folder could be read — a folder that
// lists but cannot be searched reaches no receipt it names, and one that could
// not be listed is not a day this run can read (#545). A file the listing could
// not stat or this run cannot read is left out and partial is true, so no cache
// remembers the miss as absence: the next read tries it again (#538, #545). A
// name that left between the listing and its stat is the move's remove — which
// the writer's own side tolerates — not an unreadable file: it is skipped
// whole, the way canRead skips a path that is gone (#552). A folder that is
// not there holds no receipts (marks nil, ok): a store nothing has written yet
// is an empty store, not an unreadable one.
func receiptMarks(dir string) (marks []fileMark, partial, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, true
		}
		return nil, false, false
	}
	if !canRead(dir, accessRead|accessSearch) {
		return nil, false, false
	}
	if receiptsListed != nil {
		receiptsListed(dir)
	}
	marks = make([]fileMark, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // the move took this name between the listing and the stat
			}
			partial = true
			continue
		}
		if !canRead(filepath.Join(dir, n), accessRead) {
			partial = true
			continue
		}
		marks = append(marks, fileMark{Name: n, Size: info.Size(), MTime: info.ModTime().UnixNano()})
	}
	return marks, partial, true
}

// indexDay stats every receipt in a day folder and reads only the ones new or
// changed since old; it returns the map it read them at, whether anything was
// read or dropped, and whether a receipt could not be read. A file it could not
// read is left out of the map and partial is true, so neither a closed day nor
// the flat index remembers the miss as absence: the next read tries it again
// (#538). A receipt that left between the listing and this read is gone, not
// unreadable — the move's link-then-remove, whose remove the writer's own side
// tolerates — so it is skipped without partial and without a cached absence,
// and the day name its link made is a later surface of this read's to count
// (#552). ok is false when the folder itself could not be listed (#545): the
// map is then no map at all, and the day's rows are not the day's whole truth.
// Archives are verified and indexed under their original receipt names; their
// content keys detect rewrites even when size and mtime stay unchanged.
// It is the one index a day folder has now, open or closed: a closed
// folder reuses it whole while its mtime and validity are unchanged, and an open
// folder consults it file by file so an in-place rewrite is a change even though
// the folder's mtime is not.
func indexDay(dir string, old map[string]seenFile) (map[string]seenFile, bool, bool, bool) {
	rows, err := archivedDayRows(dir)
	if err != nil {
		return nil, false, true, false
	}
	if rows == nil {
		return indexFolder(dir, old)
	}
	files := make(map[string]seenFile, len(rows))
	for _, r := range rows {
		content := hashKey(string(r.Data))
		f, kept := old[r.Name]
		if !kept || f.Size != int64(len(r.Data)) || f.MTime != r.MTimeNS || f.Content != content {
			f = seenFile{Size: int64(len(r.Data)), MTime: r.MTimeNS, Content: content, Row: decodeRow(r.Data)}
		}
		files[r.Name] = f
	}
	return files, !sameFileMarks(marksOf(files), marksOf(old)), false, true
}

// archivedDayRows validates the archive even on cache hits, then merges live overlays.
func archivedDayRows(dir string) ([]paths.UsageArchiveRow, error) {
	archived, err := paths.ReadUsageArchive(dir + ".jsonl")
	if err != nil || archived == nil {
		return nil, err
	}
	marks, partial, ok := receiptMarks(dir)
	if !ok {
		// The hash-verified archive answers the day on its own. A leftover
		// folder this run cannot list is the compaction's unfinished unlink
		// meeting foreign ownership — a state the writers create — and the
		// archive holds every row it ever published. One reader answering
		// while another fails the month is the split-brain this store must
		// not have: report and export answer from the archive, so quota does
		// too. A partially readable folder is different: an unreadable overlay
		// row may be newer than any generation, and stays a named refusal.
		return archived, nil
	}
	if partial {
		return nil, fmt.Errorf("%s: live receipts not readable", dir)
	}
	if len(marks) == 0 {
		return archived, nil
	}
	rows := make(map[string]paths.UsageArchiveRow, len(archived)+len(marks))
	for _, r := range archived {
		rows[r.Name] = r
	}
	for _, m := range marks {
		data, err := paths.ReadRegularFile(filepath.Join(dir, m.Name))
		if err != nil {
			return nil, err
		}
		if old, kept := rows[m.Name]; kept {
			if !sameNamedReceipt(old.Data, data) {
				// The archive name is internal; conflicting identities remain two turns.
				rows[m.Name+paths.ArchiveCopySuffix] = old
			} else if old.MTimeNS > m.MTime {
				continue
			}
		}
		rows[m.Name] = paths.UsageArchiveRow{Name: m.Name, MTimeNS: m.MTime, Data: data}
	}
	names := make([]string, 0, len(rows))
	for name := range rows {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]paths.UsageArchiveRow, 0, len(names))
	for _, name := range names {
		r := rows[name]
		r.Name = name
		out = append(out, r)
	}
	return out, nil
}

// sameNamedReceipt is util/paths.SameNamedRows — the one merge rule every
// reader of the store shares. This reader used to decode the pair with
// encoding/json directly, which refuses the NaN/Infinity tokens outright: the
// same damaged row merged to newest by report and the export was kept as two
// turns here, then decoded to nil, so the month quietly lost the turn (#654).
func sameNamedReceipt(a, b []byte) bool {
	return paths.SameNamedRows(a, b)
}

func archiveFileMarks(rows []paths.UsageArchiveRow) []fileMark {
	marks := make([]fileMark, 0, len(rows))
	for _, r := range rows {
		marks = append(marks, fileMark{Name: r.Name, Size: int64(len(r.Data)), MTime: r.MTimeNS, Content: hashKey(string(r.Data))})
	}
	return marks
}

// dayState is a day's state as an unchanged pass pays it: the archive pair's
// stat identity plus its leftover folder's receipt marks, with no row read and
// no hash. The archive's identity moves on any rewrite (its inode or ctime),
// and an overlay that appears or grows moves the folder's marks, so a memo
// built at these marks is the answer while the day is unchanged (#559, #654).
// A partially readable leftover folder is a named refusal, as in the read; one
// that cannot be listed at all is answered from the hash-verified archive.
func dayState(dir, name string) (dayMark, bool) {
	mark := dayMark{Name: name}
	info, err := os.Stat(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return mark, false
	}
	if info != nil {
		mark.MTime = info.ModTime().UnixNano()
	}
	if stamp, ok := paths.UsageArchiveStamp(dir + ".jsonl"); ok {
		fingerprint, partial, folderOK := folderMarks(dir)
		if folderOK && partial {
			return mark, false
		}
		mark.Archive = stamp.String()
		if folderOK {
			mark.Archive += "+" + fingerprint
		} else {
			mark.Archive += "+unlisted"
		}
	} else if info == nil {
		return mark, false
	}
	return mark, true
}

// folderMarks fingerprints a leftover folder's receipts for an archived day's
// mark: name, size, mtime and the inode's identity. The identity is what the
// day's own mark needs and the shared fileMark deliberately is not: a folder
// removed and recreated with the same names, sizes and mtimes must move the
// mark, or the memo serves the old totals (#635). partial is a receipt this
// run cannot see; ok is false only when the folder cannot be listed, the state
// the archive alone answers (#654).
func folderMarks(dir string) (fingerprint string, partial, ok bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return marksFingerprint(nil), false, true
		}
		return "", false, false
	}
	if !canRead(dir, accessRead|accessSearch) {
		return "", false, false
	}
	marks := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			partial = true
			continue
		}
		if !canRead(filepath.Join(dir, n), accessRead) {
			partial = true
			continue
		}
		stamp, _ := paths.UsageFileStamp(info)
		marks = append(marks, fmt.Sprintf("%s\x00%d\x00%d\x00%x\x00%x\x00%d",
			n, info.Size(), info.ModTime().UnixNano(), stamp.Dev, stamp.Ino, stamp.CTime))
	}
	return hashKey(strings.Join(marks, "\n")), partial, true
}

// marksFingerprint is a folder's receipt marks as one cache token: names and
// the size and mtime a writer moves, never a receipt's bytes.
func marksFingerprint(marks []fileMark) string {
	b, _ := json.Marshal(marks)
	return hashKey(string(b))
}

func indexFolder(dir string, old map[string]seenFile) (map[string]seenFile, bool, bool, bool) {
	marks, partial, ok := receiptMarks(dir)
	if !ok {
		return nil, false, true, false
	}
	if receiptsMarked != nil {
		receiptsMarked(dir)
	}
	seen := make(map[string]seenFile, len(marks))
	changed := partial // a file the listing could not stat or read is already a change
	for _, m := range marks {
		f, kept := old[m.Name]
		if !kept || f.Size != m.Size || f.MTime != m.MTime || f.Content != "" {
			row, err := readRow(filepath.Join(dir, m.Name))
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// The move took this name between the listing and the read:
					// gone, not unreadable, and not remembered either — the
					// name's return is read again.
					continue
				}
				changed, partial = true, true
				continue // not remembered: a read that failed may pass next time
			}
			f = seenFile{Size: m.Size, MTime: m.MTime, Row: row}
			changed = true
		}
		seen[m.Name] = f
	}
	if len(seen) != len(old) {
		changed = true // a name the last read saw is gone
	}
	return seen, changed, partial, true
}

// rowsOf is a files map's rows in receipt-name order, the order the folder read
// them in: a map range changes run to run, and the rows' order must not (#449).
func rowsOf(files map[string]seenFile) []*spendRow {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	rows := make([]*spendRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, files[n].Row)
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
		marks = append(marks, fileMark{Name: n, Size: f.Size, MTime: f.MTime, Content: f.Content})
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
	mark, ok := dayState(dir, name)
	if !ok {
		return nil, mark, false
	}
	mtime := mark.MTime
	c := loadDayFile(name)
	if c.Files != nil && c.Valid && c.MTime == mtime && c.Archive == mark.Archive {
		return rowsOf(c.Files), mark, true
	}
	files := c.Files
	if files == nil {
		files = legacyDayFiles(name)
	}
	files, _, partial, ok := indexDay(dir, files)
	if !ok {
		return nil, mark, false
	}
	// A read that failed one receipt leaves the snapshot unvalidated, so the
	// next closed read stats the files again instead of trusting it.
	writeAtomic(dayCachePath(name), dayFile{V: spendCacheVersion, MTime: mtime, Archive: mark.Archive, Valid: !partial, Files: files})
	os.Remove(oldOpenPath(name))
	return rowsOf(files), mark, !partial
}

// openDay is a day folder still being written: every receipt is statted, only
// the ones new or changed since the last read are parsed, and the ones gone
// drop out. The folder's mtime is taken before the files are read so the cache
// is ready for the day the folder closes, but the snapshot stays unvalidated:
// only a closed read may trust the folder mtime alone (#538). ok is false when
// the folder or a receipt in it could not be read (#545).
func openDay(usageDir, name string) ([]*spendRow, []fileMark, bool) {
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
	files, changed, partial, ok := indexDay(dir, files)
	if !ok {
		return nil, nil, false
	}
	if changed || c.Files == nil || c.MTime != mtime || c.Valid {
		writeAtomic(dayCachePath(name), dayFile{V: spendCacheVersion, MTime: mtime, Files: files})
	}
	os.Remove(oldOpenPath(name))
	return rowsOf(files), marksOf(files), !partial
}

// flatFile is the flat store's per-file index: what the top level held at the
// last read and the state each receipt was read at.
type flatFile struct {
	V     int                 `json:"v"`
	Files map[string]seenFile `json:"files"`
}

func loadFlat() map[string]seenFile {
	if c, ok := loadCache[flatFile](flatCachePath()); ok && c.Files != nil {
		return c.Files
	}
	return nil
}

// flatDay is the flat receipts a pre-shard store, or a move that could not
// rename, left at the top level: each read once and kept with its size and
// mtime, like a day folder's. It reads the directory its own turn, and returns
// the marks it read them at. ok is false when the directory or a receipt in it
// could not be read (#545).
func flatDay(usageDir string) ([]*spendRow, []fileMark, bool) {
	old := loadFlat()
	files, changed, partial, ok := indexFolder(usageDir, old)
	if !ok {
		return nil, nil, false
	}
	if changed || old == nil {
		writeAtomic(flatCachePath(), flatFile{V: spendCacheVersion, Files: files})
	}
	return rowsOf(files), marksOf(files), !partial
}

// dayFileMarks is a day folder's receipt states in name order, without reading
// a row: the check an unchanged store is validated against. It is receiptMarks
// with a stricter verdict — a file the listing could not stat or this run
// cannot read fails the whole check, so the memo never validates a store it
// cannot see whole, and a folder that is not there fails it too, so the check
// never validates a store that is not there at all (#545). ReadDir returns its
// entries in name order, so the marks need no sort.
func dayFileMarks(dir string) ([]fileMark, bool) {
	if paths.IsDayName(filepath.Base(dir)) {
		rows, err := archivedDayRows(dir)
		if err != nil {
			return nil, false
		}
		if rows != nil {
			return archiveFileMarks(rows), true
		}
	}
	marks, partial, ok := receiptMarks(dir)
	if !ok || partial || marks == nil {
		return nil, false
	}
	return marks, true
}

// receiptsReadable is whether a closed day's folder and every receipt in it
// can be read now. A closed day is validated by its folder's mtime alone, and a
// permission change moves no mtime: without this, the memo, the base and the
// day cache would keep answering a day the run cannot read (#545).
//
// An archived day is checked without loading it: its archive pair's identity is
// a stat, and the leftover folder's receipts are checked for reachability with
// the same marks the read takes. An unlistable leftover folder is answered from
// the hash-verified archive (the compaction's unfinished unlink); a partially
// readable one is a named refusal, as in the read (#654).
func receiptsReadable(dir string) bool {
	if paths.IsDayName(filepath.Base(dir)) {
		if _, ok := paths.UsageArchiveStamp(dir + ".jsonl"); ok {
			_, partial, folderOK := receiptMarks(dir)
			if !folderOK {
				return true
			}
			return !partial
		}
	}
	entries, ok := dirEntries(dir)
	if !ok {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || filepath.Ext(n) != ".json" {
			continue
		}
		if !canRead(filepath.Join(dir, n), accessRead) {
			return false
		}
	}
	return true
}

// dayMarks lists the month's folder and archive days once, in name order.
// Folder-only days retain the mtime-only check and deferred readability sweep;
// archive days validate their generation and overlay content before any cache hit.
// An idx-only day is included so an incomplete archive cannot look like absence.
// ok is false when the listing or a listed day's state could not be read whole.
func dayMarks(usageDir, first, last, openFrom string) ([]dayMark, []string, bool) {
	entries, ok := dirEntries(usageDir)
	if !ok {
		return nil, nil, false
	}
	names := map[string]struct{}{}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			switch {
			case strings.HasSuffix(name, ".jsonl.idx"):
				name = strings.TrimSuffix(name, ".jsonl.idx")
			case strings.HasSuffix(name, ".jsonl"):
				name = strings.TrimSuffix(name, ".jsonl")
			default:
				continue
			}
		}
		if name >= first && name <= last && paths.IsDayName(name) {
			names[name] = struct{}{}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	var days []dayMark
	var open []string
	for _, name := range ordered {
		if name >= openFrom {
			open = append(open, name)
			continue
		}
		mark, readable := dayState(filepath.Join(usageDir, name), name)
		if !readable {
			ok = false
		}
		days = append(days, mark)
	}
	return days, open, ok
}

// daysReadable is whether every closed day's folder and every receipt in it can
// be read now: the sweep. A closed day is validated by its folder's mtime alone,
// so a folder that went unreadable without changing it — permissions, not
// content — would keep answering from cache: mode 0400 lists but reaches
// nothing, and a receipt at mode 0 is cached but not readable (#545). It is
// asked after the marks it vouches for and never replaced by a stat: a mode
// moves no mtime.
func daysReadable(usageDir string, days []dayMark) bool {
	if daysSwept != nil {
		daysSwept()
	}
	for _, d := range days {
		if !receiptsReadable(filepath.Join(usageDir, d.Name)) {
			return false
		}
	}
	return true
}

// listDays is dayMarks with the sweep: the window's day folders, and whether
// every closed one can be read now. A rebuild takes it after the flat store's
// read, never before: a move may make a folder (#538). ok is false when
// dayMarks' is, or a closed day cannot be read whole: the month is then
// answered but never remembered (#545).
func listDays(usageDir, first, last, openFrom string) ([]dayMark, []string, bool) {
	days, open, ok := dayMarks(usageDir, first, last, openFrom)
	if !daysReadable(usageDir, days) {
		ok = false
	}
	return days, open, ok
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

// memoMatches is what, past the shape every cache read shares, makes a month
// memo this reader's answer: the store, the window, and every mark the answer
// was read at. The version it was written as is loadCache's to check;
// everything else is here (#546).
func memoMatches(m *monthMemo, dir string, month, today int64, days []dayMark, open map[string][]fileMark, flat []fileMark) bool {
	return m.Dir == dir && m.Month == month && m.Today == today &&
		m.Tally != nil && sameDayMarks(m.Days, days) && sameOpenMarks(m.Open, open) &&
		sameFileMarks(m.Flat, flat)
}

// closedBase is every closed day's rows merged, with the identities they
// counted and the day states they were read at. It is rebuilt only when a
// closed day's state changed: otherwise the base file answers without opening
// a day cache. listed is false when the day folders could not be listed whole
// (#545): the base is then neither answered nor kept, the way a receipt that
// could not be read is not. readOK is the verdict of every read outside the
// closed days — the flat store's and the open days': the base is kept only for
// a month read whole (#545). complete is false when a receipt could not be
// read; such a base is answered from but not kept, so the next read tries
// again (#538).
func closedBase(usageDir string, days []dayMark, listed, readOK bool, monthUnix, todayUnix int64) (map[string]tally, []dayMark, string, bool) {
	if c, ok := loadCache[monthBase](monthBasePath()); ok && listed && c.Dir == usageDir && c.Month == monthUnix &&
		c.By != nil && sameDayMarks(c.Days, days) {
		return c.By, c.Days, c.Keys, readOK
	}
	by := map[string]tally{}
	seen := map[keyID]struct{}{}
	marks := make([]dayMark, 0, len(days))
	complete := listed && readOK
	for _, d := range days {
		rows, mark, ok := closedDay(usageDir, d.Name)
		complete = complete && ok
		marks = append(marks, mark)
		// No base: the identities the closed days count are what this pass is
		// building, so only a row two closed days both hold is a duplicate.
		mergeRows(rows, monthUnix, todayUnix, nil, seen, by)
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
		if !paths.IsDayName(day) {
			continue // a cache for a folder that is not a day shard
		}
		if day < first || day > last {
			os.Remove(filepath.Join(spendCacheDir(), n))
		}
	}
}

// monthTallies is every profile's tally for the month. The receipts sit in the
// flat store and in day folders or archives named by UTC day, compared by name — stepping
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
//     ones' mtimes taken, then today's receipts statted; only a memo those
//     marks match opens each closed day to ask that it can be read now, so a
//     changed store pays that sweep once, in the rebuild (#559). The check
//     stats files and decodes neither a receipt nor a cache that keeps rows;
//   - a rebuild reads the flat receipts' rows before it lists the day folders
//     again and samples their mtimes. A move relocating a row links its
//     day-folder name first (making the folder if it must) and removes the flat
//     name only after, so whichever moment between the two reads the move lands
//     in, one of the reads holds the row. The check's marks are not reused:
//     taken before the flat read, they could keep a base built before the link
//     while the flat name is already gone. The closed days' base is built after
//     the open days are read, its write gate the whole month's read verdict
//     (#545);
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
// The error is the read's verdict: non-nil when any surface of the store — its
// listing, a day folder, the flat index, today's folder — could not be read
// (#545), and paths.ErrUsageStoreBusy when a compaction's exclusive hold
// outlasted the lock's bound. The tallies returned are still the answer to
// this call; the caller is the one that must not answer them as a healthy zero.
func monthTallies(usageDir string, today, monthStart, todayStart time.Time) (map[string]tally, error) {
	key := usageDir + "\x00" + monthStart.String() + "\x00" + todayStart.String()
	// One scan per month at a time, shared by every profile of a quota call:
	// the memo's mutex guards this struct and the in-flight map only, never the
	// store's I/O, so a read waiting on the lock can still refuse a busy store
	// (INTENT 8, #654).
	tallyMemo.Lock()
	if f := tallyMemo.inflight[key]; f != nil {
		tallyMemo.Unlock()
		<-f.done
		return f.by, f.err
	}
	f := &monthFlight{done: make(chan struct{})}
	if tallyMemo.inflight == nil {
		tallyMemo.inflight = map[string]*monthFlight{}
	}
	tallyMemo.inflight[key] = f
	maintain := tallyMemo.key != key
	tallyMemo.Unlock()
	by, err := monthRead(key, maintain, usageDir, today, monthStart, todayStart)
	tallyMemo.Lock()
	f.by, f.err = by, err
	delete(tallyMemo.inflight, key)
	close(f.done)
	tallyMemo.Unlock()
	return by, err
}

// monthRead is the one scan a month's callers share, from maintenance to the
// memo it leaves for the next pass.
func monthRead(key string, maintain bool, usageDir string, today, monthStart, todayStart time.Time) (map[string]tally, error) {
	// A test counter observes every disk load this pass makes, and nothing
	// when it is not armed: production leaves util/paths' hook nil.
	if archiveReads != nil {
		loaded := paths.UsageArchiveLoaded
		paths.UsageArchiveLoaded = func(string) { archiveReads() }
		defer func() { paths.UsageArchiveLoaded = loaded }()
	}
	if maintain {
		// Maintenance is not silent: a day the compactor refused is logged once
		// per pass rather than swallowed with the count.
		if _, cerr := compactUsage(usageDir, today, 1); cerr != nil {
			logf("usage maintenance: %v", cerr)
		}
	}
	// A statusline that queued behind a compaction's multi-minute exclusive
	// hold would be the hang INTENT 8 forbids: the store's bound answers with
	// a named refusal instead.
	lock, err := paths.UsageStoreLockWithin(usageDir, false, paths.UsageLockWait)
	if errors.Is(err, paths.ErrUsageStoreBusy) {
		return nil, err
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errUsageStoreUnreadable
	}
	if lock != nil {
		defer lock.Close()
	}
	first, last := monthStart.UTC().Format(paths.DayLayout), today.UTC().Format(paths.DayLayout)
	// The open/closed boundary is util/paths.UsageOpenFrom's, computed from the
	// same instant handed to the compactor above: a day this read calls open is
	// a day the walk will not fold, whatever the local calendar says.
	openFrom := paths.UsageOpenFrom(today)
	monthUnix, todayUnix := monthStart.Unix(), todayStart.Unix()

	// The memo check decodes no row and sweeps nothing it may not need: the
	// flat receipts' states are sampled first, then the day folders are listed
	// and the closed ones' mtimes taken, then today's receipts statted. Only a
	// memo those marks match asks the closed days whether they can be read now
	// (#545); a store that changed pays its one sweep in the rebuild (#559).
	flatMarks, flatOK := dayFileMarks(usageDir)
	days, openNames, daysOK := dayMarks(usageDir, first, last, openFrom)
	openState, openOK := openMarks(usageDir, openNames)
	tallyMemo.Lock()
	cachedKey, cachedOK, cached := tallyMemo.key, tallyMemo.ok, tallyMemo.state
	tallyMemo.Unlock()
	if flatOK && openOK && daysOK {
		if cachedKey == key && cachedOK && cached != nil &&
			memoMatches(cached, usageDir, monthUnix, todayUnix, days, openState, flatMarks) &&
			daysReadable(usageDir, days) {
			return cached.Tally, nil
		}
		if m, ok := loadCache[monthMemo](monthMemoPath()); ok &&
			memoMatches(&m, usageDir, monthUnix, todayUnix, days, openState, flatMarks) &&
			daysReadable(usageDir, days) {
			sweepSpendCache(first, last)
			tallyMemo.Lock()
			tallyMemo.key, tallyMemo.ok, tallyMemo.state = key, true, &m
			tallyMemo.Unlock()
			return m.Tally, nil
		}
	}

	// A changed store reads the flat rows first, then lists the day folders
	// again: the marks the check took predate the flat read, and a base kept at
	// them could predate a move's link while its flat name is already gone
	// (#538). This listing is the read's one readability sweep: the check asked
	// none unless its memo matched, so a permission change that lands before the
	// flat read is still seen here, under every cache layer (#545, #559).
	if beforeFlat != nil {
		beforeFlat()
	}
	flatRows, flatState, flatOK := flatDay(usageDir)
	days, openNames, daysOK = listDays(usageDir, first, last, openFrom)
	var openRows []*spendRow
	openState = make(map[string][]fileMark, len(openNames))
	openRead := true
	for _, name := range openNames {
		rows, marks, ok := openDay(usageDir, name)
		openRows = append(openRows, rows...)
		openState[name] = marks
		openRead = openRead && ok
	}
	// The base's write gate is the whole month's read verdict, so it is asked
	// for only after every other read has spoken: a base kept for a month whose
	// flat or open part failed is the same remembered absence (#545).
	by, baseDays, keys, complete := closedBase(usageDir, days, daysOK, flatOK && openRead, monthUnix, todayUnix)
	out := make(map[string]tally, len(by))
	for name, t := range by {
		out[name] = t
	}
	seen := map[keyID]struct{}{}
	base := decodeKeyIndex(keys)
	mergeRows(openRows, monthUnix, todayUnix, base, seen, out)
	mergeRows(flatRows, monthUnix, todayUnix, base, seen, out)

	// A closed day that could not be read leaves this answer incomplete (#538),
	// and so does a surface the read could not see: the store's listing, a day
	// folder, the flat index, today's folder (#545). It is still the answer to
	// this call, but neither cache is kept, so the next read retries what
	// failed instead of remembering it absent.
	memo := &monthMemo{V: spendCacheVersion, Dir: usageDir, Month: monthUnix,
		Today: todayUnix, Days: baseDays, Open: openState, Flat: flatState, Tally: out}
	if complete {
		writeAtomic(monthMemoPath(), memo)
	}
	tallyMemo.Lock()
	tallyMemo.key, tallyMemo.ok, tallyMemo.state = key, complete, memo
	tallyMemo.Unlock()
	sweepSpendCache(first, last)
	if !complete {
		// The cached read could not see the month whole. When every surface of
		// the store reads and only receipts do not, the month is the total the
		// report and the export give it — those receipts skipped and named,
		// every other one counted (#654); a surface that cannot be read stays
		// the refusal (#545).
		if by, skipped, ok := receiptGapMonth(usageDir, first, last, monthUnix, todayUnix); ok {
			if len(skipped) > 0 {
				return by, &unreadableReceipts{paths: skipped}
			}
			return by, nil
		}
		return out, errUsageStoreUnreadable
	}
	return out, nil
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
