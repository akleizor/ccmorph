package shim

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// blocked is a provider that answers 503 while fails is positive, then a
// healthy JSON message. It records every body it received, so a test can prove
// what the shim put on the wire and how many times.
type blocked struct {
	mu     sync.Mutex
	fails  int
	sse    bool // failures answer as text/event-stream
	bodies []string
}

func (b *blocked) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	buf.ReadFrom(r.Body)
	b.mu.Lock()
	b.bodies = append(b.bodies, buf.String())
	fail := b.fails > 0
	if fail {
		b.fails--
	}
	b.mu.Unlock()
	if fail {
		if b.sse {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(503)
		w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"blocked"}}`))
		return
	}
	w.Write([]byte(`{"id":"msg_ok","type":"message","content":[],"usage":{"input_tokens":1}}`))
}

func (b *blocked) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.bodies)
}

const retryBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// retrying is a shim with --retry-transport in front of upstream, driven by an
// injected clock and sleep: neither the retry waits nor the breaker cost real time.
type retrying struct {
	t      *testing.T
	s      *Server
	url    string
	now    float64
	delays []time.Duration
}

func newRetrying(t *testing.T, target string) *retrying {
	r := &retrying{t: t}
	r.s = NewServer(target)
	r.s.RetryTransport = true
	r.s.Breaker = &Breaker{clock: func() float64 { return r.now }}
	r.s.Sleep = func(d time.Duration) { r.delays = append(r.delays, d) }
	srv := httptest.NewServer(r.s)
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *retrying) post() (int, string) {
	r.t.Helper()
	resp := post(r.t, r.url, "/v1/messages", retryBody)
	defer resp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func TestTransportRetry(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		fails                         int
		sse, retry                    bool
		status, wantCalls, wantSleeps int
	}{
		{"blocked provider is retried twice", 1 << 20, false, true, 503, 3, 2},
		{"provider that recovers answers after one retry", 1, false, true, 200, 2, 1},
		// No byte has gone downstream, so the status alone decides, as for JSON.
		{"an SSE error status is retried too", 1 << 20, true, true, 503, 3, 2},
		{"without the flag nothing is retried", 1 << 20, false, false, 503, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &blocked{fails: tc.fails, sse: tc.sse}
			srv := httptest.NewServer(up)
			defer srv.Close()
			r := newRetrying(t, srv.URL)
			r.s.RetryTransport = tc.retry
			if status, _ := r.post(); status != tc.status {
				t.Fatalf("status %d", status)
			}
			if up.calls() != tc.wantCalls || len(r.delays) != tc.wantSleeps {
				t.Fatalf("calls %d sleeps %d", up.calls(), len(r.delays))
			}
			for _, b := range up.bodies { // same model, same bytes, same target
				if b != retryBody {
					t.Fatalf("a retry changed the request: %q", b)
				}
			}
		})
	}
}

func TestRefusedConnectionIsRetriedThen502(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	r := newRetrying(t, dead.URL)
	if status, _ := r.post(); status != 502 || len(r.delays) != 2 {
		t.Fatal(status, r.delays)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "connect timed out" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// A stall in the connect phase already waited the stall timeout; retrying it
// would stack that wait three times and push the failures out of the breaker's window.
func TestConnectStallIsNotRetried(t *testing.T) {
	r := newRetrying(t, "http://127.0.0.1:1")
	r.s.loopback.Transport.(*http.Transport).DialContext = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: timeoutErr{}}
	}
	if status, _ := r.post(); status != 502 || len(r.delays) != 0 {
		t.Fatal(status, r.delays)
	}
}

func TestRetryDelayIsJitteredAndBounded(t *testing.T) {
	s := NewServer("")
	distinct := map[time.Duration]bool{}
	for i := 0; i < 20; i++ {
		d := s.retryDelay(2)
		if d < 0 || d > 2*s.RetryBase {
			t.Fatal(d)
		}
		distinct[d] = true
	}
	if len(distinct) < 2 {
		t.Fatal("a fixed backoff is not jitter")
	}
}

// openBreaker is three requests that each still fail after their retries, a
// minute apart: the breaker's threshold inside its 5-minute window.
func (r *retrying) openBreaker(up *blocked) {
	up.fails = 1 << 20
	for i := 0; i < breakerFailures; i++ {
		if status, _ := r.post(); status != 503 {
			r.t.Fatal(status)
		}
		r.now += 60
	}
	if up.calls() != 9 {
		r.t.Fatal("each request costs three upstream calls:", up.calls())
	}
}

func TestBreakerHoldsThenOneProbeCloses(t *testing.T) {
	up := &blocked{}
	srv := httptest.NewServer(up)
	defer srv.Close()
	r := newRetrying(t, srv.URL)
	r.openBreaker(up)
	// Held: the client gets the breaker's own 503 and nothing goes upstream.
	r.now += 60
	if status, _ := r.post(); status != 503 || up.calls() != 9 {
		t.Fatal(status, up.calls())
	}
	r.now = 60*2 + breakerOpen - 1 // a second short of the hold
	if status, _ := r.post(); status != 503 || up.calls() != 9 {
		t.Fatal(status, up.calls())
	}
	// The next real request past it probes the provider: it succeeds, so the
	// breaker closes and the following request is normal.
	r.now += 2
	up.fails = 0
	for want := 10; want <= 11; want++ {
		if status, _ := r.post(); status != 200 || up.calls() != want {
			t.Fatal(status, up.calls())
		}
	}
}

func TestFailedProbeReopensTheBreaker(t *testing.T) {
	up := &blocked{}
	srv := httptest.NewServer(up)
	defer srv.Close()
	r := newRetrying(t, srv.URL)
	r.openBreaker(up)
	r.now += breakerOpen + 1
	// The probe reaches the still-blocked provider: one more request with its
	// retries, and the breaker is open again.
	for range 2 {
		if status, _ := r.post(); status != 503 || up.calls() != 12 {
			t.Fatal(status, up.calls())
		}
	}
}

func TestFailuresSpreadOverMoreThanFiveMinutesDoNotOpenTheBreaker(t *testing.T) {
	up := &blocked{fails: 1 << 20}
	srv := httptest.NewServer(up)
	defer srv.Close()
	r := newRetrying(t, srv.URL)
	for range 3 {
		r.post()
		r.now += 200
	}
	// Three failures, but never three inside the window: the next request still
	// goes upstream (three more calls) rather than being held.
	if status, _ := r.post(); status != 503 || up.calls() != 12 {
		t.Fatal(status, up.calls())
	}
}

func TestOpenBreakerFailsReadablyAndNeverGoesElsewhere(t *testing.T) {
	up := &blocked{}
	srv := httptest.NewServer(up)
	defer srv.Close()
	r := newRetrying(t, srv.URL)
	r.openBreaker(up)
	status, body := r.post()
	if status != 503 || !strings.Contains(body, "ccmorph") || !strings.Contains(body, "then tries one") {
		t.Fatal(status, body)
	}
	for _, b := range up.bodies {
		if b != retryBody {
			t.Fatal("an open breaker is never a reason to change the request")
		}
	}
}
