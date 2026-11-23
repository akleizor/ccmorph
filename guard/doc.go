// Package guard holds module-wide checks no single package can make.
//
// clientlint_test.go refuses a production *http.Client built outside netroute's
// CA-aware constructor or a provable direct route: inside a Remote Control
// session the edge's proxy signs every host with the CA in NODE_EXTRA_CA_CERTS,
// and Go's x509 never reads that variable by itself, so a client that honours
// the proxy without the CA fails its handshake before a request leaves (#822,
// #829).
package guard
