package netroute

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestNewClientTrustsTheSessionsExtraCA pins the builder's reason for existing:
// inside a Remote Control session the route is a proxy signing every host with
// the CA NODE_EXTRA_CA_CERTS names, and Go's x509 never reads that variable by
// itself. A client built while the variable names the CA trusts the server the
// system pool rejects (#829).
func TestNewClientTrustsTheSessionsExtraCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	// The server is loopback: take the ambient proxy out of this test's route,
	// so it asks only what it is about (the CA), not what shell ran it.
	untrusted := NewClient(5 * time.Second)
	untrusted.Transport.(*http.Transport).Proxy = nil
	if resp, err := untrusted.Get(srv.URL); err == nil {
		resp.Body.Close()
		t.Fatal("an untrusted certificate answered")
	} else if !strings.Contains(err.Error(), "certificate signed by unknown authority") {
		t.Fatalf("the refusal was not x509: %v", err)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", ca)
	trusted := NewClient(5 * time.Second)
	trusted.Transport.(*http.Transport).Proxy = nil
	resp, err := trusted.Get(srv.URL)
	if err != nil {
		t.Fatalf("the session's CA was not trusted: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestNewClientKeepsTheProxysRouteAndTimeout: the builder replaces the plain
// client each fetcher used to build, so the route the edge exports and the
// whole-exchange cap must not change (#829).
func TestNewClientKeepsTheProxysRouteAndTimeout(t *testing.T) {
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	c := NewClient(7 * time.Second)
	if c.Timeout != 7*time.Second {
		t.Fatalf("timeout %v, want 7s", c.Timeout)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	if tr.Proxy == nil || reflect.ValueOf(tr.Proxy).Pointer() != reflect.ValueOf(http.ProxyFromEnvironment).Pointer() {
		t.Fatal("the transport must keep ProxyFromEnvironment: HTTPS_PROXY, NO_PROXY and the loopback exemption decide the route")
	}
	if tr.TLSClientConfig != nil {
		t.Fatal("with no NODE_EXTRA_CA_CERTS the TLS config must stay nil and Go's own default stand")
	}
}
