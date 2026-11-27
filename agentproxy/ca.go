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
	"crypto"
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
	"strings"
	"time"
)

// CA is ccmorph's local certificate authority for the proxy.
type CA struct {
	cert *x509.Certificate
	key  crypto.Signer
	PEM  []byte // the CA certificate, for the session's trust bundle
}

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca.key"
)

// LoadCA reads the CA kept in dir, making it on first use. The key file is
// 0600 in a 0700 directory. The pair is made in a directory of its own and
// moved into place by one rename, so launchers starting together never keep
// a key from one and a certificate from another: the first rename wins and
// the others read what it left.
func LoadCA(dir string) (*CA, error) { return loadCA(dir, makeCA) }

// LoadEdgeCA is LoadCA for the Remote Control edge, whose first CA is the one
// its proxy already had: legacy is that proxy's combined PEM (key and
// certificate). Importing it keeps every session that trusts that certificate
// trusting the edge across the change of proxy; without one the edge makes its own.
func LoadEdgeCA(dir, legacy string) (*CA, error) {
	return loadCA(dir, func(tmp string) error {
		if legacy != "" {
			if err := importCA(tmp, legacy); err == nil {
				return nil
			}
		}
		return makeCA(tmp)
	})
}

func loadCA(dir string, seed func(tmp string) error) (*CA, error) {
	if ca, err := readCA(dir); err == nil {
		return ca, nil
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(parent, ".agent-proxy-ca.*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := seed(tmp); err != nil {
		return nil, err
	}
	if os.Rename(tmp, dir) != nil {
		// Something is there: another launcher's pair, or one this one could
		// not use (expiring, mismatched). Keep a good one; move a bad one aside.
		if ca, err := readCA(dir); err == nil {
			return ca, nil
		}
		old, err := os.MkdirTemp(parent, ".agent-proxy-ca.old.*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(old)
		if err := os.Rename(dir, filepath.Join(old, "ca")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if err := os.Rename(tmp, dir); err != nil {
			return readCA(dir) // another launcher replaced it first
		}
	}
	return readCA(dir)
}

func makeCA(dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
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
		return err
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, caKeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder})); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, caCertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
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
	return parseCA(certPEM, keyPEM)
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("agent proxy CA: unreadable PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	if pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool }); !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("agent proxy CA: key does not match certificate")
	}
	if time.Until(cert.NotAfter) < 30*24*time.Hour {
		return nil, errors.New("agent proxy CA: expiring")
	}
	return &CA{cert: cert, key: key, PEM: certPEM}, nil
}

// parseKey is a private key in any of the forms a CA is kept in.
func parseKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if s, ok := k.(crypto.Signer); ok {
			return s, nil
		}
	}
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	return x509.ParseECPrivateKey(der)
}

// importCA writes the CA kept in legacy (one PEM holding a private key and its
// certificate) into dir as this package keeps one.
func importCA(dir, legacy string) error {
	b, err := os.ReadFile(legacy)
	if err != nil {
		return err
	}
	var certPEM, keyPEM []byte
	for rest := b; ; {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		switch {
		case blk.Type == "CERTIFICATE" && certPEM == nil:
			certPEM = pem.EncodeToMemory(blk)
		case strings.HasSuffix(blk.Type, "PRIVATE KEY") && keyPEM == nil:
			keyPEM = pem.EncodeToMemory(blk)
		}
	}
	if _, err := parseCA(certPEM, keyPEM); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, caKeyFile), keyPEM); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, caCertFile), certPEM)
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
