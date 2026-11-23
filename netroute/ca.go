package netroute

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"time"
)

// ExtraCAs is the TLS config that also trusts the extra CA a Claude Code
// session trusts (NODE_EXTRA_CA_CERTS): inside a Remote Control session the
// route to Anthropic is a local proxy signing with that CA. nil when the
// variable names no readable certificate, which keeps Go's own default.
func ExtraCAs() *tls.Config {
	path := os.Getenv("NODE_EXTRA_CA_CERTS")
	if path == "" {
		return nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil
	}
	return &tls.Config{RootCAs: pool}
}

// NewTransport is the transport for a call that may cross the session's route:
// http.DefaultTransport's settings, so HTTPS_PROXY, NO_PROXY and the loopback
// exemption keep their meaning, plus the extra CA the session trusts. It reads
// NODE_EXTRA_CA_CERTS at call time, so a client built inside a Remote Control
// session trusts the proxy that signs every host; outside one the TLS config
// stays nil and Go's own default stands (#829).
func NewTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = ExtraCAs()
	return tr
}

// NewClient is NewTransport with timeout capping the whole exchange: the plain
// fetcher client it replaces, now trusting the session's CA.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: NewTransport(), Timeout: timeout}
}
