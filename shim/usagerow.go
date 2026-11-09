package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	var in struct {
		TranscriptPath string `json:"transcript_path"`
		HookEventName  string `json:"hook_event_name"`
	}
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if json.Unmarshal(raw, &in) != nil || in.TranscriptPath == "" {
		return 0
	}
	// SessionStart writes no row: it marks where this launch begins reading, so
	// a transcript it resumes does not charge its history to this profile.
	if in.HookEventName == "SessionStart" {
		StartTranscript(in.TranscriptPath, time.Now())
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
		id, request, model, timestamp string
		start                         int64 // offset of the response's first line
		usage                         usageRow
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
				model: e.Message.Model, timestamp: e.Timestamp, start: start, usage: usage}
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
	// spill already holds is permanent and must not stall the read.
	at := next
	for _, key := range order {
		r := responses[key]
		ok, err := WriteUsageRow(profile, pricing, r.id, r.model, r.timestamp, responseToken(r.id, r.request), r.usage)
		if ok && err == nil {
			written++
		}
		if err != nil && !errors.Is(err, errRowNameTaken) && r.start < at {
			at = r.start
		}
	}
	writeCursor(cursorPath, cursor{Offset: at, Since: c.Since})
	return written
}

// transcriptEntry is the part of one transcript line this reads. Every field is
// optional: a line that is not an assistant message is simply skipped.
type transcriptEntry struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	RequestID string `json:"requestId"`
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

// WriteUsageRow writes one complete row for a response the client itself
// recorded. turnID makes the write idempotent: the same response always owns
// the same row, so a re-read replaces it instead of spilling a duplicate.
// It reports false when the row was deliberately not written: no profile or id,
// or a row another ccmorph writer already holds for that response.
func WriteUsageRow(profile, pricing, responseID, model, timestamp, turnID string, usage usageRow) (bool, error) {
	if profile == "" || responseID == "" {
		return false, nil
	}
	s := newSidecar(profile, "transcript")
	// A session whose traffic a ccmorph adapter already relays (a Remote Control
	// session, or a launch nested inside one) writes its own row for the same
	// response. One turn, one row: the writer that saw the traffic keeps it.
	if recordedElsewhere(responseID, s.routeID, turnID, timestamp, usage) {
		return false, nil
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
	if err := s.write(rec); err != nil {
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
// live in: its stamp's day and its neighbours, and the flat store.
func recordedElsewhere(responseID, routeID, turnID, timestamp string, usage usageRow) bool {
	dirs := rowDirs(timestamp)
	primaryName := rowFileName(responseID)
	held := false
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, primaryName))
		if err != nil {
			continue
		}
		if sameResponse(data, responseID, routeID, timestamp, usage) {
			return true
		}
		// This hook's own row for this response is not another writer's: the
		// write is the sidecar's to replace, and mergeExisting folds it in.
		if d := parseDoc(data); d != nil {
			if rid, ok := d.str("route_id"); ok && rid == routeID {
				if tid, ok := d.str("turn_id"); ok && tid == turnID {
					continue
				}
			}
		}
		held = true
	}
	if !held {
		// No row under the id but our own, or none at all: nothing to match,
		// and no spill to look for. A spill is only written while the primary
		// is held, and nothing removes a primary, so an absent one means the
		// id was never recorded.
		return false
	}
	// A primary holds another response's row (a recycled id, or another route's
	// reply under the same id). The relay spills under <id>.<its own random turn>,
	// a name no reader can derive, so the row is found by the response id it
	// carries. Only this case pays for the scan: a new response reads one file.
	needle := append([]byte(`"response_id":`), jsonStr(responseID)...)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		primary := filepath.Join(dir, primaryName)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if path == primary {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, needle) {
				continue
			}
			if sameResponse(data, responseID, routeID, timestamp, usage) {
				return true
			}
		}
	}
	return false
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
// primary alone would replace the row with this pass's lines and shrink it. A
// row another writer owns for this response never reaches here
// (recordedElsewhere), and a row carrying a different response's token — a
// recycled id — keeps its own numbers.
func mergeExisting(s *UsageSidecar, responseID, turnID, timestamp string, usage usageRow) usageRow {
	for _, dir := range rowDirs(timestamp) {
		primary, spills := rowNames(dir, responseID, turnID)
		for _, path := range append([]string{primary}, spills...) {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			d := parseDoc(data)
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
			return maxUsage(old, usage)
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

// rowTime is a transcript timestamp in the row's own spelling (UTC, six
// fractional digits when it has them).
func rowTime(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return ""
	}
	t = t.UTC()
	out := t.Format("2006-01-02T15:04:05")
	if us := t.Nanosecond() / 1000; us != 0 {
		out += fmt.Sprintf(".%06d", us)
	}
	return out + "+00:00"
}

// rowFileName is the file a response's row lives in, inside its stamp's day
// folder, by response id, so a transcript or a gateway error quoting that id
// joins to it.
func rowFileName(responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return hex.EncodeToString(sum[:]) + ".json"
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
	tmp := path + "." + fmt.Sprint(os.Getpid()) + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil && os.Rename(tmp, path) != nil {
		os.Remove(tmp)
	}
}

// StartTranscript marks where this launch begins reading path, for a transcript
// that has no cursor yet: everything the file already holds was served before
// this launch — by a pre-release ccmorph, by native claude, or under a route
// that writes no rows — and none of it is this profile's spend. An existing
// cursor is left alone: a compact, a clear or a second start resumes the same
// read position, and moving it would lose the pass that had not written yet.
func StartTranscript(path string, now time.Time) {
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
