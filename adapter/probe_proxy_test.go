package adapter

import (
	"net/http"
	"net/url"
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
