package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// --- usage rows from the client's own transcript --------------------------------
// A subscription or long-lived-token session talks to its upstream directly, and
// must keep doing so: a process-wide proxy would change what every Bash command,
// hook and MCP server in the session can reach, and would leak into a launch
// started inside it. Claude Code already records what it spent on every response
// in its transcript, and its Stop hook hands us that transcript's path. So the
// only thing ccmorph adds to such a launch is one hook command; the network path
// is untouched.
//
// A row is written once per response, keyed by the message id Claude Code
// already stores, and the hook's cursor makes a later turn read only what is
// new. Claude Code writes one transcript line per content block of a response;
// those lines share a message id and are folded into that response's one row.
// The cost is the same models.dev list price the shim uses, marked
// estimated_cost because a subscription bills nothing per token.

// UsageRowMain is `ccmorph-core usage-row --profile NAME --pricing ID`: the Stop
// hook. It reads Claude Code's hook JSON on stdin and never fails the session:
// anything unreadable is no rows and exit 0.
func UsageRowMain(args []string, stdin io.Reader) int {
	profile, pricing := "", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--profile" || a == "--pricing":
			if i+1 >= len(args) {
				return 2
			}
			i++
			if a == "--profile" {
				profile = args[i]
			} else {
				pricing = args[i]
			}
		case strings.HasPrefix(a, "--profile="):
			profile = strings.TrimPrefix(a, "--profile=")
		case strings.HasPrefix(a, "--pricing="):
			pricing = strings.TrimPrefix(a, "--pricing=")
		default:
			return 2
		}
	}
	if profile == "" {
		return 2
	}
	// Inside a Remote Control edge the edge's adapter records every response under
	// the route that served it; a transcript row would charge the same call to
	// this profile too. util.InEdge is the cli's own test for the same session,
	// so a launch that kept the variable (no loopback proxy: not an edge) keeps
	// its rows here too (#636).
	if util.InEdge() {
		return 0
	}
	var in struct {
		TranscriptPath string `json:"transcript_path"`
		HookEventName  string `json:"hook_event_name"`
	}
	onStop()
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if json.Unmarshal(raw, &in) != nil || in.TranscriptPath == "" {
		return 0
	}
	// SessionStart writes no row: it marks where this launch begins reading, so
	// a transcript it resumes does not charge its history to this profile.
	if in.HookEventName == "SessionStart" {
		startTranscript(in.TranscriptPath, time.Now())
		return 0
	}
	RecordTranscript(profile, pricing, in.TranscriptPath)
	return 0
}

// RecordTranscript writes a row for every response after the stored cursor, and
// returns how many rows it wrote. Every failure is swallowed: a hook that cannot
// read its transcript must not be a failed turn.
func RecordTranscript(profile, pricing, path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return 0
	}
	cursorPath := cursorFile(path)
	c := readCursor(cursorPath)
	offset := c.Offset
	if offset > st.Size() || offset < 0 {
		offset = 0
	}
	// A cursor's start time separates the history a resumed transcript already
	// held from the turns this launch served, even when a fork copied that
	// history into a file created after the mark.
	var since time.Time
	if c.Since != "" {
		if t, err := time.Parse(time.RFC3339Nano, c.Since); err == nil {
			since = t
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return 0
	}
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return 0
	}
	// Claude Code writes one line per content block of a response: a thinking
	// block, a text block and a tool_use block are three lines with one
	// message.id, one requestId and the same usage. The response is the unit —
	// its lines are folded into one row here, never written one each, or a
	// dashboard summing rows counts that spend once per block.
	type response struct {
		id, request, model, timestamp, session string
		start                                  int64 // offset of the response's first line
		usage                                  usageRow
	}
	var order []string
	responses := map[string]*response{}
	next := offset
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break // a line still being written is read next time
		}
		line := data[:i+1]
		data = data[i+1:]
		start := next
		next += int64(len(line))
		e, usage, ok := parseLine(line)
		if !ok {
			continue
		}
		if !since.IsZero() && e.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil && ts.Before(since) {
				continue // served before this launch began reading
			}
		}
		key := e.Message.ID + "\x00" + e.RequestID
		r := responses[key]
		if r == nil {
			r = &response{id: e.Message.ID, request: e.RequestID,
				model: e.Message.Model, timestamp: e.Timestamp, session: e.SessionID, start: start, usage: usage}
			responses[key] = r
			order = append(order, key)
			continue
		}
		r.usage = maxUsage(r.usage, usage)
		if e.Message.Model != "" {
			r.model = e.Message.Model
		}
		if e.Timestamp != "" {
			r.timestamp = e.Timestamp
		}
	}
	written := 0
	// The cursor only passes lines whose row is on disk. A failed write holds it
	// at that response's first line, so the next Stop retries it; a name every
	// spill already holds is permanent and must not stall the read. The pass
	// publishes one checkpoint (see the type), moves it before the row it writes
	// and stops moving it at the first failure, and commits it at the end —
	// a pass the hook's timeout stops commits the same object from the signal
	// handler (see onStop), so a long backlog drains across Stops rather than
	// rereading from the same offset forever.
	cp := &checkpoint{path: cursorPath, since: c.Since}
	cp.hold(next)
	progress.Store(cp)
	held := false
	for _, key := range order {
		r := responses[key]
		if !held {
			cp.hold(r.start) // the row in flight: a kill mid-write retries it
		}
		ok, err := writeRow(profile, pricing, r.id, r.model, r.timestamp, r.session, responseToken(r.id, r.request), r.usage)
		if ok && err == nil {
			written++
		}
		if err != nil && !errors.Is(err, errRowNameTaken) {
			held = true // the checkpoint stays before this row for the next pass
		}
	}
	if !held {
		cp.hold(next)
	}
	progress.Store(nil)
	cp.commit()
	return written
}

// transcriptEntry is the part of one transcript line this reads. Every field is
// optional: a line that is not an assistant message is simply skipped.
type transcriptEntry struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	RequestID string `json:"requestId"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string          `json:"id"`
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	} `json:"message"`
}

// parseLine is one transcript line's contribution to a response's row: the
// entry and its usage buckets. A line that is not an assistant message, or
// carries no response id or no usage, contributes nothing.
func parseLine(line []byte) (transcriptEntry, usageRow, bool) {
	if !bytes.Contains(line, []byte(`"assistant"`)) {
		return transcriptEntry{}, nil, false
	}
	var e transcriptEntry
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" || e.Message.ID == "" || len(e.Message.Usage) == 0 {
		return transcriptEntry{}, nil, false
	}
	usage := bucketsOf(e.Message.Usage, Span{0, len(e.Message.Usage)}, true)
	if len(usage) == 0 {
		return transcriptEntry{}, nil, false
	}
	return e, usage, true
}

// checkpoint is the cursor a stopped pass leaves: before the response being
// written, so a kill mid-row retries that row and skips the ones already on
// disk. It is the pass's only cursor state: published once, moved with the row
// in flight, and committed by both of the pass's exits — the normal end and
// onStop — so what a kill leaves is exactly what the pass would have written
// had it ended there. A plain store of one int64 costs nothing next to the
// row's own write.
type checkpoint struct {
	path  string
	since string
	off   atomic.Int64
}

// hold moves the checkpoint to just before offset.
func (cp *checkpoint) hold(offset int64) { cp.off.Store(offset) }

// commit writes the cursor the pass has earned: where the next pass resumes.
func (cp *checkpoint) commit() {
	writeCursor(cp.path, cursor{Offset: cp.off.Load(), Since: cp.since})
}

// progress is the running pass's checkpoint, read by onStop from the signal
// goroutine. A pass publishes it once and moves one field inside it.
var progress atomic.Pointer[checkpoint]

// onStop writes the running pass's checkpoint when the hook is stopped. A
// SIGKILL skips it; the next pass then rereads, which is idempotent.
func onStop() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		<-ch
		if cp := progress.Load(); cp != nil {
			cp.commit()
		}
		os.Exit(1)
	}()
}

// writeRow is the hook's row writer; a test swaps it to stop a pass mid-backlog.
var writeRow = writeUsageRow

// scanNote makes logScanOnce's note once per process.
var scanNote sync.Once

// logScanOnce records a third-party read failure once per process: an
// unreadable file in an archived neighbor (a root-owned row from a past
// sudo'd compaction) must stop this row's redirect or ownership lookups, not
// usage recording forever. The cursor pins only on this row's own write or
// lock failure; a scan that could not see is treated as nothing found.
func logScanOnce(err error) {
	scanNote.Do(func() { fmt.Fprintln(os.Stderr, "ccmorph usage hook:", err) })
}

// writeUsageRow writes one complete row for a response the client itself
// recorded. turnID makes the write idempotent: the same response always owns
// the same row, so a re-read replaces it instead of spilling a duplicate.
// It reports false when the row was deliberately not written: no profile or id,
// or a row another ccmorph writer already holds for that response.
func writeUsageRow(profile, pricing, responseID, model, timestamp, session, turnID string, usage usageRow) (bool, error) {
	if profile == "" || responseID == "" {
		return false, nil
	}
	// One instant for the pass: the original stamp's boundary and, after a
	// redirect, the target's are the same rule at the same moment.
	now := time.Now()
	lock, err := lockClosedUsage(now, rowDirs(timestamp))
	if err != nil {
		return false, err
	}
	if lock != nil {
		defer lock.Close()
	}
	// An archived turn stays in its original day: writers never mutate JSONL,
	// and a second day copy would charge the same response twice. An archive
	// this run cannot read is recorded once and treated as no redirect: a
	// foreign damaged file must not stop usage recording forever.
	original := timestamp
	for _, dir := range rowDirs(timestamp)[:3] {
		archived, err := paths.ReadUsageArchive(dir + ".jsonl")
		if err != nil {
			logScanOnce(err)
			continue
		}
		for _, row := range archived {
			// A row that does not carry the id is not this response's: the
			// byte filter spares parsing every archived row of three days for
			// each written row, under the exclusive lock (#654).
			if namesResponse(row.Data, responseID) && usageOwned(row.Data, responseID, turnID) {
				if d := parseDoc(row.Data); d != nil {
					if ts, ok := d.str("timestamp"); ok {
						timestamp = ts
					}
				}
			}
		}
	}
	// A redirect re-stamps the write into an older day — possibly into the one
	// compaction is folding right now, since the original stamp may have been
	// yesterday's, which takes no lock. Re-take the target's closed lock before
	// any write or retirement reaches it: a fresh row renamed onto a name the
	// compactor is about to unlink is usage destroyed with a stale archive left
	// behind. A lock already held is the store's one exclusive lock and covers
	// every day; only the open-to-closed straddle needs it taken now.
	if lock == nil && timestamp != original {
		if l, err := lockClosedUsage(now, rowDirs(timestamp)); err != nil {
			return false, err
		} else if l != nil {
			lock = l
			defer l.Close()
		}
	}
	s := newSidecar(profile, "transcript")
	if util.ValidTaskTag(session) {
		s.tags.session = &session
	}
	// A session whose traffic a ccmorph adapter already relays (a Remote Control
	// session, or a launch nested inside one) writes its own row for the same
	// response. One turn, one row: the writer that saw the traffic keeps it.
	if recorded, err := recordedElsewhere(responseID, s.routeID, turnID, timestamp, usage, lock != nil); err != nil || recorded {
		return false, err
	}
	// A response's lines can span hook passes — a Stop may read the transcript
	// before its last block landed — so fold in what this response's own row
	// already holds, and a later pass grows the row instead of adding to it.
	usage = mergeExisting(s, responseID, turnID, timestamp, usage)
	s.prices = CachedOwnedPriceBook(pricing)
	var modelPtr *string
	if model != "" {
		modelPtr = &model
		s.served = modelPtr
	}
	rec := s.start(responseID, nil, modelPtr, usage)
	if turnID != "" {
		rec.turnID = turnID
	}
	if ts := rowTime(timestamp); ts != "" {
		rec.timestamp = ts
	}
	rec.complete = true
	if err := s.writeLocked(rec); err != nil {
		return true, err
	}
	// A pass whose lines crossed a UTC day left this turn's row in the old
	// day's folder — or flat, from a build before the shards: retire it, so a
	// response is one row wherever its latest stamp says it lives.
	retireEarlierCopies(rowDirs(rec.timestamp), responseID, rec.turnID)
	return true, nil
}

// recordedElsewhere is true when a ccmorph row was written for this same
// response: the primary name, or a spill the relay left when another turn held
// that name. A row of our own (or none) leaves the write to the sidecar's own
// ownership rules; a row another route holds for a different response — a
// gateway that recycled the id — must not swallow ours, so the hook writes its
// own into a spill. The names are looked up in every directory the row can
// live in: its stamp's day and its neighbours, and the flat store. moveLocked
// says the caller already holds the store's move lock. A directory this run
// cannot scan is recorded once and skipped: a foreign damaged file stops this
// lookup, never the write — the cursor pins only on this row's own write or
// lock failure.
func recordedElsewhere(responseID, routeID, turnID, timestamp string, usage usageRow, moveLocked bool) (bool, error) {
	dirs := rowDirs(timestamp)
	primaryName := rowFileName(responseID)
	held := false
	for _, dir := range dirs {
		rows, err := usageRowsFor(dir, responseID, turnID, false, false)
		if err != nil {
			logScanOnce(err)
			continue
		}
		for _, row := range rows {
			if row.Name != primaryName {
				continue
			}
			data := row.Data
			if sameResponse(data, responseID, routeID, timestamp, usage) {
				return true, nil
			}
			if d := parseDoc(data); d != nil {
				if rid, ok := d.str("route_id"); ok && rid == routeID {
					if tid, ok := d.str("turn_id"); ok && tid == turnID {
						continue
					}
				}
			}
			held = true
		}
	}
	if !held {
		// No row under the id but our own, or none at all: nothing to match,
		// and no spill to look for. A spill is only written while the primary
		// is held, and nothing removes a primary, so an absent one means the
		// id was never recorded.
		return false, nil
	}
	// A primary holds another response's row (a recycled id, or another route's
	// reply under the same id). The relay spills under <id>.<its own random turn>,
	// a name no reader can derive, so the row is found by the response id it
	// carries. Only this case pays for the scan: a new response reads one file.
	// A spill's name starts with its response id's hash, so the spills of one
	// response are a glob, not a read of every row in the day. A spill a build
	// before #633 wrote is re-filed under that shape first, once per directory:
	// its old name hashes the joined key, so no glob of the new shape can find
	// it and no reader holding only the hook's turn could name it.
	//
	// The sweep renames rows the pre-shard move may be carrying, and the move
	// links a flat row into its day folder only then removing the flat name: a
	// rename landing between the two would leave one row under two names and a
	// reader counting both. Both run under the store's one lock — the caller's
	// own when it already holds it, as a closed day's write does, else a shared
	// claim that waits out a running move. A store whose lock cannot be opened
	// has no move or compaction either: they take the same file or run not at all.
	sweep := moveLocked
	if !moveLocked {
		// Bounded, never a hang: a busy store means the move or compaction this
		// re-file orders against is running now, so this pass skips it — the
		// status quo of a pass before it — and the next one tries (INTENT 8).
		if lock, err := paths.UsageStoreLockWithin(usageBase(), false, paths.UsageLockWait); err == nil && lock != nil {
			defer lock.Close()
			sweep = true
		}
	}
	for _, dir := range dirs {
		if sweep {
			migrateOldSpills(dir)
		}
		hit, err := spillHolds(dir, responseID, routeID, turnID, timestamp, usage)
		if err != nil {
			logScanOnce(err)
			continue
		}
		if hit {
			return true, nil
		}
	}
	return false, nil
}

// spillHolds reports whether any spill of this response under dir is this
// response. The scan may run on names the process cached earlier, and a cached
// listing is trusted for a yes only: a row it shows is a row that is there.
// A no cannot come from it — the relay lands spills beside a running pass, and
// a pass that finds no match writes its own row and moves the cursor past the
// response, so a miss is a permanent double count (dirNames). A miss on cached
// names is therefore re-checked against a listing taken for this answer; the
// re-check also heals a folder whose last listing failed, since refreshNames
// dropped the bad entry and lists again (the caller must not remember the
// miss, #538).
func spillHolds(dir, responseID, routeID, turnID, timestamp string, usage usageRow) (bool, error) {
	_, cached := dirNames[dir]
	rows, err := usageRowsFor(dir, responseID, turnID, true, false)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if sameResponse(row.Data, responseID, routeID, timestamp, usage) {
			return true, nil
		}
	}
	if !cached {
		// The listing behind these names was taken for this very scan: a miss
		// against it is a miss as of now.
		return false, nil
	}
	if _, ok := refreshNames(dir); !ok {
		return false, nil
	}
	rows, err = usageRowsFor(dir, responseID, turnID, true, false)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if sameResponse(row.Data, responseID, routeID, timestamp, usage) {
			return true, nil
		}
	}
	return false, nil
}

// sameResponse is true when a row another writer left under this response id is
// this response: the id, the request's own input counts, and a stamp no later
// than the reply's. The adapter observes the very payload the client receives,
// so its input-side counts are the transcript line's; a recycled id carries
// another request's counts, and a row stamped after the reply landed belongs to
// a later turn. Neither is this response's row to replace, and neither makes
// this response one already recorded.
func sameResponse(data []byte, responseID, routeID, timestamp string, usage usageRow) bool {
	d := parseDoc(data)
	if d == nil {
		return false
	}
	if rid, ok := d.str("route_id"); !ok || rid == routeID {
		return false
	}
	if id, ok := d.str("response_id"); !ok || id != responseID {
		return false
	}
	sp, ok := d.last("usage")
	if !ok {
		return false
	}
	if !sameInput(bucketsOf(d.b, sp, true), usage) {
		return false
	}
	theirs, ok := d.str("timestamp")
	if !ok {
		return true
	}
	return !stampedAfter(theirs, timestamp)
}

// inputBuckets are a response's input-side counts. Every writer that sees one
// response sees one request, so these three are equal however long the reply
// took, whatever profile wrote first and whichever name holds the row.
var inputBuckets = []string{"input_tokens", "cache_creation_input_tokens", "cache_read_input_tokens"}

// sameInput is true when two usage rows name the same request: every
// inputBuckets count equals, a missing key counting as zero.
func sameInput(a, b usageRow) bool {
	for _, key := range inputBuckets {
		va, _ := finiteNumber(valueOf(a, key))
		vb, _ := finiteNumber(valueOf(b, key))
		if va != vb {
			return false
		}
	}
	return true
}

func valueOf(u usageRow, key string) []byte {
	for _, n := range u {
		if n.key == key {
			return n.raw
		}
	}
	return nil
}

// stampSlack is how much later than the reply a relaying adapter's stamp may
// sit and still be this turn: the adapter stamps its row at message start, so a
// row stamped after the reply landed is a later response's, not this one's.
const stampSlack = time.Minute

// stampedAfter is true when theirs is provably later than ours. An unparsable
// stamp is never provably later: not knowing is not grounds for writing a
// second row for one response.
func stampedAfter(theirs, ours string) bool {
	ta, err := time.Parse(time.RFC3339Nano, theirs)
	if err != nil {
		return false
	}
	tb, err := time.Parse(time.RFC3339Nano, ours)
	if err != nil {
		return false
	}
	return ta.Sub(tb) > stampSlack
}

// mergeExisting folds the usage this hook's own row for responseID already
// holds into usage, so a response whose lines span hook passes keeps one row
// with the larger counts. It reads the names the sidecar itself writes — the
// primary, then the spills a recycled id's turn owns — in every directory the
// row can live in, because that row is not always the primary: when another
// turn holds <sha(id)>.json, the writer put ours in a spill, and reading the
// primary alone would replace the row with this pass's lines and shrink it.
// A spill a build before #633 wrote is read too: its name hashes this turn's
// joined key, so a pass that holds the turn can still name it. A row another
// writer owns for this response never reaches here
// (recordedElsewhere), and a row carrying a different response's token — a
// recycled id — keeps its own numbers.
func mergeExisting(s *UsageSidecar, responseID, turnID, timestamp string, usage usageRow) usageRow {
	for _, dir := range rowDirs(timestamp) {
		rows, err := usageRowsFor(dir, responseID, turnID, false, true)
		if err != nil {
			continue
		}
		for _, row := range rows {
			d := parseDoc(row.Data)
			if d == nil {
				continue
			}
			if rid, ok := d.str("route_id"); !ok || rid != s.routeID {
				continue
			}
			if tid, ok := d.str("turn_id"); !ok || tid != turnID {
				continue
			}
			sp, ok := d.last("usage")
			if !ok {
				continue
			}
			// bucketsOf drops the row's own estimated_cost (it is no bucket), and the
			// billed cost is dropped with it: this pass's lines, not the old row, carry
			// the numbers, and the estimate is recomputed from them.
			var old usageRow
			for _, n := range bucketsOf(d.b, sp, true) {
				if n.key != "cost" {
					old = append(old, n)
				}
			}
			usage = maxUsage(old, usage)
		}
	}
	return usage
}

// maxUsage is the per-key maximum of two usage rows: the counts a response
// reports are final in their last line, and a re-read must never shrink a row.
// A value that is not a plain number (the nested cache_creation object) is not
// compared — the later row's value is kept. Neither input is modified.
func maxUsage(a, b usageRow) usageRow {
	out := append(usageRow(nil), a...)
	for _, n := range b {
		found := false
		for i := range out {
			if out[i].key != n.key {
				continue
			}
			found = true
			if fa, ok := finiteNumber(out[i].raw); !ok {
				out[i].raw = append([]byte(nil), n.raw...)
			} else if fb, ok := finiteNumber(n.raw); ok && fb > fa {
				out[i].raw = append([]byte(nil), n.raw...)
			}
			break
		}
		if !found {
			out = append(out, kv{n.key, append([]byte(nil), n.raw...)})
		}
	}
	return out
}

// responseToken is the stable ownership key of one API response: every
// transcript line of one message.id and requestId folds into the same row,
// however many lines that response spans and however many times it is read.
func responseToken(messageID, requestID string) string {
	sum := sha256.Sum256([]byte("ccmorph-transcript-response\x00" + messageID + "\x00" + requestID))
	return hex.EncodeToString(sum[:16])
}

// utcStamp is the one spelling of a UTC timestamp the usage store uses:
// whole seconds, six fractional digits when they are not zero, +00:00. Every
// row and outcome carries it: the sidecar stamps what it writes now, and
// rowTime re-spells a transcript's stamp in it.
func utcStamp(t time.Time) string {
	out := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		out += fmt.Sprintf(".%06d", us)
	}
	return out + "+00:00"
}

// rowTime is a transcript timestamp in the row's own spelling (UTC, six
// fractional digits when it has them).
func rowTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	return utcStamp(t.UTC())
}

// dirNames holds each day folder's file names, from a listing that succeeded:
// a hook pass checks thousands of responses against folders of ~20,000 rows.
// A failed listing is never kept — the caller must not remember the miss
// (#538) — and a kept one is trusted for a yes only: a scan that finds no
// matching row re-checks against a listing taken for that answer (spillHolds),
// because a pass that misses a spill writes its own row and moves the cursor
// past the response: no later pass re-reads it, so a miss is a permanent
// double count. The window no listing closes is a spill the relay lands
// between this response's last listing and the hook's own write: that
// stays a duplicate candidate, not a clobber, because every writer claims the
// names it writes and drops its own row rather than overwrite another's
// (claim, sidecar.go), so a miss leaves two rows beside each other, never a
// lost relay row.
var dirNames = map[string][]string{}

// listDir is the folder listing spillsIn caches; a test swaps it to fail a
// listing (the writeRow pattern).
var listDir = os.ReadDir

// refreshNames lists dir and caches its names only on success; a failed
// listing drops any listing cached before it, so the next call retries
// instead of trusting a folder the process can no longer read.
func refreshNames(dir string) ([]string, bool) {
	entries, err := listDir(dir)
	if err != nil {
		delete(dirNames, dir)
		return nil, false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	dirNames[dir] = names // sorted by ReadDir
	return names, true
}

// spillsIn is every file in dir whose name starts with prefix except
// primaryName: the prefix is the primary's stem plus a dot, so it holds the
// primary's own name too, while this scan is for spills and its caller reads
// the primary itself. A cached listing whose glob comes up empty is checked
// against a fresh one before the empty answer goes out — relays add spills to
// these folders beside a running pass — and the fuller rule, that a cached
// listing may answer yes but never no, is spillHolds (dirNames).
func spillsIn(dir, prefix, primaryName string) []string {
	if names, ok := dirNames[dir]; ok {
		if out := globSpills(dir, names, prefix, primaryName); len(out) > 0 {
			return out
		}
	}
	names, ok := refreshNames(dir)
	if !ok {
		return nil
	}
	return globSpills(dir, names, prefix, primaryName)
}

// globSpills is the paths under dir whose names carry this response's spills;
// the primary's own name is not one of them.
func globSpills(dir string, names []string, prefix, primaryName string) []string {
	var out []string
	for i := sort.SearchStrings(names, prefix); i < len(names) && strings.HasPrefix(names[i], prefix); i++ {
		if names[i] != primaryName {
			out = append(out, filepath.Join(dir, names[i]))
		}
	}
	return out
}

// spillFileName is a spill's name: the response id's hash, then the spill key's,
// so every spill of one response matches <rowFileName stem>.*.json.
func spillFileName(responseID, key string) string {
	sum := sha256.Sum256([]byte(key))
	return strings.TrimSuffix(rowFileName(responseID), ".json") + "." + hex.EncodeToString(sum[:8]) + ".json"
}

// rowFileName is the file a response's row lives in, inside its stamp's day
// folder, by response id, so a transcript or a gateway error quoting that id
// joins to it.
func rowFileName(responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return hex.EncodeToString(sum[:]) + ".json"
}

// legacySpillMarker is the file that marks a directory's one-time rename of the
// spill names a build before #633 wrote. The sweep reads every row in a day
// folder — ~20,000 on a busy store — and a store pays for that once per
// directory, not on every pass. The name does not end in .json, so no reader
// takes it for a row.
const legacySpillMarker = ".spill-names-renamed"

// migrateOldSpills re-files the spill rows a build before #633 wrote. The old
// name is the hash of the joined <response id>.<turn id> key and the new one is
// the response id's hash followed by the key's, so no name of one shape names a
// row of the other: only the row's own content tells one apart. A row whose
// content says it is a spill — it carries a response id, and its name is
// neither that id's primary name nor a #633 spill name — is renamed from the
// response and turn it carries. Nothing else is touched: a row whose content
// does not parse stays where it is, and a new name another row already holds is
// never replaced. The scan runs once per directory per store (the marker), and
// only the rare held+miss spill scan calls it, so a normal pass never pays.
func migrateOldSpills(dir string) {
	marker := filepath.Join(dir, legacySpillMarker)
	if _, err := os.Stat(marker); err == nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no such directory (a compacted day), or none this pass can read
	}
	renamed, complete := false, true
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := paths.ReadRegularFile(path)
		if err != nil {
			complete = false
			continue
		}
		responseID, turnID, legacy := legacySpillOf(data, name)
		if !legacy || turnID == "" {
			continue // not a spill, or no key to spell the #633 name from
		}
		target := filepath.Join(dir, spillFileName(responseID, responseID+"."+turnID))
		if err := renameRow(path, target); err != nil {
			if !errors.Is(err, fs.ErrExist) && !errors.Is(err, fs.ErrNotExist) {
				complete = false
			}
			continue
		}
		renamed = true
	}
	if renamed {
		delete(dirNames, dir) // a listing this process cached names the old shape
	}
	if complete {
		// The marker is empty: its existence is the whole answer, so it is
		// created rather than written.
		if f, err := os.OpenFile(marker, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Close()
		}
	}
}

// legacySpillOf is the response and turn a row carries when the name it sits
// under is a spill a build before #633 wrote: a row whose name is neither its
// own primary name nor a #633 spill shape, identified by the content it holds.
func legacySpillOf(data []byte, name string) (responseID, turnID string, ok bool) {
	d := parseDoc(data)
	if d == nil {
		return "", "", false
	}
	id, hasID := d.str("response_id")
	if !hasID || id == "" {
		return "", "", false
	}
	primary := rowFileName(id)
	if name == primary {
		return "", "", false
	}
	if stem := strings.TrimSuffix(primary, ".json") + "."; strings.HasPrefix(name, stem) {
		if sum := strings.TrimSuffix(strings.TrimPrefix(name, stem), ".json"); len(sum) == 16 && isLowerHex(sum) {
			return "", "", false // the name #633 already gives a spill
		}
	}
	turn, _ := d.str("turn_id")
	return id, turn, true
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// renameRow gives a row its new name only when that name is free: the link (or,
// where the filesystem has no hardlinks, an exclusive create) is the claim, so a
// row another name already holds is never replaced. A failed unlink puts the
// new name back, leaving the row where it was.
func renameRow(old, target string) error {
	err := link(old, target)
	if err == nil {
		if rerr := remove(old); rerr != nil {
			remove(target)
			return rerr
		}
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	f, cerr := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if cerr != nil {
		return cerr
	}
	f.Close()
	if rerr := os.Rename(old, target); rerr != nil {
		remove(target)
		return rerr
	}
	return nil
}

// cursorFile is where this session's read position is kept, keyed by the
// transcript path, so two sessions never share one.
func cursorFile(transcript string) string {
	sum := sha256.Sum256([]byte(transcript))
	return filepath.Join(paths.StateDir(), "usage-cursors", hex.EncodeToString(sum[:])+".json")
}

// cursor is where a transcript's read position is kept. Offset is the byte a
// pass resumes at; Since is when this launch began reading it, so a resumed
// transcript's older turns — which another profile served — are not charged to
// this one. A cursor written before Since existed reads as offset only.
type cursor struct {
	Offset int64  `json:"offset"`
	Since  string `json:"since,omitempty"`
}

func readCursor(path string) cursor {
	var c cursor
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &c) != nil {
		return cursor{}
	}
	return c
}

func writeCursor(path string, c cursor) {
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	AtomicWrite(path, data, 0o600, filepath.Base(path)+".*.tmp")
}

// startTranscript marks where this launch begins reading path, for a transcript
// that has no cursor yet: everything the file already holds was served before
// this launch — by a pre-release ccmorph, by native claude, or under a route
// that writes no rows — and none of it is this profile's spend. An existing
// cursor is left alone: a compact, a clear or a second start resumes the same
// read position, and moving it would lose the pass that had not written yet.
func startTranscript(path string, now time.Time) {
	cursorPath := cursorFile(path)
	if _, err := os.Stat(cursorPath); err == nil {
		return
	}
	var offset int64
	if st, err := os.Stat(path); err == nil && st.Mode().IsRegular() {
		offset = st.Size()
	}
	writeCursor(cursorPath, cursor{Offset: offset, Since: now.UTC().Format(time.RFC3339Nano)})
}
