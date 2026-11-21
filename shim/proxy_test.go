package shim

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// noSocketListener is the listener a test's shim is built with: nothing here
// serves it, so no port is needed.
type noSocketListener struct{}

func (noSocketListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (noSocketListener) Close() error              { return nil }
func (noSocketListener) Addr() net.Addr            { return &net.TCPAddr{} }

// configured builds a shim from getenv without binding its port.
func configured(t *testing.T, target string, getenv func(string) string) *Server {
	t.Helper()
	old := listenFn
	listenFn = func(_, _ string) (net.Listener, error) { return noSocketListener{}, nil }
	t.Cleanup(func() { listenFn = old })
	srv, _, err := Listen(Hooks{}, Options{Target: target}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := srv.Handler.(*Server)
	if !ok {
		t.Fatalf("Listen's handler is %T, not a *Server", srv.Handler)
	}
	return s
}

// recordDials captures every address a transport tries to reach, then fails
// the dial: the route a call takes is the address asked for.
func recordDials(t *testing.T, tr *http.Transport) func() []string {
	t.Helper()
	var mu sync.Mutex
	var dials []string
	old := tr.DialContext
	tr.DialContext = func(_ context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		dials = append(dials, addr)
		mu.Unlock()
		return nil, errors.New("test dial")
	}
	t.Cleanup(func() { tr.DialContext = old })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), dials...)
	}
}

func noAmbientProxy(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
}

// The profile's CCMORPH_UPSTREAM_PROXY reaches the shim's environment; the
// session's own calls must dial through it, or a native gateway this process
// cannot reach starts and then fails every request (GH-580).
func TestShimDialsTheUpstreamThroughTheProfileProxy(t *testing.T) {
	noAmbientProxy(t)
	s := configured(t, "http://gateway.lan:8443", func(k string) string {
		if k == "CCMORPH_UPSTREAM_PROXY" {
			return "http://bridge.invalid:3128"
		}
		return ""
	})
	dials := recordDials(t, s.remote.Transport.(*http.Transport))

	rec := httptest.NewRecorder()
	// Origin-form, as a client sends to the shim: an absolute target would make
	// RequestURI absolute too, which the shim does not accept from a client.
	s.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/complete", strings.NewReader("{}")))
	if got := dials(); len(got) != 1 || got[0] != "bridge.invalid:3128" {
		t.Fatalf("the session's request dialed %v; want the profile proxy (upstream %v, answer %d: %s)",
			got, s.Upstream, rec.Code, rec.Body.String())
	}
}

// NO_PROXY keeps its meaning under the knob: a listed gateway is dialed
// directly, as the environment's own transport would have done.
func TestShimHonoursNoProxyUnderTheProfileProxy(t *testing.T) {
	noAmbientProxy(t)
	t.Setenv("NO_PROXY", "gateway.lan")
	s := configured(t, "http://gateway.lan:8443", func(k string) string {
		if k == "CCMORPH_UPSTREAM_PROXY" {
			return "http://bridge.invalid:3128"
		}
		return ""
	})
	dials := recordDials(t, s.remote.Transport.(*http.Transport))

	s.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest("POST", "/v1/complete", strings.NewReader("{}")))
	if got := dials(); len(got) != 1 || got[0] != "gateway.lan:8443" {
		t.Fatalf("a NO_PROXY-listed gateway dialed %v; want it directly", got)
	}
}

// The knob is parsed once when the shim starts, so a malformed value leaves
// the environment's own resolution standing instead of failing the session.
func TestShimTakesOnlyAParsableUpstreamProxy(t *testing.T) {
	s := configured(t, "http://gateway.lan:8443", func(k string) string {
		if k == "CCMORPH_UPSTREAM_PROXY" {
			return "gateway.lan:3128" // no scheme: not a proxy the translator could use either
		}
		return ""
	})
	if s.Upstream != nil {
		t.Fatalf("an unparsable knob became the upstream route: %v", s.Upstream)
	}
}
