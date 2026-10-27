package shim

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// rawRequest writes req verbatim and returns what the server answers.
func rawRequest(t *testing.T, addr, req string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte(req))
	b, _ := io.ReadAll(c)
	return string(b)
}
