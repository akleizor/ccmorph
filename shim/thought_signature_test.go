package shim

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// #646: the translator (akleizor/CLIProxyAPI from v8.0.76-akleizor.4) carries
// Gemini's thought signatures to Claude Code as one thinking block whose
// signature is "cliproxy-reasoning-details:" + base64 of the gateway's
// reasoning_details, and turns it back into reasoning_details on the next
// request. These fixtures are that translator's exact output for one Bash call
// whose reasoning_details were [{"type":"tool","text":"Bash","signature":"SIG-BASH"}].
const carrierSignature = "cliproxy-reasoning-details:W3sidHlwZSI6InRvb2wiLCJ0ZXh0IjoiQmFzaCIsInNpZ25hdHVyZSI6IlNJRy1CQVNIIn1d"

// The streamed reply: the carrier is the last block, and it starts empty.
const carrierStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"c1","type":"message","role":"assistant","model":"gemini-4-carbon","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Bash"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_bash","name":"Bash","input":{}}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"echo hi\"}"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":1}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":2,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"thinking_delta","thinking":"."}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":2,"delta":{"type":"signature_delta","signature":"` + carrierSignature + `"}}` + "\n\n" +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":2}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":5,"output_tokens":5}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// The same reply, not streamed: the carrier comes first.
const carrierMessage = `{"id":"c1","type":"message","role":"assistant","model":"gemini-4-carbon","content":[` +
	`{"type":"thinking","thinking":".","signature":"` + carrierSignature + `"},` +
	`{"type":"thinking","thinking":"Bash"},` +
	`{"type":"tool_use","id":"call_bash","name":"Bash","input":{"command":"echo hi"}}],` +
	`"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":5}}`

func TestMalformedThinkingSparesTheThoughtSignatureCarrier(t *testing.T) {
	carrier := []byte(`{"type":"thinking","thinking":".","signature":"` + carrierSignature + `"}`)
	if malformedThinking(carrier, Span{Start: 0, End: len(carrier)}) {
		t.Fatal("the signed carrier block counts as malformed")
	}
	// Its stream start is empty and unsigned: held until the "." delta releases it.
	start := []byte(`{"type":"thinking","thinking":""}`)
	if !malformedThinking(start, Span{Start: 0, End: len(start)}) {
		t.Fatal("an empty unsigned start is no longer held")
	}
}

func TestThinkingStreamRelaysTheThoughtSignatureCarrierUnchanged(t *testing.T) {
	s := NewServer("")
	s.PreserveIDs = true
	if got := streamThrough(t, s, carrierStream, nil, ""); got != carrierStream {
		t.Fatalf("stream changed:\n%q\nwant\n%q", got, carrierStream)
	}
	s.PreserveIDs = false // the id is minted anew; the carrier is not touched
	if got := streamThrough(t, s, carrierStream, nil, ""); !strings.Contains(got, `"signature":"`+carrierSignature+`"`) ||
		strings.Count(got, `"type":"thinking"`) != 2 {
		t.Fatalf("carrier lost: %q", got)
	}
}

func TestRewriteKeepsTheThoughtSignatureCarrier(t *testing.T) {
	s := NewServer("http://x")
	s.PreserveIDs = true
	if out, changed := s.rewrite([]byte(carrierMessage), "", false); changed {
		t.Fatalf("message changed: %s", out)
	}
	s.PreserveIDs = false
	out, _ := s.rewrite([]byte(carrierMessage), "", false)
	if !strings.Contains(string(out), `{"type":"thinking","thinking":".","signature":"`+carrierSignature+`"}`) {
		t.Fatalf("carrier lost: %s", out)
	}
}

// vertexRefusal is the refusal the gateway relays verbatim for a Gemini tool
// call whose signature did not come back: a paragraph naming a tool position,
// with no repair in it.
const vertexRefusal = `{"error":{"code":400,"message":"Google Vertex bad request: Function call is missing a thought_signature in functionCall parts. function call ` + "`default_api:Bash`" + `, position 3","status":"INVALID_ARGUMENT"}}`

// refusing is an upstream that answers every POST with status and body.
func refusing(status int, body string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func postMessages(t *testing.T, s *Server) (int, string) {
	t.Helper()
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// oneLineError is the message of an Anthropic error body that is one line.
func oneLineError(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}
	if doc.Type != "error" || doc.Error.Type != "invalid_request_error" || strings.ContainsAny(doc.Error.Message, "\r\n") {
		t.Fatalf("not a one-line Anthropic error: %s", body)
	}
	if strings.Contains(body, "position 3") || strings.Contains(body, "default_api") {
		t.Fatalf("the raw Vertex text reached the client: %s", body)
	}
	if !strings.Contains(doc.Error.Message, "thought_signature") {
		t.Fatalf("the error does not name thought_signature: %s", body)
	}
	return doc.Error.Message
}

// A shim with no translator behind it names the gateway, never cliproxyapi.
func TestThoughtSignatureRefusalBecomesOneLine(t *testing.T) {
	up := refusing(400, vertexRefusal)
	defer up.Close()
	status, got := postMessages(t, NewServer(up.URL))
	if status != 400 {
		t.Fatalf("status %d", status)
	}
	msg := oneLineError(t, got)
	if !strings.Contains(msg, "no translator") || !strings.Contains(msg, "gateway") || strings.Contains(msg, "cliproxyapi") {
		t.Fatalf("a direct route must name the gateway: %s", msg)
	}
}

// Behind the translator the line names the cliproxyapi build that sends the
// signature back, and the command that installs it.
func TestThoughtSignatureRefusalBehindTheTranslatorNamesTheUpgrade(t *testing.T) {
	msg := oneLineError(t, string(thoughtSignatureRefusal([]byte(vertexRefusal), true)))
	for _, want := range []string{"cliproxyapi 8.0.76-akleizor.4", "brew install akleizor/tap/cliproxyapi", "cliproxyapi -h"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("the line lacks %q: %s", want, msg)
		}
	}
}

// Only Gemini's missing-signature refusal under a 400 is rewritten: a 400 that
// merely names the field, and the same refusal under another status, reach
// the client as the upstream sent them.
func TestOnlyTheMissingSignatureRefusalIsRewritten(t *testing.T) {
	unknownField := `{"error":{"code":400,"message":"Invalid JSON payload received. Unknown name \"thought_signature\" at 'contents[1].parts[0]': Cannot find field.","status":"INVALID_ARGUMENT"}}`
	for _, translator := range []bool{false, true} {
		if got := string(thoughtSignatureRefusal([]byte(unknownField), translator)); got != unknownField {
			t.Fatalf("translator=%v: rewrote a refusal that only names the field: %s", translator, got)
		}
	}
	for _, tc := range []struct {
		status int
		body   string
	}{{400, unknownField}, {422, vertexRefusal}} {
		up := refusing(tc.status, tc.body)
		status, got := postMessages(t, NewServer(up.URL))
		up.Close()
		if status != tc.status || got != tc.body {
			t.Fatalf("status %d: got %d %s", tc.status, status, got)
		}
	}
}

// The route spec says whether the shim fronts the translator: only a spec that
// names a translator config does.
func TestBehindTranslatorReadsTheRouteSpec(t *testing.T) {
	if (&Server{}).behindTranslator() {
		t.Fatal("a shim with no route spec is behind no translator")
	}
	for _, tc := range []struct {
		spec string
		want bool
	}{
		{`{"key_sha256":"k","timeout":5,"upstream":"https://gw"}`, false},
		{`{"key_sha256":"k","timeout":5,"upstream":"https://gw","adapter":"/bin/ccmorph-core","adapter_args":["adapter"],"config":"/tmp/c.yaml","port":4000}`, true},
	} {
		r, err := newModelRoutes(tc.spec, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got := (&Server{Routes: r}).behindTranslator(); got != tc.want {
			t.Fatalf("%s: behindTranslator %v, want %v", tc.spec, got, tc.want)
		}
	}
}

func TestAnOrdinaryRefusalIsEchoedUnchanged(t *testing.T) {
	body := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 32000 is more than this model accepts"}}`
	up := refusing(400, body)
	defer up.Close()
	status, got := postMessages(t, NewServer(up.URL))
	if status != 400 || !strings.Contains(got, "max_tokens: 32000") {
		t.Fatalf("status %d body %s", status, got)
	}
}

// A 400 that merely quotes the refusal — an invalid tool enum value, say — is
// not Gemini's missing-signature refusal and must reach the client as sent.
func TestAnUnrelatedRefusalQuotingTheWordsIsEchoed(t *testing.T) {
	body := `{"error":{"code":400,"message":"Invalid enum value. Supported values: [\"Function call is missing a thought_signature in functionCall parts.\"]","status":"INVALID_ARGUMENT"}}`
	up := refusing(400, body)
	defer up.Close()
	status, got := postMessages(t, NewServer(up.URL))
	if status != 400 || got != body {
		t.Fatalf("status %d: got %s", status, got)
	}
}

// A resend refused with a retryable status is passed through, never replaced by
// the first 400, and the breaker records the failure it saw.
func TestAResendRefusedWith429KeepsThe429(t *testing.T) {
	first := `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is too high"}}`
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			w.Write([]byte(first))
			return
		}
		w.WriteHeader(429)
		w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer up.Close()
	r := newRetrying(t, up.URL)
	r.s.Hooks = Hooks{ResendBody: func(_, _ []byte) []byte { return []byte(`{"messages":[]}`) }}
	resp := post(t, r.url, "/v1/messages", resendOriginal)
	resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status %d, want the 429", resp.StatusCode)
	}
	if len(r.s.Breaker.failures) == 0 {
		t.Fatal("the breaker recorded no failure for the 429")
	}
}
