package shim

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// answering is an upstream that plays one scripted answer per call (the last one
// repeats) and records the bodies it received.
type answering struct {
	mu     sync.Mutex
	script []string // "400", "400 sse", "200"
	bodies []string
}

func (a *answering) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	n := len(a.bodies)
	a.bodies = append(a.bodies, string(b))
	step := a.script[min(n, len(a.script)-1)]
	a.mu.Unlock()
	if strings.HasSuffix(step, "sse") {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(step, "400") {
		w.WriteHeader(400)
		w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`))
		return
	}
	w.Write([]byte(`{"id":"msg_ok","type":"message","content":[],"usage":{"input_tokens":1}}`))
}

const resendOriginal = `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"t"}]}]}`
const resendReplaced = `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"."}]}]}`

// swapOnce resends with resendReplaced, but only a request that still carries
// the original, and counts how often it is asked.
func swapOnce(asked *int) Hooks {
	return Hooks{ResendBody: func(req, _ []byte) []byte {
		*asked++
		if string(req) != resendOriginal {
			return nil
		}
		return []byte(resendReplaced)
	}}
}

func TestResendAfter400(t *testing.T) {
	for _, tc := range []struct {
		name        string
		script      []string
		body        string
		path        string
		status      int
		wantBodies  []string
		wantAsked   int
		withoutHook bool
	}{
		{"400 then success", []string{"400", "200"}, resendOriginal, "/v1/messages", 200, []string{resendOriginal, resendReplaced}, 1, false},
		{"400 twice reaches the client", []string{"400", "400"}, resendOriginal, "/v1/messages", 400, []string{resendOriginal, resendReplaced}, 1, false},
		{"stream content type", []string{"400 sse", "200"}, resendOriginal, "/v1/messages", 200, []string{resendOriginal, resendReplaced}, 1, false},
		{"nothing to strip means no resend", []string{"400", "200"}, `{"messages":[]}`, "/v1/messages", 400, []string{`{"messages":[]}`}, 1, false},
		{"other paths never resend", []string{"400", "200"}, resendOriginal, "/v1/messages/count_tokens", 400, []string{resendOriginal}, 0, false},
		{"no hook means no resend", []string{"400", "200"}, resendOriginal, "/v1/messages", 400, []string{resendOriginal}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &answering{script: tc.script}
			srv := httptest.NewServer(up)
			defer srv.Close()
			s := NewServer(srv.URL)
			asked := 0
			if !tc.withoutHook {
				s.Hooks = swapOnce(&asked)
			}
			resp := post(t, front(t, s), tc.path, tc.body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d", resp.StatusCode)
			}
			if strings.Join(up.bodies, "\n") != strings.Join(tc.wantBodies, "\n") || asked != tc.wantAsked {
				t.Fatalf("bodies %q asked %d", up.bodies, asked)
			}
		})
	}
}

func TestResendIsNotATransportRetryAndNotCountedByTheBreaker(t *testing.T) {
	up := &answering{script: []string{"400", "200"}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	r := newRetrying(t, srv.URL)
	asked := 0
	r.s.Hooks = swapOnce(&asked)
	resp := post(t, r.url, "/v1/messages", resendOriginal)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(up.bodies) != 2 {
		t.Fatalf("status %d bodies %d", resp.StatusCode, len(up.bodies))
	}
	if len(r.delays) != 0 || len(r.s.Breaker.failures) != 0 {
		t.Fatalf("delays %v failures %v", r.delays, r.s.Breaker.failures)
	}
}

func TestPanickingResendHookSendsNothingMore(t *testing.T) {
	up := &answering{script: []string{"400", "200"}}
	srv := httptest.NewServer(up)
	defer srv.Close()
	s := NewServer(srv.URL)
	s.Hooks = Hooks{ResendBody: func(_, _ []byte) []byte { panic("boom") }}
	resp := post(t, front(t, s), "/v1/messages", resendOriginal)
	resp.Body.Close()
	if resp.StatusCode != 400 || len(up.bodies) != 1 {
		t.Fatalf("status %d bodies %d", resp.StatusCode, len(up.bodies))
	}
}
