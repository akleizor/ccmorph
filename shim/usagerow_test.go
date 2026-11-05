package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsonv2 "encoding/json/v2"
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
// response carries its own uuid, the same requestId, and the same message.
func blockLine(uuid, id, req, block, usage string) string {
	return `{"type":"assistant","uuid":"` + uuid + `","requestId":"` + req + `","timestamp":"2026-11-05T10:00:00Z",` +
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
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
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
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
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
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
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
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(files) != 1 {
		t.Fatalf("spill files: %v", files)
	}
	usage := decodeRow(t, rows(t, dir)[0])["usage"].(map[string]any)
	if usage["output_tokens"] != float64(25) || usage["input_tokens"] != float64(10) {
		t.Fatalf("a later pass must never shrink the row: %v", usage)
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
