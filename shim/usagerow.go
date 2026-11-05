package shim

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	}
	raw, _ := io.ReadAll(io.LimitReader(stdin, 1<<20))
	if json.Unmarshal(raw, &in) != nil || in.TranscriptPath == "" {
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
	offset := readCursor(cursorPath)
	if offset > st.Size() || offset < 0 {
		offset = 0
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
		next += int64(len(line))
		e, usage, ok := parseLine(line)
		if !ok {
			continue
		}
		key := e.Message.ID + "\x00" + e.RequestID
		r := responses[key]
		if r == nil {
			r = &response{id: e.Message.ID, request: e.RequestID,
				model: e.Message.Model, timestamp: e.Timestamp, usage: usage}
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
	for _, key := range order {
		r := responses[key]
		ok, err := WriteUsageRow(profile, pricing, r.id, r.model, r.timestamp, responseToken(r.id, r.request), r.usage)
		if ok && err == nil {
			written++
		}
	}
	writeCursor(cursorPath, next)
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
	if recordedElsewhere(responseID, s.routeID) {
		return false, nil
	}
	// A response's lines can span hook passes — a Stop may read the transcript
	// before its last block landed — so fold in what this response's own row
	// already holds, and a later pass grows the row instead of adding to it.
	usage = mergeExisting(s, responseID, turnID, usage)
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
	return true, s.write(rec)
}

// recordedElsewhere is true when the row under responseID was written by
// another ccmorph writer: its route id is not this transcript hook's. A row of
// our own (or none) leaves the write to the sidecar's own ownership rules.
func recordedElsewhere(responseID, routeID string) bool {
	data, err := os.ReadFile(filepath.Join(stateDir(), "usage", rowFileName(responseID)))
	if err != nil {
		return false
	}
	d := parseDoc(data)
	if d == nil {
		return false
	}
	rid, ok := d.str("route_id")
	return ok && rid != routeID
}

// mergeExisting folds the usage this hook's own row for responseID already
// holds into usage, so a response whose lines span hook passes keeps one row
// with the larger counts. A row another writer owns never reaches here
// (recordedElsewhere), and a row carrying a different response's token — a
// recycled id — keeps its own numbers.
func mergeExisting(s *UsageSidecar, responseID, turnID string, usage usageRow) usageRow {
	data, err := os.ReadFile(filepath.Join(stateDir(), "usage", rowFileName(responseID)))
	if err != nil {
		return usage
	}
	d := parseDoc(data)
	if d == nil {
		return usage
	}
	if rid, ok := d.str("route_id"); !ok || rid != s.routeID {
		return usage
	}
	if tid, ok := d.str("turn_id"); !ok || tid != turnID {
		return usage
	}
	sp, ok := d.last("usage")
	if !ok {
		return usage
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

// rowFileName is the file a response's row lives in, by response id, so a
// transcript or a gateway error quoting that id joins to it.
func rowFileName(responseID string) string {
	sum := sha256.Sum256([]byte(responseID))
	return hex.EncodeToString(sum[:]) + ".json"
}

// cursorFile is where this session's read position is kept, keyed by the
// transcript path, so two sessions never share one.
func cursorFile(transcript string) string {
	sum := sha256.Sum256([]byte(transcript))
	return filepath.Join(stateDir(), "usage-cursors", hex.EncodeToString(sum[:])+".json")
}

func readCursor(path string) int64 {
	var c struct {
		Offset int64 `json:"offset"`
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &c) != nil {
		return 0
	}
	return c.Offset
}

func writeCursor(path string, offset int64) {
	data, err := json.Marshal(struct {
		Offset int64 `json:"offset"`
	}{offset})
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
