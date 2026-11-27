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

// nameOf is the file a row key lands in: a bare response id is a primary, an
// "<id>.<turn>[.<n>]" key is that id's spill.
func nameOf(key string) string {
	if id, _, ok := strings.Cut(key, "."); ok {
		return spillFileName(id, key)
	}
	return rowFileName(key)
}

// byName is every row in dir, keyed by file name, as written.
func byName(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := rowFilesOf(dir)
	out := map[string]string{}
	for _, f := range files {
		b, _ := os.ReadFile(f)
		out[filepath.Base(f)] = string(b)
	}
	return out
}

func firstRowPath(t *testing.T, dir string) string {
	t.Helper()
	files := rowFilesOf(dir)
	if len(files) == 0 {
		t.Fatal("no row on disk")
	}
	return files[0]
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
	// share/usage-export pins the same literal; neither side may drift.
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
	os.Remove(filepath.Join(dir, usageDay(first.timestamp), nameOf("msg_reclaimed")))
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
	os.Remove(filepath.Join(dir, usageDay(first.timestamp), nameOf("msg_spill_stable")))
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
			// Another turn's row sits at each of this turn's spill names.
			holder := turn(u, "msg_a", 5)
			day := filepath.Dir(firstRowPath(t, dir))
			if err := os.WriteFile(filepath.Join(day, nameOf(k)), u.row(holder), 0o600); err != nil {
				t.Fatal(err)
			}
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
		if tmp, _ := filepath.Glob(filepath.Join(dir, "*", "*.tmp")); len(tmp) != 0 || len(after) != 7 {
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

// A copy of the hook's own row in another day that a build before #633 named
// under the joined key's hash is retired with the new-shape copies; another
// turn's old-name row in the same folder stays (2026-11-17).
func TestAnOldSpillInAnotherDayIsRetired(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("p", "identity")
	mine, theirs := turn(u, "msg_old", 1), turn(u, "msg_old", 2)
	mine.timestamp = "2026-11-04T23:00:00+00:00"
	day := filepath.Join(dir, usageDay(mine.timestamp))
	if err := os.MkdirAll(day, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(day, rowFileName("msg_old."+mine.turnID))
	if err := os.WriteFile(old, u.row(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(day, rowFileName("msg_old."+theirs.turnID))
	if err := os.WriteFile(other, u.row(theirs), 0o600); err != nil {
		t.Fatal(err)
	}
	retireEarlierCopies(rowDirs("2026-11-05T00:10:00Z"), "msg_old", mine.turnID)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("the old-name copy of this turn's row is still there: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another turn's old-name row was removed: %v", err)
	}
}

// --- prompt-cache lineage rows (GH-993) ---------------------------------------

// relayRows relays each body as one gateway-served /v1/messages call on a fresh
// route and returns the written rows keyed by the response id that answers it.
func relayRows(t *testing.T, bodies ...string) map[string]string {
	t.Helper()
	dir := stateHome(t)
	s := NewServer("http://up.invalid")
	n := 0
	s.remote = &http.Client{Transport: roundTrip(func(*http.Request) *http.Response {
		n++
		body := `{"id":"msg_lin_` + strconv.Itoa(n) + `","model":"m","usage":{"input_tokens":1,"cache_read_input_tokens":100}}`
		return &http.Response{StatusCode: 200, Status: "200 OK",
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: int64(len(body))}
	})}
	sum := sha256.Sum256([]byte("k"))
	spec := `{"profile":"p","identity":"i","upstream":"http://up.invalid","timeout":1,"key_sha256":"` + hex.EncodeToString(sum[:]) + `"}`
	routes, err := newModelRoutes(spec, s.remote)
	if err != nil {
		t.Fatal(err)
	}
	routes.usage.prices = nil
	s.Routes = routes
	out := map[string]string{}
	for _, b := range bodies {
		rq := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(b))
		rq.Header.Set("x-api-key", "k")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, rq)
		if rec.Code != 200 {
			t.Fatalf("relay: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, row := range rows(t, dir) {
		id, rest, ok := strings.Cut(row, `"response_id":"`)
		if !ok {
			continue
		}
		id, _, _ = strings.Cut(rest, `"`)
		out[id] = row
	}
	if len(out) != len(bodies) {
		t.Fatalf("%d rows for %d calls: %v", len(out), len(bodies), out)
	}
	return out
}

// rowField is the string value of a top-level row field, "" when absent.
func rowField(t *testing.T, row, name string) string {
	t.Helper()
	_, rest, ok := strings.Cut(row, `,"`+name+`":"`)
	if !ok {
		return ""
	}
	v, _, _ := strings.Cut(rest, `"`)
	return v
}

// lineageEntries maps each breakpoint's "position:ttl" to its digest, and
// returns the names in the order the row carries them (the scope summary).
func lineageEntries(t *testing.T, row string) (map[string]string, []string) {
	t.Helper()
	out := map[string]string{}
	var names []string
	lin := rowField(t, row, "lineage")
	if lin == "" {
		return out, names
	}
	for _, e := range strings.Split(lin, ",") {
		parts := strings.Split(e, ":")
		if len(parts) != 3 {
			t.Fatalf("lineage entry %q is not position:ttl:digest", e)
		}
		name := parts[0] + ":" + parts[1]
		out[name] = parts[2]
		names = append(names, name)
	}
	return out, names
}

// The gateway's rows must answer what a session's transcript cannot (GH-993):
// whether the forwarded cacheable prefix changed between adjacent calls, and
// above which block. Three calls — one that only appends, one that rewrites
// the system block with the markers unchanged — read from the rows as: same
// breakpoint names throughout, digests equal up to the append, then the head
// digest moved with every digest below it. And the rows carry digests only.
func TestRowsCorrelateTheWarmCacheLineageFromMetadataOnly(t *testing.T) {
	const marker = `{"type":"ephemeral","ttl":"1h"}`
	head := `"tools":[{"name":"t1"},{"name":"t2","cache_control":{"type":"ephemeral"}}],` +
		`"system":[{"type":"text","text":"sys-one","cache_control":` + marker + `}],`
	msg1 := `{"role":"user","content":[{"type":"text","text":"probe-one","cache_control":` + marker + `}]}`
	call1 := `{"model":"m",` + head + `"messages":[` + msg1 + `]}`
	call2 := `{"model":"m",` + head + `"messages":[` + msg1 +
		`,{"role":"assistant","content":[{"type":"text","text":"reply-one"}]},` +
		`{"role":"user","content":[{"type":"text","text":"probe-two","cache_control":` + marker + `}]}]}`
	call3 := strings.Replace(call2, "sys-one", "sys-two", 1)

	t.Run("adjacent calls correlate by scope and prefix digest", func(t *testing.T) {
		got := relayRows(t, call1, call2, call3)
		r1, r2, r3 := got["msg_lin_1"], got["msg_lin_2"], got["msg_lin_3"]
		h1, h2, h3 := rowField(t, r1, "head_sha256"), rowField(t, r2, "head_sha256"), rowField(t, r3, "head_sha256")
		if h1 == "" || h2 != h1 {
			t.Fatalf("an append-only history moved the head digest: %q vs %q", h1, h2)
		}
		e1, n1 := lineageEntries(t, r1)
		if want := []string{"t1:", "s0:1h", "m0.0:1h"}; strings.Join(n1, ",") != strings.Join(want, ",") {
			t.Fatalf("call 1 breakpoints %v, want %v", n1, want)
		}
		e2, n2 := lineageEntries(t, r2)
		if strings.Join(n2, ",") != "t1:,s0:1h,m0.0:1h,m2.0:1h" {
			t.Fatalf("call 2 breakpoints %v", n2)
		}
		for _, name := range []string{"t1:", "s0:1h", "m0.0:1h"} {
			if e2[name] != e1[name] {
				t.Fatalf("the warm prefix at %q was rewritten by an append-only history: %q vs %q", name, e1[name], e2[name])
			}
		}
		e3, n3 := lineageEntries(t, r3)
		if strings.Join(n3, ",") != strings.Join(n2, ",") {
			t.Fatalf("the scope summary moved without the markers changing: %v vs %v", n3, n2)
		}
		if h3 == h1 {
			t.Fatal("a rewritten system block left the head digest unchanged")
		}
		if e3["t1:"] != e2["t1:"] {
			t.Fatal("the digest above the rewritten system block moved: the head digest cannot say where the divergence starts")
		}
		if e3["s0:1h"] == e2["s0:1h"] || e3["m2.0:1h"] == e2["m2.0:1h"] {
			t.Fatal("a rewritten system block left a digest below it unchanged")
		}
	})

	t.Run("rows carry no prompt text", func(t *testing.T) {
		for id, row := range relayRows(t, call1, call2, call3) {
			for _, needle := range []string{"probe-", "reply-", "sys-one", "sys-two", "ephemeral"} {
				if strings.Contains(row, needle) {
					t.Fatalf("row %s carries %q", id, needle)
				}
			}
			if rowField(t, row, "head_sha256") == "" || !strings.Contains(row, `"route_id"`) {
				t.Fatalf("row %s lost its own metadata: %s", id, row)
			}
		}
	})

	t.Run("string system, no markers and a body that is not an object", func(t *testing.T) {
		stringSys := `{"model":"m","system":"plain","messages":[{"role":"user","content":[{"type":"text","text":"probe","cache_control":{"type":"ephemeral"}}]}]}`
		got := relayRows(t, stringSys)
		row := got["msg_lin_1"]
		if rowField(t, row, "head_sha256") == "" {
			t.Fatal("a valid body wrote no head digest")
		}
		if _, names := lineageEntries(t, row); strings.Join(names, ",") != "m0.0:" {
			t.Fatalf("string-system breakpoints %v, want [m0.0:]", names)
		}
		got = relayRows(t, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		row = got["msg_lin_1"]
		if rowField(t, row, "lineage") != "" || rowField(t, row, "head_sha256") == "" {
			t.Fatalf("an unmarked body: %s", row)
		}
		got = relayRows(t, `{"model":"m",`)
		row = got["msg_lin_1"]
		if rowField(t, row, "head_sha256") != "" || rowField(t, row, "lineage") != "" {
			t.Fatalf("an unparseable body was digested: %s", row)
		}
	})
}
