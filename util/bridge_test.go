package util

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A fake cliproxyapi: python serving /v1/models on the config's port, checking the key.
const fakeBridge = `#!/usr/bin/env python3
import sys, re, http.server
conf = open(sys.argv[2]).read()
port = int(re.search(r'port: (\d+)', conf).group(1))
key = re.search(r'api-keys:\n  - "([^"]+)"', conf).group(1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = self.headers.get('Authorization') == 'Bearer ' + key
        self.send_response(200 if ok else 401); self.end_headers(); self.wfile.write(b'{"data":[]}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
`

func TestBridgeStartsServesAndStops(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 needed for the fake bridge")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	bin := filepath.Join(dir, "cliproxyapi")
	os.WriteFile(bin, []byte(fakeBridge), 0o755)
	auth := filepath.Join(dir, "auth")
	os.MkdirAll(auth, 0o700)
	cred := filepath.Join(auth, "c.json")
	os.WriteFile(cred, []byte("{}"), 0o644)

	var out, errw bytes.Buffer
	if rc := bridgeStart([]string{"p", bin, auth, filepath.Join(dir, "log")}, nil, &out, &errw); rc != 0 {
		t.Fatalf("rc=%d err=%s", rc, errw.String())
	}
	got := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		k, v, _ := strings.Cut(l, "=")
		got[k] = v
	}
	if len(got["KEY"]) != 32 || got["PORT"] == "" || got["PID"] == "" {
		t.Fatalf("bad handoff %q", out.String())
	}
	if fi, _ := os.Stat(filepath.Join(got["TMPDIR"], "config.yaml")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("rendered config must be 0600")
	}
	bridgeStop([]string{got["PID"], got["TMPDIR"], auth}, nil, &out, &errw)
	var pid int
	for _, c := range got["PID"] {
		pid = pid*10 + int(c-'0')
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("bridge must be gone after stop")
	}
	if _, err := os.Stat(got["TMPDIR"]); err == nil {
		t.Fatal("rendered config dir must be removed")
	}
	if fi, _ := os.Stat(cred); fi.Mode().Perm() != 0o600 {
		t.Fatal("credentials must be re-tightened to 0600")
	}
}

func TestBridgeThatDiesIsReported(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	bin := filepath.Join(dir, "cliproxyapi")
	os.WriteFile(bin, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	var out, errw bytes.Buffer
	if rc := bridgeStart([]string{"p", bin, dir, os.DevNull}, nil, &out, &errw); rc != 1 ||
		!strings.Contains(errw.String(), "exited before becoming ready") {
		t.Fatalf("rc=%d err=%q", rc, errw.String())
	}
}
