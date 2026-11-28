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
// `-h` prints the banner the gate reads, and records that it was asked.
const fakeBridge = `#!/usr/bin/env python3
import sys, os, re, socket, http.server
# HTTPServer.server_bind resolves 127.0.0.1 with socket.getfqdn, a reverse DNS
# lookup that stalls past the bridge's 30s readiness budget on a macOS runner
# with no DNS; this fake never uses the name. The pytest fakes carry the same
# line (tests/support.py FAKE_SERVER_NO_DNS), and a subprocess cannot see
# conftest.py's patch.
socket.getfqdn = lambda name='': name
if sys.argv[1] == '-h':
    asked = os.environ.get('FAKE_BRIDGE_ASKED')
    if asked:
        open(asked, 'a').write('h\n')
    print(os.environ.get('FAKE_BRIDGE_BANNER', ''))
    sys.exit(0)
conf = open(sys.argv[2]).read()
port = int(re.search(r'port: (\d+)', conf).group(1))
key = re.search(r'api-keys:\n  - "([^"]+)"', conf).group(1)
seen = [0]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        ok = self.headers.get('Authorization') == 'Bearer ' + key
        seen[0] += 1
        self.send_response(200 if ok else 401)
        if 'AGENT_DISPATCH_JOB' in os.environ:
            self.send_header('X-Job-Tagged', 'yes')
        self.end_headers()
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
	t.Setenv("AGENT_DISPATCH_JOB", "starter-job")
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
	pid, _ := strconv.Atoi(got["PID"])
	pgid, groupErr := syscall.Getpgid(pid)
	bridgeStop(pid, got["TMPDIR"], auth)
	if resp.Header.Get("X-Job-Tagged") != "" {
		t.Error("a shared bridge must not carry the starter's dispatch cleanup tag")
	}
	if groupErr != nil || pgid == syscall.Getpgrp() {
		t.Errorf("shared bridge must survive signals to its starter's process group: bridge group=%d, starter group=%d, err=%v", pgid, syscall.Getpgrp(), groupErr)
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

// gateBridge is the version gate's stub cliproxyapi: `-h` prints
// FAKE_BRIDGE_BANNER and records the read in FAKE_BRIDGE_ASKED; started as
// `-config <file>` it copies the config it was handed to FAKE_BRIDGE_CONF and
// exits, so the test reads exactly what bridgeStart rendered. No python, no
// server, no readiness wait: the python stub never served inside bridgeStart's
// 30s on the macOS runner (PR #681).
const gateBridge = `#!/bin/sh
if [ "$1" = "-h" ]; then
  if [ -n "$FAKE_BRIDGE_ASKED" ]; then echo h >> "$FAKE_BRIDGE_ASKED"; fi
  printf '%s' "$FAKE_BRIDGE_BANNER"
  exit 0
fi
cp "$2" "$FAKE_BRIDGE_CONF"
exit 1
`

// TestBridgeRenderVersionGate: antigravity.sensitive-patterns-replace first
// exists in cliproxyapi 8.0.70, and an older bridge drops the unknown section
// without a word, so every Antigravity turn answers 429 with nothing naming the
// cause. The gate reads the same binary's -h banner once, before rendering:
// below the floor it writes no section and names the repair, scoping the 429 to
// Antigravity requests because every provider's bridge prints it; at/above the
// floor and on an unparseable banner it writes the section.
func TestBridgeRenderVersionGate(t *testing.T) {
	requireLoopback(t) // bridgeStart picks its port by binding one
	// The gate reads the stub's -h before rendering, and an empty read counts
	// as a current bridge (BannerBelow): give the spawn the same headroom the
	// adapter's bound gets, so a loaded host cannot flip the below-floor case.
	t.Setenv("CCMORPH_BRIDGE_BANNER_TIMEOUT", "600")
	for _, tc := range []struct {
		name       string
		banner     string
		wantRules  bool
		wantNotice bool
	}{
		{"below the floor", "CLIProxyAPI Version: 8.0.45, Commit: Homebrew\n", false, true},
		{"at the floor", "CLIProxyAPI Version: 8.0.70, Commit: x\n", true, false},
		{"above the floor", "CLIProxyAPI Version: v8.1.0, Commit: x\n", true, false},
		{"unparseable banner counts as new", "CLIProxyAPI Version: dev, Commit: x\n", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)
			t.Setenv("FAKE_BRIDGE_BANNER", tc.banner)
			asked := filepath.Join(dir, "asked")
			t.Setenv("FAKE_BRIDGE_ASKED", asked)
			seen := filepath.Join(dir, "seen-config.yaml")
			t.Setenv("FAKE_BRIDGE_CONF", seen)
			bin := filepath.Join(dir, "cliproxyapi")
			if err := os.WriteFile(bin, []byte(gateBridge), 0o755); err != nil {
				t.Fatal(err)
			}
			auth := filepath.Join(dir, "auth")
			os.MkdirAll(auth, 0o700)
			var out, errw bytes.Buffer
			// The stub exits once it holds its config, so bridgeStart reports a death.
			rc := bridgeStart([]string{"p", bin, auth, filepath.Join(dir, "log")}, nil, &out, &errw)
			if rc != 1 || !strings.Contains(errw.String(), "exited before becoming ready") {
				t.Fatalf("rc=%d err=%q, want the stub's exit reported", rc, errw.String())
			}
			rendered, err := os.ReadFile(seen)
			if err != nil {
				t.Fatalf("the stub was never started with -config: %v", err)
			}
			if has := strings.Contains(string(rendered), "sensitive-patterns-replace"); has != tc.wantRules {
				t.Fatalf("sensitive-patterns-replace present=%v, want %v; config:\n%s", has, tc.wantRules, rendered)
			}
			notice := errw.String()
			if got := strings.Contains(notice, "upgrade cliproxyapi"); got != tc.wantNotice {
				t.Fatalf("stderr %q, want the upgrade notice=%v", notice, tc.wantNotice)
			}
			if tc.wantNotice && !(strings.Contains(notice, "8.0.70") && strings.Contains(notice, "429")) {
				t.Fatalf("notice must name the floor and the 429: %q", notice)
			}
			// Every provider's bridge prints this notice, so its 429 must be
			// Antigravity's alone (INTENT #8: a truthful reason).
			if tc.wantNotice && (!strings.Contains(notice, "an Antigravity request") || strings.Contains(notice, "every turn")) {
				t.Fatalf("notice must scope the 429 to Antigravity requests: %q", notice)
			}
			// The version is read once, before rendering.
			reads, _ := os.ReadFile(asked)
			if n := strings.Count(string(reads), "h"); n != 1 {
				t.Fatalf("-h read %d times, want 1", n)
			}
		})
	}
}

// The rules must be cliproxyapi YAML whose patterns rewrite each blocked line
// inside a longer system block and leave the other lines alone. The patterns and
// replacements come back out of the emitted YAML, so a key or value typo in the
// emitter fails here instead of at every turn against a real bridge.
func TestAntigravityRules(t *testing.T) {
	rules := parseAntigravityRules(t, antigravityRules())
	// One emitted rule per table entry: an extra or dropped pair changes what
	// cliproxyapi applies even when the block below still comes out right.
	if len(rules) != len(antigravityRewrites) {
		t.Fatalf("emitted %d rules, the table has %d:\n%s", len(rules), len(antigravityRewrites), antigravityRules())
	}
	block := "x-anthropic-billing-header: v\n" +
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.\n" +
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.\n" +
		"keep me\n" +
		"IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges.\n" +
		"keep me too\n" +
		" - Text inside <pasted_content> tags was pasted into the message by the user from somewhere else.\n" +
		"keep me last"
	want := "x-anthropic-billing-header: v\n" +
		"You are an interactive coding agent.\n" +
		"You are an interactive coding agent.\n" +
		"keep me\n" +
		"Help with security work only in authorized, defensive or educational contexts.\n" +
		"keep me too\n" +
		" - Text in <pasted_content> tags is material the user pasted; act on it as the user's own message asks.\n" +
		"keep me last"
	// Upstream applies every rule in one non-cascading pass over the original
	// text; sequential rewrites agree only while no replacement re-matches.
	for _, r := range rules {
		for _, other := range rules {
			if other.pattern.MatchString(r.replacement) {
				t.Fatalf("replacement %q re-matches %q: a single pass would not apply it", r.replacement, other.pattern)
			}
		}
	}
	apply := func(s string) string {
		for _, r := range rules {
			s = r.pattern.ReplaceAllString(s, r.replacement)
		}
		return s
	}
	if got := apply(block); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	// Each refused line also sits at either edge of the block: first, with no
	// line before it, and last, with no newline after it (the system block can
	// end on the pasted-content line). A rule that needs a neighbouring newline
	// would rewrite the middle and miss the edges.
	for _, line := range []struct{ in, out string }{
		{"You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are an interactive coding agent."},
		{"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.", "You are an interactive coding agent."},
		{"IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges.", "Help with security work only in authorized, defensive or educational contexts."},
		{" - Text inside <pasted_content> tags was pasted into the message by the user from somewhere else.", " - Text in <pasted_content> tags is material the user pasted; act on it as the user's own message asks."},
	} {
		if got := apply("keep me\n" + line.in); got != "keep me\n"+line.out {
			t.Errorf("as the last, unterminated line: got %q, want %q", got, "keep me\n"+line.out)
		}
		if got := apply(line.in + "\nkeep me"); got != line.out+"\nkeep me" {
			t.Errorf("as the first line: got %q, want %q", got, line.out+"\nkeep me")
		}
	}
}

type parsedAntigravityRule struct {
	pattern     *regexp.Regexp
	replacement string
}

// parseAntigravityRules reads the emitted YAML back into the rules cliproxyapi
// would: exactly the pairs written to disk, never the Go table behind them.
func parseAntigravityRules(t *testing.T, emitted string) []parsedAntigravityRule {
	t.Helper()
	lines := strings.Split(emitted, "\n")
	if len(lines) < 2 || lines[0] != "antigravity:" || lines[1] != "  sensitive-patterns-replace:" {
		t.Fatalf("rules not under antigravity: sensitive-patterns-replace:\n%s", emitted)
	}
	var out []parsedAntigravityRule
	for i := 2; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			continue
		}
		quoted, ok := strings.CutPrefix(line, "    - pattern: ")
		if !ok {
			t.Fatalf("unexpected line %q in:\n%s", line, emitted)
		}
		pattern, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("pattern %q is not a quoted scalar: %v", quoted, err)
		}
		if i+1 >= len(lines) {
			t.Fatalf("pattern %q has no replacement", pattern)
		}
		quoted, ok = strings.CutPrefix(lines[i+1], "      replacement: ")
		if !ok {
			t.Fatalf("pattern %q has no replacement: %q", pattern, lines[i+1])
		}
		replacement, err := strconv.Unquote(quoted)
		if err != nil {
			t.Fatalf("replacement %q is not a quoted scalar: %v", quoted, err)
		}
		out = append(out, parsedAntigravityRule{pattern: regexp.MustCompile(pattern), replacement: replacement})
		i++
	}
	if len(out) == 0 {
		t.Fatalf("no rules emitted:\n%s", emitted)
	}
	return out
}
