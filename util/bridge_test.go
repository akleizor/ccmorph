package util

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// requireLoopback skips a test whose subject must bind a loopback port, but only
// on the sandbox's permission refusal (issue #611): EPERM or EACCES at bind is a
// stable environment fact and gets the one loud skip. Every other listen error
// stays a visible failure — a transient resource problem must not silently
// disable the bridge tests on an otherwise-capable runner. CI carries no sandbox
// and runs every caller.
func requireLoopback(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == syscall.EPERM || errno == syscall.EACCES) {
			t.Skip("SKIP: loopback bind denied in this sandbox (runs in CI)")
		}
		t.Fatalf("loopback probe failed: %v", err)
	}
	l.Close()
}

// A fake cliproxyapi: python serving /v1/models on the config's port, checking the
// key. Like a fresh antigravity login, its first two answers are 200 with no models.
const fakeBridge = `#!/usr/bin/env python3
import sys, re, http.server
conf = open(sys.argv[2]).read()
port = int(re.search(r'port: (\d+)', conf).group(1))
key = re.search(r'api-keys:\n  - "([^"]+)"', conf).group(1)
seen = [0]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = self.headers.get('Authorization') == 'Bearer ' + key
        seen[0] += 1
        self.send_response(200 if ok else 401); self.end_headers()
        self.wfile.write(b'{"data":[{"id":"m"}]}' if seen[0] > 2 else b'{"data":[]}')
    def log_message(self, *a): pass
http.server.HTTPServer(('127.0.0.1', port), H).serve_forever()
`

func TestBridgeStartsServesAndStops(t *testing.T) {
	requireLoopback(t) // the fake bridge answers on a bound port
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
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+got["PORT"]+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+got["KEY"])
	resp, err := (&http.Client{Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"id":"m"`) {
		t.Fatalf("ready before the bridge named a model: %s", body)
	}
	if fi, _ := os.Stat(filepath.Join(got["TMPDIR"], "config.yaml")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("rendered config must be 0600")
	}
	pidN, _ := strconv.Atoi(got["PID"])
	BridgeStop(pidN, got["TMPDIR"], auth)
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
	requireLoopback(t) // bridgeStart picks its port by binding one
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

// The rules must be cliproxyapi YAML whose patterns rewrite each blocked line
// inside a longer system block and leave the other lines alone.
func TestAntigravityRules(t *testing.T) {
	r := antigravityRules()
	if !strings.HasPrefix(r, "antigravity:\n  sensitive-patterns-replace:\n") || strings.Count(r, "- pattern:") != len(antigravityRewrites) {
		t.Fatalf("rules:\n%s", r)
	}
	block := "x-anthropic-billing-header: v\nYou are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.\n" +
		"keep me\nIMPORTANT: Assist with authorized security testing, defensive security, CTF challenges.\nkeep me too\n" +
		" - Text inside <pasted_content> tags was pasted into the message by the user from somewhere else."
	for _, rw := range antigravityRewrites {
		block = regexp.MustCompile(rw[0]).ReplaceAllString(block, rw[1])
	}
	want := "x-anthropic-billing-header: v\nYou are an interactive coding agent.\nkeep me\n" +
		"Help with security work only in authorized, defensive or educational contexts.\nkeep me too\n" +
		" - Text in <pasted_content> tags is material the user pasted; act on it as the user's own message asks."
	if block != want {
		t.Fatalf("got %q", block)
	}
}
