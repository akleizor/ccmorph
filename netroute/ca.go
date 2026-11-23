package netroute

import (
	"crypto/tls"
	"crypto/x509"
	"os"
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
