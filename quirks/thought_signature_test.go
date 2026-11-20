package quirks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/akleizor/ccmorph/shim"
)

// #646: the second request of a Gemini tool turn, as Claude Code replays it: the
// translator's thought-signature carrier (a thinking block signed
// "cliproxy-reasoning-details:<base64>"), the tool_use it belongs to, and the
// tool_result. The translator turns the carrier back into reasoning_details.
const carrierRequest = `{"model":"gemini-4-carbon","max_tokens":32000,"stream":true,` +
	`"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[` +
	`{"role":"user","content":"run echo hi with Bash"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"Bash"},` +
	`{"type":"tool_use","id":"call_bash","name":"Bash","input":{"command":"echo hi"}},` +
	`{"type":"thinking","thinking":".","signature":"cliproxy-reasoning-details:W3sidHlwZSI6InRvb2wiLCJ0ZXh0IjoiQmFzaCIsInNpZ25hdHVyZSI6IlNJRy1CQVNIIn1d"}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_bash","content":"hi"}]}]}`

// resendWithoutThinking runs only after a 400. On a turn the upstream accepts,
// the stock hooks send the carrier upstream byte for byte, once.
func TestStockHooksSendTheThoughtSignatureCarrierUntouched(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	h, err := build("", "")
	if err != nil {
		t.Fatal(err)
	}
	s := shim.NewServer(up.URL)
	s.Hooks = h
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(carrierRequest))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(bodies) != 1 || bodies[0] != carrierRequest {
		t.Fatalf("upstream got %d bodies: %q", len(bodies), bodies)
	}
}

// vertexSignatureRefusal is what the gateway relays when the signature did not
// come back on the previous tool call.
const vertexSignatureRefusal = `{"error":{"code":400,"message":"Google Vertex bad request: Function call is missing a thought_signature in functionCall parts. function call ` + "`default_api:Bash`" + `, position 3","status":"INVALID_ARGUMENT"}}`

// On a route with no translator chat models (a direct, native Anthropic route)
// the resend strips the carrier with every other thinking block, as that route
// needs. When the stripped body is refused too, the client must still see the
// upstream's FIRST refusal, never the thought_signature complaint the strip
// provoked (#646). The translator chat route keeps the carrier instead:
// TestTheResendKeepsTheCarrierThroughTheShim.
func TestAFailedNativeResendKeepsTheFirstRefusal(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		if n == 0 {
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: 32000 is more than this model accepts"}}`))
			return
		}
		w.Write([]byte(vertexSignatureRefusal))
	}))
	defer up.Close()
	h, err := build("", "")
	if err != nil {
		t.Fatal(err)
	}
	s := shim.NewServer(up.URL)
	s.Hooks = h
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(carrierRequest))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "max_tokens: 32000") || strings.Contains(string(body), "thought_signature") {
		t.Fatalf("the client saw the wrong refusal: %s", body)
	}
	// The hook really ran: the resend went out, and on this route without the carrier.
	if len(bodies) != 2 {
		t.Fatalf("upstream saw %d bodies, want the first request and one resend", len(bodies))
	}
	if strings.Contains(bodies[1], "cliproxy-reasoning-details:") {
		t.Fatalf("the resend kept the carrier: %s", bodies[1])
	}
}
