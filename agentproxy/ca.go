// Package agentproxy is the local proxy a ccmorph session sends its traffic
// through, so a subagent the router picked on another profile still runs as
// Claude Code's own subagent: that one agent's inference requests go to the
// picked profile, and everything else passes through untouched.
//
// The session keeps api.anthropic.com as its base URL, so Claude Code keeps
// every first-party behaviour. Only api.anthropic.com is decrypted, with a
// certificate from ccmorph's own local CA, which only the launched Claude Code
// trusts (NODE_EXTRA_CA_CERTS): no keychain entry, no system trust change.
// Every other host is a plain CONNECT tunnel the proxy never reads.
package agentproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"github.com/akleizor/ccmorph/util/paths"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// CA is ccmorph's local certificate authority for the proxy.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	PEM  []byte // the CA certificate, for the session's trust bundle
}

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca.key"
)

// LoadCA reads the CA kept in dir, making it on first use. The key file is
// 0600 in a 0700 directory.
func LoadCA(dir string) (*CA, error) {
	if ca, err := readCA(dir); err == nil {
		return ca, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "ccmorph local agent proxy CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})); err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(dir, caCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	return readCA(dir)
}

func readCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("agent proxy CA: unreadable PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return nil, errors.New("agent proxy CA: expiring")
	}
	return &CA{cert: cert, key: key, PEM: certPEM}, nil
}

// Leaf is a server certificate for host, signed by the CA, held in memory only.
func (ca *CA) Leaf(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 30),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

func writeFile(path string, b []byte) error {
	return paths.AtomicWrite(paths.WriteIn{Path: path, Temp: ".ca.*.tmp", Data: b, Mode: 0o600, Sync: true})
}
