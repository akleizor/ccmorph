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
