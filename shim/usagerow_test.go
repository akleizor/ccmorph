package shim

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"

	"github.com/akleizor/ccmorph/util/paths"
)

func seedOwnedPrices(t *testing.T, usageDir, provider string, models map[string]price) {
	t.Helper()
	root := filepath.Dir(usageDir)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := jsonv2.Marshal(priceCache{Host: "owned:" + provider, Fetched: time.Now().Unix(), Models: models})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "prices-owned-"+provider+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func transcriptLine(uuid, id, model, ts string, usage string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","timestamp":"` + ts + `",` +
		`"message":{"id":"` + id + `","model":"` + model + `","usage":` + usage + `}}` + "\n"
}

// blockLine is one content block of a response, the way Claude Code splits an
// assistant message into one transcript line per block: every block line of a
// response carries its own uuid, the same requestId, and the same message. An
// optional last argument is the line's timestamp (2026-11-05T10:00:00Z).
func blockLine(uuid, id, req, block, usage string, ts ...string) string {
	stamp := "2026-11-05T10:00:00Z"
	if len(ts) > 0 {
		stamp = ts[0]
	}
	return `{"type":"assistant","uuid":"` + uuid + `","requestId":"` + req + `","timestamp":"` + stamp + `",` +
		`"message":{"id":"` + id + `","model":"claude-opus-5-5","content":[{"type":"` + block + `"}],` +
		`"usage":` + usage + `}}` + "\n"
}

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeRow(t *testing.T, raw string) map[string]any {
	t.Helper()
	var row map[string]any
	if err := json.Unmarshal([]byte(raw), &row); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	return row
}

func TestRecordTranscriptWritesOneRowPerAssistantMessage(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		`{"type":"user","uuid":"u0","message":{"role":"user","content":"hi"}}`+"\n",
		transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00.500Z",
			`{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":0}`),
		"not json at all\n",
		transcriptLine("u2", "msg_2", "claude-opus-5-5", "2026-11-05T10:00:02Z",
			`{"input_tokens":1000,"output_tokens":5,"cache_read_input_tokens":2000,"cache_creation_input_tokens":7}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 2 {
		t.Fatalf("rows written %d", n)
	}
	all := rows(t, dir)
	if len(all) != 2 {
		t.Fatalf("%v", all)
	}
	byID := map[string]map[string]any{}
	for _, raw := range all {
		row := decodeRow(t, raw)
		byID[row["response_id"].(string)] = row
		if row["profile"] != "sub" || row["served_model"] != "claude-opus-5-5" || row["complete"] != true {
			t.Fatalf("%v", row)
		}
		usage := row["usage"].(map[string]any)
		if _, billed := usage["cost"]; billed {
			t.Fatalf("a subscription bills nothing per token: %v", usage)
		}
		if usage["estimated_cost"] == nil {
			t.Fatalf("every class the provider prices must be estimated: %v", usage)
		}
	}
	first := byID["msg_1"]
	if first == nil || first["timestamp"] != "2026-11-05T10:00:00.500000+00:00" {
		t.Fatalf("%v", first)
	}
	firstUsage := first["usage"].(map[string]any)
	// (10*4 + 25*20) / 1e6
	if firstUsage["estimated_cost"] != 0.00054 || firstUsage["input_tokens"] != float64(10) {
		t.Fatalf("%v", firstUsage)
	}
	second := byID["msg_2"]
	if second == nil || second["usage"].(map[string]any)["cache_read_input_tokens"] != float64(2000) {
		t.Fatalf("%v", second)
	}
	// Each row lives in the day folder of its UTC stamp (#227): a windowed
	// read then opens that day's folder alone, never every row ever written.
	for _, id := range []string{"msg_1", "msg_2"} {
		if _, err := os.Stat(filepath.Join(dir, "2026-11-05", rowFileName(id))); err != nil {
			t.Fatalf("the row for %s is not in its day folder: %v", id, err)
		}
	}
}

// The one-time move puts each flat row into the day folder of its mtime, keeps
// a row whose name is already taken in that folder flat, and is safe to run
// again (#227).
func TestTheMovePutsFlatRowsIntoTheirMtimeDayFolder(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string, mtime time.Time) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(dir, name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	// The mtime names the day: a row written yesterday belongs to yesterday.
	yday := time.Now().UTC().AddDate(0, 0, -1)
	write("a.json", `{"response_id":"a"}`, yday)
	write("taken.json", `{"response_id":"first"}`, yday)
	write("tmp9.tmp", `{"response_id":"in flight"}`, yday) // the atomic writer's, never a row
	// A writer of the build before the shards may still rewrite a row inside
	// its live stream, under the flat name the move would free: a row that
	// young stays flat, or the old writer's next claim would be a second copy.
	write("fresh.json", `{"response_id":"fresh"}`, time.Now())
	folder := filepath.Join(dir, yday.Format("2006-01-02"))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	// A name already held in that day folder is another row's: the flat one
	// stays; never a row is destroyed.
	if err := os.WriteFile(filepath.Join(folder, "taken.json"), []byte(`{"response_id":"second"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	moveFlatRows()
	moved := filepath.Join(folder, "a.json")
	if data, err := os.ReadFile(moved); err != nil || string(data) != `{"response_id":"a"}` {
		t.Fatalf("the row was not moved into its mtime's day folder: %v %s", err, data)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.json")); !os.IsNotExist(err) {
		t.Fatalf("the moved row left a flat copy: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "taken.json")); err != nil || string(data) != `{"response_id":"first"}` {
		t.Fatalf("a taken name must leave the flat row be: %v %s", err, data)
	}
	if _, err := os.Stat(filepath.Join(dir, "tmp9.tmp")); err != nil {
		t.Fatalf("an in-flight temp is not the move's to take: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.json")); err != nil {
		t.Fatalf("a row a pre-shard writer may still rewrite must stay flat: %v", err)
	}
	if st, err := os.Stat(folder); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("the day folder is not private: %v %v", err, st)
	}
	// Safe to run again: nothing movable is flat any more, and nothing changes.
	before := rowFilesOf(dir)
	moveFlatRows()
	if after := rowFilesOf(dir); strings.Join(after, "\x00") != strings.Join(before, "\x00") {
		t.Fatalf("a second move changed the store: %v -> %v", before, after)
	}
}

// A move stopped between its link and its remove (killed, or a remove that
// failed) must leave each row in one place, and the next run finishes it: a
// row read from the flat store and a day folder is counted twice (#298).
func TestAnInterruptedMoveLeavesEveryRowInOnePlace(t *testing.T) {
	dir := stateHome(t)
	yday := time.Now().UTC().AddDate(0, 0, -1)
	folder := filepath.Join(dir, yday.Format("2006-01-02"))
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	flat := func(name string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(`{"response_id":"`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, yday, yday); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// A move killed after its link: the row sits under both names.
	killed := flat("killed.json")
	if err := os.Link(killed, filepath.Join(folder, "killed.json")); err != nil {
		t.Fatal(err)
	}
	// A move whose remove fails: the flat name will not go.
	stuck := flat("stuck.json")
	old := remove
	t.Cleanup(func() { remove = old })
	remove = func(path string) error {
		if path == stuck {
			return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission}
		}
		return old(path)
	}
	moveFlatRows()
	if got := usageRowFiles(dir, time.Time{}, time.Now()); len(got) != 2 {
		t.Fatalf("a row is read from two stores: %v", got)
	}
	if _, err := os.Stat(killed); !os.IsNotExist(err) {
		t.Fatalf("the re-run kept the killed move's flat copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(folder, "killed.json")); err != nil {
		t.Fatalf("the killed move's row is gone: %v", err)
	}
	if _, err := os.Stat(stuck); err != nil {
		t.Fatalf("a row whose flat name would not go must stay flat: %v", err)
	}
	// Once the remove works, a re-run completes the move.
	remove = old
	moveFlatRows()
	if _, err := os.Stat(stuck); !os.IsNotExist(err) {
		t.Fatalf("the re-run did not complete the move: %v", err)
	}
	if got := usageRowFiles(dir, time.Time{}, time.Now()); len(got) != 2 {
		t.Fatalf("after the re-run: %v", got)
	}
}

// Two reports can run at once, and a move that ran beside another run's
// rollback could take a row's last name: one run finds the interrupted move's
// claim and removes the flat name while the other takes the day name back.
// The move holds one lock, so a run that finds it held leaves the move to its
// holder — the kernel drops the lock when the holder exits (#298).
func TestTheMoveRunsUnderOneLockAtATime(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	yday := time.Now().UTC().AddDate(0, 0, -1)
	path := filepath.Join(dir, "a.json")
	if err := os.WriteFile(path, []byte(`{"response_id":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, yday, yday); err != nil {
		t.Fatal(err)
	}
	held, ok := moveLock(dir)
	if !ok {
		t.Fatal("the move lock was free")
	}
	moveFlatRows()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a move ran beside the lock's holder: %v", err)
	}
	held.Close()
	moveFlatRows()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the move did not run once the lock was free: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, yday.Format("2006-01-02"), "a.json")); err != nil {
		t.Fatalf("the row did not reach its day folder: %v", err)
	}
}

// A response whose later lines land on the next UTC day: the next pass folds
// the row's earlier lines and replaces it where the new stamp says, leaving no
// copy in the old day folder — one response, one row, in the day folder of the
// stamp it was last read with (#227).
func TestARowWhoseStampCrossesMidnightMovesFolders(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_1", "thinking", `{"input_tokens":10,"output_tokens":25}`, "2026-11-05T23:59:30Z"),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("first pass %d", n)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(blockLine("u2", "msg_1", "req_1", "tool_use", `{"input_tokens":10,"output_tokens":5}`, "2026-11-06T00:00:20Z"))
	f.Close()
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("the second pass wrote %d rows; it must rewrite its own", n)
	}
	files := rowFilesOf(dir)
	if len(files) != 1 || filepath.Base(filepath.Dir(files[0])) != "2026-11-06" {
		t.Fatalf("the row did not move to the day folder of its new stamp: %v", files)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	if usage["output_tokens"] != float64(25) || usage["input_tokens"] != float64(10) {
		t.Fatalf("a later pass must never shrink the row: %v", usage)
	}
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("a third pass wrote %d rows; the cursor passed the response", n)
	}
	if got := len(rowFilesOf(dir)); got != 1 {
		t.Fatalf("rows = %d, want 1", got)
	}
}

func TestRecordTranscriptIsIdempotentAndResumesFromTheCursor(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t, transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("first pass %d", n)
	}
	// A second Stop with nothing new writes nothing.
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("second pass %d", n)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// A half-written line waits for its newline.
	f.WriteString(`{"type":"assistant","uuid":"u2","message":{"id":"msg_2"`)
	f.Close()
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("a partial line wrote a row")
	}
	f, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString(`,"model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n")
	f.Close()
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("after the newline %d", n)
	}
	if all := rows(t, dir); len(all) != 2 {
		t.Fatalf("%v", all)
	}
	// A transcript replaced under the same path is read from the start again,
	// and the row it already has is replaced, not duplicated.
	if err := os.WriteFile(path, []byte(transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	RecordTranscript("sub", "anthropic", path)
	all := rows(t, dir)
	if len(all) != 2 {
		t.Fatalf("a rewritten transcript must not spill duplicates: %v", all)
	}
	files := rowFilesOf(dir)
	if len(files) != 2 {
		t.Fatalf("spill files: %v", files)
	}
}

func TestRecordTranscriptLeavesNoCredentialAndNoBody(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	line := `{"type":"assistant","uuid":"u1","timestamp":"2026-11-05T10:00:00Z","message":{"id":"msg_1",` +
		`"model":"claude-opus-5-5","content":[{"type":"text","text":"password=canary-secret"}],` +
		`"usage":{"input_tokens":10,"output_tokens":25,"service_tier":"standard"}}}` + "\n"
	path := writeTranscript(t, line)
	RecordTranscript("sub", "anthropic", path)
	raw := rows(t, dir)[0]
	for _, secret := range []string{"canary-secret", "password", "service_tier"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("row carries %q: %s", secret, raw)
		}
	}
}

func TestRecordTranscriptWithoutACachedPriceStillWritesTheRow(t *testing.T) {
	dir := stateHome(t) // no seeded price slice, and the hook never fetches one
	path := writeTranscript(t, transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("%d", n)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	if _, priced := usage["estimated_cost"]; priced {
		t.Fatalf("no cached price means no estimate, never a fetch: %v", usage)
	}
	if usage["input_tokens"] != float64(10) {
		t.Fatalf("%v", usage)
	}
}

func TestARowAnotherWriterRecordedIsNotDuplicated(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	model := "claude-opus-5-5"
	// A Remote Control session (or a launch nested in one) relays the traffic
	// through ccmorph's adapter, which writes the row for this response.
	other := newSidecar("sub", "adapter")
	other.prices = CachedOwnedPriceBook("anthropic")
	rec := other.start("msg_1", nil, &model, usageRow{{"input_tokens", []byte("10")}})
	// The relay stamps its row when the turn begins; the reply lands later.
	rec.timestamp = "2026-11-05T09:59:59+00:00"
	rec.complete = true
	if err := other.write(rec); err != nil {
		t.Fatal(err)
	}
	before := rows(t, dir)
	if len(before) != 1 {
		t.Fatalf("%v", before)
	}
	path := writeTranscript(t, transcriptLine("u1", "msg_1", model, "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("the hook rewrote a row another writer owns: %d", n)
	}
	after := rows(t, dir)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("the adapter's row changed: %v -> %v", before, after)
	}
}

// A row that could not be written sits before the cursor forever unless the
// cursor holds: a full or unwritable state dir must not lose that response.
func TestRecordTranscriptRetriesARowItCouldNotWrite(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A regular file where the usage dir belongs makes every row write fail.
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, transcriptLine("u1", "msg_1", "claude-opus-5-5", "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("an unwritable usage dir wrote %d rows", n)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("the next pass wrote %d rows; the cursor passed a row it could not write", n)
	}
	if all := rows(t, dir); len(all) != 1 {
		t.Fatalf("%v", all)
	}
}

// A gateway that mints the same response id for two responses puts the later
// one in a spill; a later hook pass must fold its line into that spill, not into
// the primary, or the row shrinks to the last block's numbers.
func TestARecycledIdsSpillRowNeverShrinks(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_a", "text", `{"input_tokens":10,"output_tokens":25}`),
		blockLine("u2", "msg_1", "req_b", "text", `{"input_tokens":3,"output_tokens":40}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 2 {
		t.Fatalf("first pass %d", n)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(blockLine("u3", "msg_1", "req_b", "tool_use", `{"input_tokens":3,"output_tokens":5}`))
	f.Close()
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("the second pass wrote %d rows; it must rewrite its own", n)
	}
	files := rowFilesOf(dir)
	if len(files) != 2 {
		t.Fatalf("spill files: %v", files)
	}
	var spilled map[string]any
	for _, raw := range rows(t, dir) {
		row := decodeRow(t, raw)
		if row["usage"].(map[string]any)["input_tokens"] == float64(3) {
			spilled = row
		}
	}
	if spilled == nil {
		t.Fatalf("the second response's row is gone: %v", rows(t, dir))
	}
	if got := spilled["usage"].(map[string]any)["output_tokens"]; got != float64(40) {
		t.Fatalf("a later pass shrank the spill row: output_tokens = %v, want 40", got)
	}
}

// A gateway that recycles a response id across routes: another profile's
// adapter holds the primary name for an id this session's transcript also
// carries, and the hook must still record its own response — in a spill.
func TestARecycledIdAnotherRouteHoldsIsStillRecorded(t *testing.T) {
	dir := stateHome(t)
	model := "claude-opus-5-5"
	other := newSidecar("gw", "adapter")
	other.prices = CachedOwnedPriceBook("anthropic")
	rec := other.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte("7")}})
	rec.timestamp = "2026-11-01T09:00:00+00:00"
	rec.complete = true
	if err := other.write(rec); err != nil {
		t.Fatal(err)
	}
	before := rows(t, dir)
	if len(before) != 1 {
		t.Fatalf("%v", before)
	}
	path := writeTranscript(t, transcriptLine("u1", "resp_1", model, "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("codex", "anthropic", path); n != 1 {
		t.Fatalf("a recycled id another route holds lost this response: %d", n)
	}
	after := rows(t, dir)
	if len(after) != 2 {
		t.Fatalf("rows = %d, want 2: %v", len(after), after)
	}
	kept := false
	for _, raw := range after {
		if raw == before[0] {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("the other route's row changed: %v -> %v", before, after)
	}
}

// A response reaches the store from two writers: the adapter that relays the
// session's traffic and the hook that reads its transcript. They are one
// response when they name one request — the id and the input-side counts —
// however long the turn ran, whichever profile wrote first, and whichever name
// holds the row. A recycled id is another request under the same id, and both
// rows stay.
func TestAnotherRoutesRowForTheSameId(t *testing.T) {
	model := "claude-opus-5-5"
	cases := []struct {
		name                                      string
		otherProfile, otherTime, otherInput, hook string
		wantWritten, wantRows                     int
	}{
		{
			name:         "a reply landing long after its turn began is one row",
			otherProfile: "gw", otherTime: "2026-11-05T09:30:00+00:00", otherInput: "10",
			hook: "sub", wantWritten: 0, wantRows: 1,
		},
		{
			name:         "a recycled id inside the old window is a different response",
			otherProfile: "gw", otherTime: "2026-11-05T09:55:00+00:00", otherInput: "7",
			hook: "codex", wantWritten: 1, wantRows: 2,
		},
		{
			name:         "a recycled id of the same profile is a different response",
			otherProfile: "sub", otherTime: "2026-11-01T09:00:00+00:00", otherInput: "7",
			hook: "sub", wantWritten: 1, wantRows: 2,
		},
		{
			name:         "a row stamped after the reply landed is a later response",
			otherProfile: "gw", otherTime: "2026-11-05T10:05:00+00:00", otherInput: "10",
			hook: "sub", wantWritten: 1, wantRows: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := stateHome(t)
			seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
			other := newSidecar(c.otherProfile, "adapter")
			other.prices = CachedOwnedPriceBook("anthropic")
			rec := other.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte(c.otherInput)}})
			rec.timestamp = c.otherTime
			rec.complete = true
			if err := other.write(rec); err != nil {
				t.Fatal(err)
			}
			path := writeTranscript(t, transcriptLine("u1", "resp_1", model, "2026-11-05T10:00:00Z",
				`{"input_tokens":10,"output_tokens":25}`))
			if n := RecordTranscript(c.hook, "anthropic", path); n != c.wantWritten {
				t.Fatalf("rows written %d, want %d", n, c.wantWritten)
			}
			if got := len(rows(t, dir)); got != c.wantRows {
				t.Fatalf("rows on disk %d, want %d", got, c.wantRows)
			}
		})
	}
}

// The recycled id's old row is the same profile's, so the old same-profile
// shortcut read it as this response and the cursor passed a response with no
// row of its own. The new response is written beside the old one, and the
// cursor only moves past a response whose row is on disk.
func TestARecycledIdOfTheSameProfileDoesNotSwallowTheRow(t *testing.T) {
	dir := stateHome(t)
	model := "claude-opus-5-5"
	other := newSidecar("sub", "adapter")
	other.prices = CachedOwnedPriceBook("anthropic")
	rec := other.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte("7")}})
	rec.timestamp = "2026-11-01T09:00:00+00:00"
	rec.complete = true
	if err := other.write(rec); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, transcriptLine("u1", "resp_1", model, "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("a recycled id of the same profile lost this response: %d", n)
	}
	all := rows(t, dir)
	if len(all) != 2 {
		t.Fatalf("rows = %d, want 2: %v", len(all), all)
	}
	if !strings.Contains(strings.Join(all, "\n"), `"input_tokens":10`) {
		t.Fatalf("the new response's row is not on disk: %v", all)
	}
	// The cursor passed the response only because its row landed: a second pass
	// writes nothing and adds nothing.
	if n := RecordTranscript("sub", "anthropic", path); n != 0 {
		t.Fatalf("second pass wrote %d rows", n)
	}
	if got := len(rows(t, dir)); got != 2 {
		t.Fatalf("rows = %d, want 2", got)
	}
}

// reportOutput is what the report prints for the window, captured from stdout.
func reportOutput(t *testing.T, days float64) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var b strings.Builder
		io.Copy(&b, r)
		done <- b.String()
	}()
	report(days)
	os.Stdout = old
	w.Close()
	out := <-done
	r.Close()
	return out
}

// A windowed report reads the day folders its window touches and the rows a
// pre-shard store still holds flat, never a folder outside the window: the day
// folder is the index that keeps a time-window read from opening every row ever
// written (#227).
func TestAWindowedReportReadsTheDayFoldersItTouches(t *testing.T) {
	dir := stateHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	row := func(name, folder, task string, stamped time.Time, cost string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, folder), 0o700); err != nil {
			t.Fatal(err)
		}
		raw := `{"response_id":"` + name + `"`
		if task != "" {
			raw += `,"task":"` + task + `"`
		}
		raw += `,"served_model":"sonnet-x","timestamp":"` +
			stamped.Format(time.RFC3339Nano) + `","usage":{"input_tokens":5,"cost":` + cost +
			`},"complete":true}`
		if err := os.WriteFile(filepath.Join(dir, folder, name+".json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	today, old := now.Format("2006-01-02"), now.AddDate(0, 0, -3).Format("2006-01-02")
	row("sharded", today, "", now.Add(-30*time.Minute), "2") // in the window, in its day folder
	row("flat", "", "", now.Add(-20*time.Minute), "3")       // in the window, still flat
	row("before", old, "", now.Add(-72*time.Hour), "5")
	if out := reportOutput(t, 1); !strings.Contains(out, "total spend $5.00") {
		t.Fatalf("the report did not read its window's day folders beside the flat rows: %s", out)
	}
	// A task named by a verdict in the window keeps its whole-task cost: its
	// row before the window lives in an older folder, and the task read still
	// reaches it while a run with no verdict opens only its window.
	row("task-old", old, "t", now.Add(-72*time.Hour), "5")
	row("task-new", today, "t", now.Add(-20*time.Minute), "1")
	outcomes := `{"task":"t","outcome":"accepted","timestamp":"` + now.Format(time.RFC3339) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(paths.StateDir(), "outcomes.jsonl"), []byte(outcomes), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := reportOutput(t, 1); !strings.Contains(out, "  accepted 1  $6.00/task") {
		t.Fatalf("a task report lost the task's pre-window usage: %s", out)
	}
}

// A response the store holds under two names (the flat copy and the day
// folder's, left by a move that stopped halfway) is one response: the report
// counts it once, while a recycled id's other turn at the same stamp is still
// its own response (#298).
func TestAResponseInBothStoresIsCountedOnce(t *testing.T) {
	dir := stateHome(t)
	now := time.Now().UTC()
	today := filepath.Join(dir, now.Format("2006-01-02"))
	if err := os.MkdirAll(today, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	body := func(turn, cost string) []byte {
		return []byte(`{"schema":1,"response_id":"resp_1","turn_id":"` + turn + `","served_model":"sonnet-x","timestamp":"` +
			stamp + `","usage":{"input_tokens":5,"cost":` + cost + `},"complete":true}`)
	}
	for _, path := range []string{filepath.Join(dir, "resp_1.json"), filepath.Join(today, "resp_1.json")} {
		if err := os.WriteFile(path, body("turn_a", "2"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(today, "resp_1.turn_b.json"), body("turn_b", "3"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := reportOutput(t, 1); !strings.Contains(out, "total spend $5.00") {
		t.Fatalf("a response in both stores was not counted exactly once: %s", out)
	}
}

// The store's row files for a window: the flat rows plus the day folders whose
// day falls inside it, and nothing else — an older day folder, a newer one, a
// folder that is no day shard, the atomic writer's in-flight tmp.
func TestUsageRowFilesSelectsTheWindowsDayFolders(t *testing.T) {
	base := t.TempDir()
	write := func(name string) string {
		t.Helper()
		path := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	inWindow := write(filepath.Join("2026-11-08", "a.json"))
	write(filepath.Join("2026-11-08", "tmp492.tmp")) // the atomic writer's in-flight name
	write(filepath.Join("2026-11-05", "b.json"))     // a day before the window
	write(filepath.Join("2026-11-20", "c.json"))     // a day after it
	write(filepath.Join("not-a-day", "d.json"))      // a folder that is no day shard
	flat := write("e.json")
	since := time.Date(2026, 11, 7, 18, 0, 0, 0, time.UTC)
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	got := usageRowFiles(base, since, now)
	want := []string{flat, inWindow}
	for _, list := range [][]string{got, want} {
		sort.Strings(list)
	}
	if len(got) != 2 || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("usageRowFiles = %v, want %v", got, want)
	}
}

// When another turn holds the primary name, the relaying adapter's row for this
// response lands in a spill — a name the hook cannot derive from the id. The
// hook must still find it, or the response is counted twice and the report sums
// both rows.
func TestAReplyTheAdapterSpilledIsNotWrittenAgain(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	model := "claude-opus-5-5"
	// An old session of the same route still holds the primary for the id.
	old := newSidecar("gw", "adapter")
	old.prices = CachedOwnedPriceBook("anthropic")
	rec := old.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte("7")}})
	rec.timestamp = "2026-11-01T09:00:00+00:00"
	rec.complete = true
	if err := old.write(rec); err != nil {
		t.Fatal(err)
	}
	// The adapter relaying this session's traffic writes the same response to a
	// spill, stamped when the turn began.
	relay := newSidecar("gw", "adapter")
	relay.prices = CachedOwnedPriceBook("anthropic")
	rec = relay.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte("10")}})
	rec.timestamp = "2026-11-05T09:50:00+00:00"
	rec.complete = true
	if err := relay.write(rec); err != nil {
		t.Fatal(err)
	}
	if got := len(rows(t, dir)); got != 2 {
		t.Fatalf("setup rows = %d, want 2: %v", got, rows(t, dir))
	}
	path := writeTranscript(t, transcriptLine("u1", "resp_1", model, "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("codex", "anthropic", path); n != 0 {
		t.Fatalf("the hook rewrote a reply the adapter had recorded in a spill: %d", n)
	}
	if got := len(rows(t, dir)); got != 2 {
		t.Fatalf("rows = %d, want 2: %v", got, rows(t, dir))
	}
	if out := reportOutput(t, 3650); !strings.Contains(out, "over 2 rows") {
		t.Fatalf("the report does not sum the response once: %s", out)
	}
}

// A spill exists only while its primary is held: the sidecar claims the primary
// first, and nothing removes a primary. With no primary there is no row to
// match, so the hook must read that one name and move on — an orphan spill
// beside a new response's id must not suppress it, and a new response must not
// send the hook reading every row ever written.
func TestANewResponseReadsOnlyItsPrimary(t *testing.T) {
	dir := stateHome(t)
	model := "claude-opus-5-5"
	other := newSidecar("gw", "adapter")
	other.prices = CachedOwnedPriceBook("anthropic")
	rec := other.start("resp_1", nil, &model, usageRow{{"input_tokens", []byte("10")}})
	rec.timestamp = "2026-11-05T09:50:00+00:00"
	rec.complete = true
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The name the sidecar gives a spill, with no primary beside it.
	spill := filepath.Join(dir, rowFileName("resp_1."+rec.turnID))
	if err := os.WriteFile(spill, other.row(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTranscript(t, transcriptLine("u1", "resp_1", model, "2026-11-05T10:00:00Z",
		`{"input_tokens":10,"output_tokens":25}`))
	if n := RecordTranscript("codex", "anthropic", path); n != 1 {
		t.Fatalf("a spill with no primary suppressed the response: %d", n)
	}
	if got := len(rows(t, dir)); got != 2 {
		t.Fatalf("rows = %d, want 2: %v", got, rows(t, dir))
	}
}

func TestMaxUsageKeepsTheLargerNumberPerKey(t *testing.T) {
	a := usageRow{
		{"input_tokens", []byte("10")},
		{"output_tokens", []byte("5")},
		{"cache_creation", []byte(`{"ephemeral_5m_input_tokens":7}`)},
	}
	b := usageRow{
		{"output_tokens", []byte("25")},
		{"cache_read_input_tokens", []byte("3")},
		{"cache_creation", []byte(`{"ephemeral_5m_input_tokens":9}`)},
	}
	got := maxUsage(a, b)
	want := `{"input_tokens":10,"output_tokens":25,"cache_creation":{"ephemeral_5m_input_tokens":9},"cache_read_input_tokens":3}`
	if string(got.json()) != want {
		t.Fatalf("maxUsage = %s, want %s", got.json(), want)
	}
	// Neither input is modified.
	if string(a[1].raw) != "5" || string(a[2].raw) != `{"ephemeral_5m_input_tokens":7}` || len(b) != 3 {
		t.Fatalf("maxUsage mutated its inputs: %v %v", a, b)
	}
}

// Claude Code writes one transcript line per content block of a response, all
// with the same message.id and requestId: one response, one row.
func TestOneResponseSplitAcrossLinesIsOneRow(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_1", "thinking", `{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":0}`),
		blockLine("u2", "msg_1", "req_1", "text", `{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":0}`),
		blockLine("u3", "msg_1", "req_1", "tool_use", `{"input_tokens":10,"output_tokens":25,"cache_read_input_tokens":0}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("a three-block response wrote %d rows", n)
	}
	files := rowFilesOf(dir)
	if len(files) != 1 {
		t.Fatalf("spill files: %v", files)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	// (10*4 + 25*20) / 1e6, once — not three times.
	if usage["input_tokens"] != float64(10) || usage["output_tokens"] != float64(25) || usage["estimated_cost"] != 0.00054 {
		t.Fatalf("%v", usage)
	}
}

func TestSplitResponseKeepsTheLargestUsage(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_1", "thinking", `{"input_tokens":10,"output_tokens":1}`),
		blockLine("u2", "msg_1", "req_1", "text", `{"input_tokens":10,"output_tokens":25}`),
		blockLine("u3", "msg_1", "req_1", "tool_use", `{"input_tokens":10,"output_tokens":7}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("%d", n)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	if usage["output_tokens"] != float64(25) || usage["input_tokens"] != float64(10) {
		t.Fatalf("the row must carry the response's final counts: %v", usage)
	}
}

func TestTwoResponsesAreTwoRows(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_1", "text", `{"input_tokens":10,"output_tokens":25}`),
		blockLine("u2", "msg_1", "req_1", "tool_use", `{"input_tokens":10,"output_tokens":25}`),
		blockLine("u3", "msg_2", "req_2", "text", `{"input_tokens":3,"output_tokens":4}`),
		blockLine("u4", "msg_2", "req_2", "tool_use", `{"input_tokens":3,"output_tokens":4}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 2 {
		t.Fatalf("%d", n)
	}
	files := rowFilesOf(dir)
	if len(files) != 2 {
		t.Fatalf("spill files: %v", files)
	}
}

// A Stop may read the transcript before a response's last block landed: the
// next pass folds its line into the row the first one wrote.
func TestSplitResponseAcrossTwoPassesStaysOneRow(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "anthropic", map[string]price{"claude-opus-5-5": {Input: 4, Output: 20}})
	path := writeTranscript(t,
		blockLine("u1", "msg_1", "req_1", "thinking", `{"input_tokens":10,"output_tokens":25}`),
		blockLine("u2", "msg_1", "req_1", "text", `{"input_tokens":10,"output_tokens":25}`),
	)
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("first pass %d", n)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(blockLine("u3", "msg_1", "req_1", "tool_use", `{"input_tokens":10,"output_tokens":5}`))
	f.Close()
	if n := RecordTranscript("sub", "anthropic", path); n != 1 {
		t.Fatalf("the second pass wrote %d rows; it must rewrite its own", n)
	}
	files := rowFilesOf(dir)
	if len(files) != 1 {
		t.Fatalf("spill files: %v", files)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	if usage["output_tokens"] != float64(25) || usage["input_tokens"] != float64(10) {
		t.Fatalf("a later pass must never shrink the row: %v", usage)
	}
}

// A transcript a launch resumes may be one no ccmorph hook ever read: created
// before this release, by native claude, or under a route that writes no rows.
// Its history was another profile's spend; only the turns this launch serves
// are its own, so SessionStart marks the file's end as the read position.
func TestAResumedTranscriptChargesOnlyTheNewTurns(t *testing.T) {
	dir := stateHome(t)
	now := time.Now().UTC()
	model := "claude-opus-5-5"
	path := writeTranscript(t, transcriptLine("u1", "msg_old", model, now.Add(-time.Hour).Format(time.RFC3339Nano),
		`{"input_tokens":1000,"output_tokens":500}`))
	start := `{"session_id":"s1","source":"resume","hook_event_name":"SessionStart","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(start)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if all := rows(t, dir); len(all) != 0 {
		t.Fatalf("SessionStart wrote rows: %v", all)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(transcriptLine("u2", "msg_new", model, now.Add(time.Minute).Format(time.RFC3339Nano),
		`{"input_tokens":10,"output_tokens":25}`))
	f.Close()
	stop := `{"session_id":"s1","hook_event_name":"Stop","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(stop)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	all := rows(t, dir)
	if len(all) != 1 || decodeRow(t, all[0])["response_id"] != "msg_new" {
		t.Fatalf("a resumed transcript charged its prior history: %v", all)
	}
}

// A fork copies the history into a file this launch created, so the cursor's
// offset alone cannot tell old lines from new: the cursor's start time does.
func TestAForkedTranscriptsCopiedHistoryIsNotCharged(t *testing.T) {
	dir := stateHome(t)
	now := time.Now().UTC()
	model := "claude-opus-5-5"
	path := filepath.Join(t.TempDir(), "fork.jsonl")
	start := `{"session_id":"s1","source":"resume","hook_event_name":"SessionStart","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(start)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if err := os.WriteFile(path, []byte(
		transcriptLine("u1", "msg_old", model, now.Add(-time.Hour).Format(time.RFC3339Nano), `{"input_tokens":1000,"output_tokens":500}`)+
			transcriptLine("u2", "msg_new", model, now.Add(time.Minute).Format(time.RFC3339Nano), `{"input_tokens":10,"output_tokens":25}`)), 0o600); err != nil {
		t.Fatal(err)
	}
	stop := `{"session_id":"s1","hook_event_name":"Stop","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(stop)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	all := rows(t, dir)
	if len(all) != 1 || decodeRow(t, all[0])["response_id"] != "msg_new" {
		t.Fatalf("a fork charged its copied history: %v", all)
	}
}

// A compact starts a new session file the launch was already reading, or
// resumes one it started after: an existing cursor is the read position, and
// SessionStart must not move it.
func TestSessionStartKeepsAnExistingCursor(t *testing.T) {
	dir := stateHome(t)
	now := time.Now().UTC()
	model := "claude-opus-5-5"
	path := writeTranscript(t, transcriptLine("u1", "msg_1", model, now.Add(-time.Minute).Format(time.RFC3339Nano),
		`{"input_tokens":10,"output_tokens":25}`))
	stop := `{"session_id":"s1","hook_event_name":"Stop","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(stop)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(transcriptLine("u2", "msg_2", model, now.Add(time.Minute).Format(time.RFC3339Nano),
		`{"input_tokens":3,"output_tokens":4}`))
	f.Close()
	start := `{"session_id":"s1","source":"compact","hook_event_name":"SessionStart","transcript_path":` + quote(path) + `}`
	if rc := UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(start)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	UsageRowMain([]string{"--profile", "sub", "--pricing", "anthropic"}, strings.NewReader(stop))
	if all := rows(t, dir); len(all) != 2 {
		t.Fatalf("a compact's SessionStart moved the cursor: %v", all)
	}
}

func TestUsageRowMainReadsTheHookInput(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "any", map[string]price{"gpt-5.6-sol": {Input: 2, Output: 10}})
	path := writeTranscript(t, transcriptLine("u1", "resp_1", "gpt-5.6-sol", "2026-11-05T10:00:00Z",
		`{"input_tokens":1000,"output_tokens":500}`))
	in := `{"session_id":"s1","transcript_path":` + quote(path) + `,"hook_event_name":"Stop"}`
	if rc := UsageRowMain([]string{"--profile", "codex", "--pricing", "any"}, strings.NewReader(in)); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	row := decodeRow(t, rows(t, dir)[0])
	if row["profile"] != "codex" || row["served_model"] != "gpt-5.6-sol" {
		t.Fatalf("%v", row)
	}
	if row["usage"].(map[string]any)["estimated_cost"] != 0.007 {
		t.Fatalf("%v", row["usage"])
	}
	// Nothing to read is a quiet success: a hook must never fail a turn.
	if rc := UsageRowMain([]string{"--profile", "codex"}, strings.NewReader("{not json")); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if rc := UsageRowMain([]string{"--profile", "codex"}, strings.NewReader("")); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if rc := UsageRowMain([]string{"--profile=", "--pricing=any"}, strings.NewReader(in)); rc != 2 {
		t.Fatalf("an unnamed profile is a usage error, rc %d", rc)
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A provider launch's hook passes --pricing any: every provider, not one named "any".
func TestOwnedPriceBookAnyMeansEveryProvider(t *testing.T) {
	api := []byte(`{"openai":{"npm":"@ai-sdk/openai","models":{"gpt-x":{"cost":{"input":2,"output":10}}}}}`)
	if got := newOwnedPriceBook("any").parse(api); got["gpt-x"].Input != 2 {
		t.Fatalf("any priced nothing: %v", got)
	}
	if got := newOwnedPriceBook("anthropic").parse(api); len(got) != 0 {
		t.Fatalf("a named provider priced another's model: %v", got)
	}
}

func TestRecordTranscriptCarriesAValidSessionIDOnly(t *testing.T) {
	dir := stateHome(t)
	line := func(id, sess string) string {
		return strings.Replace(transcriptLine("u-"+id, id, "m", "2026-11-05T10:00:00Z", `{"input_tokens":1}`),
			`{"type":"assistant",`, `{"type":"assistant","sessionId":"`+sess+`",`, 1)
	}
	path := writeTranscript(t, line("msg_ok", "sess-1"), line("msg_bad", "a b/c"))
	if n := RecordTranscript("sub", "", path); n != 2 {
		t.Fatalf("rows written %d", n)
	}
	for _, raw := range rows(t, dir) {
		row := decodeRow(t, raw)
		sess, has := row["session"]
		if (row["response_id"] == "msg_ok") != has || (has && sess != "sess-1") {
			t.Fatalf("%v", row)
		}
	}
}
