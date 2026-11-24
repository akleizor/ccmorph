package quirks

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/akleizor/ccmorph/shim"
)

// chatRoute is the stock hooks as the adapter builds them behind the
// translator, where gemini-4-carbon is a chat-path model (#646).
func chatRoute(t *testing.T) shim.Hooks {
	t.Helper()
	h, err := FromGetenv(func(k string) string {
		if k == "CCMORPH_TRANSLATOR_CHAT_MODELS" {
			return "gpt-x, gemini-4-carbon"
		}
		return ""
	})
	if err != nil || h.ResendBody == nil {
		t.Fatal(h, err)
	}
	return h
}

// unsignedThinking and signedCarrier are the two thinking blocks in
// carrierRequest, each with the comma that joins it to its neighbour.
const (
	unsignedThinking = `{"type":"thinking","thinking":"Bash"},`
	signedCarrier    = `,{"type":"thinking","thinking":".","signature":"cliproxy-reasoning-details:W3sidHlwZSI6InRvb2wiLCJ0ZXh0IjoiQmFzaCIsInNpZ25hdHVyZSI6IlNJRy1CQVNIIn1d"}`
)

// On a translator chat route the resend drops the other thinking blocks and
// keeps the carrier, so Gemini still gets its thought signature back.
func TestTheResendKeepsTheCarrierOnATranslatorChatModel(t *testing.T) {
	got := string(chatRoute(t).ResendBody([]byte(carrierRequest), nil))
	want := strings.Replace(carrierRequest, unsignedThinking, "", 1)
	if got != want {
		t.Fatalf("resend\n%s\nwant\n%s", got, want)
	}
}

// With nothing but the carrier left to strip there is nothing to repair: no
// resend, so the client gets the upstream's own first answer.
func TestNoResendWhenOnlyTheCarrierIsLeft(t *testing.T) {
	onlyCarrier := strings.Replace(carrierRequest, unsignedThinking, "", 1)
	if got := chatRoute(t).ResendBody([]byte(onlyCarrier), nil); got != nil {
		t.Fatalf("resent %s", got)
	}
}

// A native Anthropic route rejects the carrier's signature, so wherever the
// translator's chat path is not the route the strip takes it too: a profile
// with no translator chat models, and a model the translator passes through.
func TestTheResendStripsTheCarrierOffEveryOtherRoute(t *testing.T) {
	stripped := strings.Replace(strings.Replace(carrierRequest, unsignedThinking, "", 1), signedCarrier, "", 1)
	h, err := FromGetenv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if got := string(h.ResendBody([]byte(carrierRequest), nil)); got != stripped {
		t.Fatalf("native route resend\n%s\nwant\n%s", got, stripped)
	}
	toClaude := strings.Replace(carrierRequest, `"model":"gemini-4-carbon"`, `"model":"claude-opus-4-5"`, 1)
	want := strings.Replace(stripped, `"model":"gemini-4-carbon"`, `"model":"claude-opus-4-5"`, 1)
	if got := string(chatRoute(t).ResendBody([]byte(toClaude), nil)); got != want {
		t.Fatalf("passthrough model resend\n%s\nwant\n%s", got, want)
	}
}

// Through the real shim: a 400 on a translator chat route is resent with the
// carrier still on, so an upstream that needs the signature accepts the resend.
func TestTheResendKeepsTheCarrierThroughTheShim(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case n == 0:
			w.WriteHeader(400)
			w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: thinking block not accepted"}}`))
		case !strings.Contains(string(b), "cliproxy-reasoning-details:"):
			w.WriteHeader(400)
			w.Write([]byte(vertexSignatureRefusal))
		default:
			w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		}
	}))
	defer up.Close()
	s := shim.NewServer(up.URL)
	s.Hooks = chatRoute(t)
	front := httptest.NewServer(s)
	defer front.Close()
	resp, err := http.Post(front.URL+"/v1/messages", "application/json", strings.NewReader(carrierRequest))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if len(bodies) != 2 || bodies[1] != strings.Replace(carrierRequest, unsignedThinking, "", 1) {
		t.Fatalf("upstream got %q", bodies)
	}
}

// The chat list is read when the resend runs, not when the hooks are built: a
// model the adapter prepares mid-session joins the list on disk, and the shim
// that started before it must keep that model's carrier (#646).
func TestTheChatListIsReadWhenTheResendRuns(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml.chat-models")
	if err := os.WriteFile(file, []byte("gpt-x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := FromGetenv(func(k string) string {
		if k == "CCMORPH_TRANSLATOR_CHAT_MODELS_FILE" {
			return file
		}
		return ""
	})
	if err != nil || h.ResendBody == nil {
		t.Fatal(h, err)
	}
	if got := string(h.ResendBody([]byte(carrierRequest), nil)); strings.Contains(got, "cliproxy-reasoning-details:") {
		t.Fatalf("kept the carrier before the model was listed: %s", got)
	}
	if err := os.WriteFile(file, []byte("gpt-x\ngemini-4-carbon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(carrierRequest, unsignedThinking, "", 1)
	if got := string(h.ResendBody([]byte(carrierRequest), nil)); got != want {
		t.Fatalf("resend\n%s\nwant\n%s", got, want)
	}
}

// GH-544: the chat list names bare ids, and Claude Code may send the model
// with its window marker in either case: the resend keeps the carrier for
// both spellings, as it does for the bare id.
func TestTheResendKeepsTheCarrierUnderEitherMarkerSpellingIssue544(t *testing.T) {
	for _, model := range []string{"gemini-4-carbon[1m]", "gemini-4-carbon[1M]"} {
		req := strings.Replace(carrierRequest, `"model":"gemini-4-carbon"`, `"model":"`+model+`"`, 1)
		got := string(chatRoute(t).ResendBody([]byte(req), nil))
		if want := strings.Replace(req, unsignedThinking, "", 1); got != want {
			t.Errorf("%s: resend\n%s\nwant\n%s", model, got, want)
		}
	}
}
