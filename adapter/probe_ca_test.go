package adapter

import (
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// A classification probe run inside a Remote Control session reaches its
// upstream through a local proxy signed with the session's extra CA
// (NODE_EXTRA_CA_CERTS). The probe must trust it, as quota's reads and
// `ccmorph check` do: without it the TLS handshake fails before any request
// leaves, so a launch that must classify a new model cannot start (#822).
func TestProbeTrustsTheSessionsExtraCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	if status, note := postImpl(srv.URL+"/v1/messages", map[string]any{"model": "m"}, anthropicHeaders("k")); status != 0 {
		t.Fatalf("an untrusted certificate answered: %d %s", status, note)
	}
	ca := filepath.Join(t.TempDir(), "ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_EXTRA_CA_CERTS", ca)
	if status, note := postImpl(srv.URL+"/v1/messages", map[string]any{"model": "m"}, anthropicHeaders("k")); status != 200 {
		t.Fatalf("the session's CA was not trusted: %d %s", status, note)
	}
}
