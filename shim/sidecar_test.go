package shim

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func strp(s string) *string { return &s }

// turn is a record for response id id, as a turn a gateway answered.
func turn(u *UsageSidecar, id string, cost int) *record {
	return u.start(id, strp("m"), strp("m"), usageRow{{"cost", []byte(strconv.Itoa(cost))}})
}

func nameOf(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]) + ".json"
}

// byName is every row in dir, keyed by file name, as written.
func byName(t *testing.T, dir string) map[string]string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	out := map[string]string{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out[filepath.Base(f)] = string(b)
	}
	return out
}

func hasTurn(row string, r *record) bool { return strings.Contains(row, `"turn_id":"`+r.turnID+`"`) }

// withoutHardlinks makes the filesystem refuse link(2), as exFAT and some network
// mounts do, so the claim falls back to an exclusive create.
func withoutHardlinks(t *testing.T) {
	old := link
	link = func(a, b string) error { return &os.LinkError{Op: "link", Old: a, New: b, Err: syscall.ENOTSUP} }
	t.Cleanup(func() { link = old })
}

// eachLinkMode runs fn once with hardlinks and once without.
func eachLinkMode(t *testing.T, fn func(t *testing.T)) {
	t.Run("hardlinks", fn)
	t.Run("no hardlinks", func(t *testing.T) { withoutHardlinks(t); fn(t) })
}

func TestCostLimitIsTheExporters(t *testing.T) {
	// scripts/usage-export pins the same literal; neither side may drift.
	if costLimit != float64(1<<53)/100 {
		t.Fatal(costLimit)
	}
}

func TestUnusableCostsStayOutOfTheRow(t *testing.T) {
	limit := strconv.FormatFloat(costLimit, 'g', -1, 64)
	for _, bad := range []string{"-0.01", "true", `"0.1"`, "null", "[1]", "1" + strings.Repeat("0", 308), "1e308", "9.1e13", limit} {
		in := []byte(`{"output_tokens":1,"cost":` + bad + `}`)
		if got := string(bucketsOf(in, Span{0, len(in)}, true).json()); got != `{"output_tokens":1}` {
			t.Errorf("cost %s kept: %s", bad, got)
		}
	}
	for _, good := range []string{"0", "1.5", "1e13"} {
		in := []byte(`{"cost":` + good + `}`)
		if got := string(bucketsOf(in, Span{0, len(in)}, true).json()); got != `{"cost":`+good+`}` {
			t.Errorf("cost %s dropped: %s", good, got)
		}
	}
}

func TestSSEBilledCostIsKeptFromEachUsageEvent(t *testing.T) {
	const extra = `"cost":0.5,"private":"not receipt data"`
	for _, kind := range []string{"message_start", "message_delta", "message_stop"} {
		t.Run(kind, func(t *testing.T) {
			dir := stateHome(t)
			usage := func(at string) string {
				if at == kind {
					return "," + extra
				}
				return ""
			}
			u := newSidecar("p", "identity")
			var rec *record
			for _, ev := range []string{
				`{"type":"message_start","message":{"id":"msg_c","model":"m","usage":{"input_tokens":11` + usage("message_start") + `}}}`,
				`{"type":"message_delta","usage":{"output_tokens":42` + usage("message_delta") + `}}`,
				`{"type":"message_stop","usage":{` + strings.TrimPrefix(usage("message_stop"), ",") + `}}`,
			} {
				rec = u.observe(parseDoc([]byte(ev)), rec, strp("m"))
			}
			got := rows(t, dir)
			if len(got) != 1 || !strings.Contains(got[0], `"complete":true`) || !strings.Contains(got[0], `"cost":0.5`) ||
				!strings.Contains(got[0], `"output_tokens":42`) || strings.Contains(got[0], "private") {
				t.Fatal(got)
			}
		})
	}
}

func TestRecycledResponseIDSpillsAndRewritesStayPut(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	first, second := turn(u, "msg_dup", 1), turn(u, "msg_dup", 2)
	u.write(first)
	u.write(second)
	first.complete = true // a same-record rewrite keeps its path
	u.write(first)
	got := byName(t, dir)
	spill := nameOf("msg_dup." + second.turnID)
	if len(got) != 2 || !hasTurn(got[nameOf("msg_dup")], first) || !strings.Contains(got[nameOf("msg_dup")], `"complete":true`) ||
		!hasTurn(got[spill], second) {
		t.Fatal(got)
	}
}

// Without hardlinks the claim is an exclusive create: sequential turns on a
// recycled id still each keep a row, and a loser lands in its spill on its very
// first write (issue 444), so a stream cancelled before its next update keeps it.
func TestWithoutHardlinksRecycledIDsKeepEveryRow(t *testing.T) {
	withoutHardlinks(t)
	dir := stateHome(t)
	a, b := newSidecar("p", "identity"), newSidecar("p", "identity")
	first, second := turn(a, "msg_no_link", 1), turn(b, "msg_no_link", 2)
	a.write(first)
	b.write(second)
	if got := byName(t, dir); len(got) != 2 || !hasTurn(got[nameOf("msg_no_link")], first) || !hasTurn(got[nameOf("msg_no_link."+second.turnID)], second) {
		t.Fatal("the loser must land in its spill on the first write:", got)
	}
	first.complete, second.complete = true, true
	a.write(first)
	b.write(second)
	got := byName(t, dir)
	if len(got) != 2 || !strings.Contains(got[nameOf("msg_no_link")], `"complete":true`) || !strings.Contains(got[nameOf("msg_no_link."+second.turnID)], `"complete":true`) {
		t.Fatal(got)
	}
}

// Writers racing on one response id each keep their row (#199): the path is
// claimed atomically, not picked with an exists() check; and a record's later
// rewrite replaces the file it owns, never another turn's row.
func TestConcurrentWritersEachKeepTheirRow(t *testing.T) {
	eachLinkMode(t, func(t *testing.T) {
		dir := stateHome(t)
		u := newSidecar("p", "identity")
		var records []*record
		for i := 0; i < 8; i++ {
			records = append(records, turn(u, "msg_race", i))
		}
		var wg sync.WaitGroup
		for _, r := range records {
			wg.Add(1)
			go func() { defer wg.Done(); u.write(r) }()
		}
		wg.Wait()
		got := byName(t, dir)
		if len(got) != 8 {
			t.Fatalf("%d rows", len(got))
		}
		for _, r := range records {
			found := 0
			for _, row := range got {
				if hasTurn(row, r) {
					found++
				}
			}
			if found != 1 {
				t.Fatalf("turn %s has %d rows", r.turnID, found)
			}
		}
		mine := records[3]
		mine.usage = usageRow{{"cost", []byte("99")}}
		mine.complete = true
		u.write(mine)
		got = byName(t, dir)
		updated := 0
		for _, row := range got {
			if hasTurn(row, mine) {
				updated++
				if !strings.Contains(row, `"complete":true`) || !strings.Contains(row, `"cost":99`) {
					t.Fatal(row)
				}
			}
		}
		if len(got) != 8 || updated != 1 {
			t.Fatal(len(got), updated)
		}
	})
}

// A temp file that will not unlink stays a second name for the claimed row; a
// later write must not reach the row through it.
func TestFailedTempUnlinkNeverTruncatesAClaimedRow(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	first, second := turn(u, "msg_alias", 1), turn(u, "msg_alias", 2)
	old := remove
	remove = func(string) error { return errors.New("fixture unlink") }
	u.write(first)
	remove = old
	u.write(second)
	got := byName(t, dir)
	if len(got) != 2 || !hasTurn(got[nameOf("msg_alias")], first) {
		t.Fatal(got)
	}
}

func TestRemovedClaimReclaimedByAnotherTurnIsNotOverwritten(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	first, second := turn(u, "msg_reclaimed", 1), turn(u, "msg_reclaimed", 2)
	u.write(first)
	os.Remove(filepath.Join(dir, nameOf("msg_reclaimed")))
	u.write(second)
	first.complete = true
	u.write(first)
	got := byName(t, dir)
	if len(got) != 2 || !hasTurn(got[nameOf("msg_reclaimed")], second) {
		t.Fatal(got)
	}
}

func TestSpillRewriteKeepsItsPathWhenPrimaryIsRemoved(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	first, second := turn(u, "msg_spill_stable", 1), turn(u, "msg_spill_stable", 2)
	u.write(first)
	u.write(second)
	os.Remove(filepath.Join(dir, nameOf("msg_spill_stable")))
	second.usage = usageRow{{"cost", []byte("9")}}
	u.write(second)
	second.complete = true
	u.write(second)
	got := byName(t, dir)
	if len(got) != 1 || !hasTurn(got[nameOf("msg_spill_stable."+second.turnID)], second) || !strings.Contains(got[nameOf("msg_spill_stable."+second.turnID)], `"complete":true`) {
		t.Fatal(got)
	}
}

// #320: both names of a turn can belong to other turns, the primary to a recycled
// id and the spill to a row whose own primary is that spill name. The write moves
// on to a further name and never replaces either row.
func TestSpillAliasingAnotherPrimaryKeepsEveryRow(t *testing.T) {
	eachLinkMode(t, func(t *testing.T) {
		dir := stateHome(t)
		u := newSidecar("p", "identity")
		unrelated, aliased := turn(u, "msg_a", 1), turn(u, "msg_a", 2)
		other := turn(u, "msg_a."+aliased.turnID, 3)
		for _, r := range []*record{unrelated, other, aliased} {
			u.write(r)
		}
		check := func() {
			t.Helper()
			got := byName(t, dir)
			if len(got) != 3 {
				t.Fatal(got)
			}
			for _, r := range []*record{unrelated, aliased, other} {
				n := 0
				for _, row := range got {
					if hasTurn(row, r) {
						n++
					}
				}
				if n != 1 {
					t.Fatalf("turn %s has %d rows", r.turnID, n)
				}
			}
		}
		check()
		for _, r := range []*record{aliased, other} { // rewrites keep their own rows
			r.complete = true
			u.write(r)
		}
		check()
		// Every one of its five names held by other turns: this turn loses its
		// own row and leaves all theirs untouched.
		blocked := turn(u, "msg_a", 4)
		key := "msg_a." + blocked.turnID
		for _, k := range []string{key, key + ".1", key + ".2", key + ".3"} {
			u.write(turn(u, k, 5))
		}
		before := byName(t, dir)
		if len(before) != 7 {
			t.Fatal(len(before))
		}
		if err := u.write(blocked); !errors.Is(err, fs.ErrExist) {
			t.Fatal(err)
		}
		after := byName(t, dir)
		for name, row := range before {
			if after[name] != row {
				t.Fatalf("%s changed", name)
			}
		}
		if tmp, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(tmp) != 0 || len(after) != 7 {
			t.Fatal("temp left or row added", tmp)
		}
	})
}

// --- stream exits ----------------------------------------------------------------

const captureSSE = "event:message_start\n" +
	`data:{"type":"message_start","message":{"id":"msg_capture_sse","model":"m-served","usage":{"input_tokens":11,"cache_read_input_tokens":2,"service_tier":"standard"}}}` + "\n\n" +
	"event:content_block_start\n" +
	`data:{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"Bash_0","name":"Bash","input":{}}}` + "\n\n" +
	"event:message_delta\n" +
	`data:{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
	"event:message_stop\n" +
	`data:{"type":"message_stop"}` + "\n\n"

// deadClient is a response writer that fails the way a hung-up client's does,
// once survive writes have gone through.
type deadClient struct {
	err     error
	survive int
	h       http.Header
}

func (d *deadClient) Header() http.Header {
	if d.h == nil {
		d.h = http.Header{}
	}
	return d.h
}
func (d *deadClient) WriteHeader(int) {}
func (d *deadClient) Flush()          {}
func (d *deadClient) Write(b []byte) (int, error) {
	if d.survive == 0 {
		return 0, d.err
	}
	d.survive--
	return len(b), nil
}

type stallReader struct{ r io.Reader }

func (s stallReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err == io.EOF {
		return n, errStall
	}
	return n, err
}

func relay(t *testing.T, w http.ResponseWriter, in io.Reader, u *UsageSidecar) (readErr, writeErr error) {
	t.Helper()
	rq := &request{w: w, rc: http.NewResponseController(w)}
	return NewServer("").relayStream(rq, &thinkingStream{r: bufio.NewReader(in)}, u, strp("m"), "")
}

func TestClientDisconnectMidStreamIsSwallowed(t *testing.T) {
	for _, err := range []error{syscall.EPIPE, syscall.ECONNRESET} {
		t.Run(err.Error(), func(t *testing.T) {
			readErr, writeErr := relay(t, &deadClient{err: err}, strings.NewReader(captureSSE), nil)
			if readErr != nil || !errors.Is(writeErr, err) {
				t.Fatal(readErr, writeErr)
			}
		})
	}
}

func TestClientDisconnectKeepsThePartialRow(t *testing.T) {
	dir := stateHome(t)
	// Two writes get through, the message_start event's two lines, which is where
	// the row is written; the blank line after it is the last the client hears.
	_, writeErr := relay(t, &deadClient{err: syscall.EPIPE, survive: 2}, strings.NewReader(captureSSE), newSidecar("p", "identity"))
	got := rows(t, dir)
	if writeErr == nil || len(got) != 1 || !strings.Contains(got[0], `"response_id":"msg_capture_sse"`) || !strings.Contains(got[0], `"complete":false`) {
		t.Fatal(writeErr, got)
	}
}

// Every way a stream can end before message_stop leaves its own incomplete row:
// each turn here reuses one response id, so a row lost or shared shows as a count.
func TestStreamExitsEachLeaveTheirOwnIncompleteRow(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	start := captureSSE[:strings.Index(captureSSE, "event:content_block_start")]
	exits := map[string]func() (io.Reader, http.ResponseWriter){
		"upstream EOF": func() (io.Reader, http.ResponseWriter) { return strings.NewReader(start), httptest.NewRecorder() },
		"error event": func() (io.Reader, http.ResponseWriter) {
			return strings.NewReader(start + "event: error\ndata: {\"type\":\"error\"}\n\n"), httptest.NewRecorder()
		},
		"client disconnect": func() (io.Reader, http.ResponseWriter) {
			return strings.NewReader(captureSSE), &deadClient{err: syscall.EPIPE, survive: 2}
		},
		"upstream stall": func() (io.Reader, http.ResponseWriter) {
			return stallReader{strings.NewReader(start)}, httptest.NewRecorder()
		},
	}
	for name, exit := range exits {
		in, w := exit()
		relay(t, w, in, u)
		t.Log(name)
	}
	got := rows(t, dir)
	if len(got) != len(exits) {
		t.Fatalf("%d rows for %d turns", len(got), len(exits))
	}
	for _, row := range got {
		if strings.Contains(row, `"complete":true`) {
			t.Fatal("an unfinished stream left a complete row:", row)
		}
	}
}
