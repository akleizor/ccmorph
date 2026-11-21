package adapter

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

func TestProbesUseTheProfileUpstreamProxy(t *testing.T) {
	swap(t, &probeVia, &url.URL{Scheme: "http", Host: "127.0.0.1:18480"})
	req, _ := http.NewRequest("GET", "https://gateway.lan:18443/v1/models", nil)
	if u, _ := client.Transport.(*http.Transport).Proxy(req); u == nil || u.Host != "127.0.0.1:18480" {
		t.Fatalf("probe ignored the profile proxy: got %v", u)
	}
}

// A test-home gateway is loopback, where the profile's proxy cannot reach;
// sending the probe there refused every test-home launch.
func TestProbesNeverProxyLoopback(t *testing.T) {
	swap(t, &probeVia, &url.URL{Scheme: "http", Host: "127.0.0.1:18480"})
	for _, target := range []string{
		"http://127.0.0.1:61183/v1/models",
		"http://localhost:61183/v1/models",
		"http://[::1]:61183/v1/models",
	} {
		req, _ := http.NewRequest("GET", target, nil)
		if u, _ := client.Transport.(*http.Transport).Proxy(req); u != nil {
			t.Fatalf("%s: probe used the proxy %v", target, u)
		}
	}
}

// recordDials captures every address a transport tries to reach, then fails
// the dial: the route a call takes is the address asked for, and no test here
// needs a listening port.
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

// onlyDial is the one address a call dialed, or a failure naming what it dialed.
func onlyDial(t *testing.T, dials func() []string, want string) {
	t.Helper()
	got := dials()
	if len(got) != 1 || got[0] != want {
		t.Fatalf("the probe dialed %v; want %s", got, want)
	}
}

// noAmbientProxy hides every proxy the process might carry, so the profile's
// knob is the only route a probe could take.
func noAmbientProxy(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, "")
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
}

// A gateway this process cannot dial is reachable only through the profile's
// proxy: the probe must dial the proxy, never the unresolvable host (GH-580).
func TestProbesDialTheGatewayThroughTheProfileProxy(t *testing.T) {
	noAmbientProxy(t)
	dials := recordDials(t, client.Transport.(*http.Transport))
	swap(t, &probeVia, &url.URL{Scheme: "http", Host: "bridge.invalid:3128"})

	inventory("http://gateway.lan:8443", "k", nil)
	onlyDial(t, dials, "bridge.invalid:3128")
}

// afb477d's opposite side: the knob is not a licence to send this machine's
// own 127.0.0.1 through a remote proxy. A loopback base is dialed directly.
func TestProbesNeverSendALoopbackBaseThroughTheProfileProxy(t *testing.T) {
	noAmbientProxy(t)
	dials := recordDials(t, client.Transport.(*http.Transport))
	swap(t, &probeVia, &url.URL{Scheme: "http", Host: "bridge.invalid:3128"})

	inventory("http://127.0.0.1:4000", "k", nil)
	onlyDial(t, dials, "127.0.0.1:4000")
}

// NO_PROXY keeps its meaning under the profile's knob, as it had under the
// environment's own transport.
func TestProbesHonourNoProxyUnderTheProfileProxy(t *testing.T) {
	noAmbientProxy(t)
	t.Setenv("NO_PROXY", "gateway.lan")
	dials := recordDials(t, client.Transport.(*http.Transport))
	swap(t, &probeVia, &url.URL{Scheme: "http", Host: "bridge.invalid:3128"})

	inventory("https://gateway.lan:18443", "k", nil)
	onlyDial(t, dials, "gateway.lan:18443")
}
