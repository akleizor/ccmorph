package ctxwindow

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// edgeOpenerBody is what the edge's catalogue answer carries.
const edgeOpenerBody = `{"deepseek":{"api":"https://api.deepseek.com"}}`

// TestOpenerTrustsTheSessionsExtraCA is #829's regression: the windows' opener
// is the default transport behind FetchCatalogue and every context probe, and
// inside a Remote Control edge the route is a proxy signing every host with the
// session CA. Go's x509 never reads NODE_EXTRA_CA_CERTS by itself. The fetch
// runs in a fresh test binary because net/http reads its proxy environment once
// per process (the seam netroute's TestStdlibProxyChild uses).
func TestOpenerTrustsTheSessionsExtraCA(t *testing.T) {
	if target := os.Getenv("CCMORPH_TEST_OPENER_EDGE_URL"); target != "" {
		body, err := openURL(context.Background(), target, nil, 5*time.Second)
		if err != nil {
			t.Fatalf("catalogue fetch through the edge proxy: %v", err)
		}
		if string(body) != edgeOpenerBody {
			t.Fatalf("opener returned %q; want %q", body, edgeOpenerBody)
		}
		return
	}
	cert, ca := edgeCertificate(t)
	proxy := edgeProxyURL(t, cert)
	target := "https://example.com/api.json"
	// The tunnel is used: without the CA the same route stops at x509.
	out, err := runOpenerEdgeChild(t, proxy, target, "")
	if err == nil || !strings.Contains(string(out), "certificate signed by unknown authority") {
		t.Fatalf("without NODE_EXTRA_CA_CERTS the opener was not refused by x509 (err %v): %s", err, out)
	}
	if out, err = runOpenerEdgeChild(t, proxy, target, ca); err != nil {
		t.Fatalf("the session's CA was not trusted through the edge proxy (err %v): %s", err, out)
	}
}

// edgeCertificate is the session's CA and a certificate signed with it, in the
// shape mitmproxy presents: the local test server's own certificate, valid for
// example.com and this machine's loopback names.
func edgeCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "session-ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.TLS.Certificates[0], ca
}

// edgeProxyURL is the edge's route for one test: a CONNECT proxy that answers
// the tunnel with TLS signed by cert, as mitmproxy signs every host. The target
// name is never resolved -- the proxy is the host -- so reaching it needs the
// CA and no network.
func edgeProxyURL(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tc.Handshake(); err != nil {
					return
				}
				defer tc.Close()
				if _, err := http.ReadRequest(bufio.NewReader(tc)); err != nil {
					return
				}
				fmt.Fprintf(tc, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(edgeOpenerBody), edgeOpenerBody)
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

// runOpenerEdgeChild asks a fresh test binary for the opener's answer, through
// proxy, trusting ca when it names one. net/http caches the proxy environment
// once per process, so only a fresh binary can be asked twice.
func runOpenerEdgeChild(t *testing.T, proxy, target, ca string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenerTrustsTheSessionsExtraCA$")
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
			"REQUEST_METHOD", "NODE_EXTRA_CA_CERTS", "CCMORPH_TEST_OPENER_EDGE_URL":
			continue // the test decides the route and the CA
		}
		env = append(env, kv)
	}
	env = append(env, "HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY=", "no_proxy=",
		"CCMORPH_TEST_OPENER_EDGE_URL="+target)
	if ca != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+ca)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}
