package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitNULKeepsEmptyValues(t *testing.T) {
	if got := splitNUL("a\x00\x00b c\x00."); !slices.Equal(got, []string{"a", "", "b c"}) {
		t.Fatalf("got %q", got)
	}
	if got := splitNUL("."); len(got) != 0 {
		t.Fatalf("got %q", got)
	}
}

func TestEnvHelpersReadTheLastValue(t *testing.T) {
	env := []string{"A=1", "B=x=y", "A=2"}
	if v, ok := envGet(env, "A"); !ok || v != "2" {
		t.Fatalf("A=%q %v", v, ok)
	}
	if v, _ := envGet(env, "B"); v != "x=y" {
		t.Fatalf("B=%q", v)
	}
	if got := envDrop(env, "A"); !slices.Equal(got, []string{"B=x=y"}) {
		t.Fatalf("drop: %q", got)
	}
	if authKey([]string{"ANTHROPIC_AUTH_TOKEN=t"}) != "t" || authKey([]string{"ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=k"}) != "k" {
		t.Fatal("authKey must prefer the API key")
	}
}

func TestProjectionStripsEveryLaunchKey(t *testing.T) {
	got := projectionArgs([]string{"--acw", "none"}, []string{"A=1", "junk", "B="})
	if !slices.Equal(got, []string{"--acw", "none", "--strip", "A", "--strip", "B"}) {
		t.Fatalf("got %q", got)
	}
}

func TestNestedEdgeEnvOnlyBehindTheLoopbackEdge(t *testing.T) {
	l := &launcher{}
	t.Setenv("CCMORPH_EDGE_ROUTE_ENV", "X=1\nY=2\nZ=3")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTPS_PROXY", "http://example.com:8080")
	if got := l.nestedEdgeEnv("X"); got != nil {
		t.Fatalf("a remote proxy must add nothing, got %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47670/")
	if got := l.nestedEdgeEnv("X", "Z"); !slices.Equal(got, []string{"X=1", "Z=3"}) {
		t.Fatalf("got %q", got)
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:70000")
	if got := l.nestedEdgeEnv("X"); got != nil {
		t.Fatalf("an out-of-range port must add nothing, got %q", got)
	}
}

// GH-113: the state dir is a literal path, not pattern syntax. A bracket in it
// matched nothing and a * or ? swallowed sibling directories; ccm[6], ccm* and
// ccm? all match the sibling name ccm6.
func TestConfNamesToleratesGlobMetacharactersInTheStateDir(t *testing.T) {
	for _, meta := range []string{"[6]", "*", "?"} {
		root := t.TempDir()
		dir := filepath.Join(root, "ccm"+meta)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tok.conf"), []byte("type=token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		decoy := filepath.Join(root, "ccm6")
		if err := os.MkdirAll(decoy, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(decoy, "phantom.conf"), []byte("type=token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		l := &launcher{dir: dir}
		if got := l.confNames(); !slices.Equal(got, []string{"tok"}) {
			t.Fatalf("state dir ending in %q: got %q", meta, got)
		}
	}
}

// GH-113: the profile dirs path is as literal as the state dir — a resume by id
// must find a session under it whatever the path's own glob metacharacters.
func TestSessionFileToleratesGlobMetacharactersInTheProfileDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	dirs := filepath.Join(root, "ccm[6]")
	slug := filepath.Join(dirs, "demo", "projects", "one-project")
	if err := os.MkdirAll(slug, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "sess-123"
	if err := os.WriteFile(filepath.Join(slug, id+".jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &launcher{profileDirs: dirs}
	p, ok := l.sessionFile(id)
	if !ok || filepath.Base(p) != id+".jsonl" {
		t.Fatalf("got %q %v", p, ok)
	}
}

// `claude auth status` must not see any backend key: a Bedrock/Vertex flag or a
// socket redirects the check just like an API key.
func TestClaudeAuthDropsEveryBackendKey(t *testing.T) {
	l := &launcher{backend: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK"}}
	t.Setenv("ANTHROPIC_API_KEY", "leak")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "leak")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "leak")
	env := l.claudeAuth("/tmp/p", "status").Env
	for _, e := range env {
		if strings.HasSuffix(e, "=leak") {
			t.Fatalf("auth status inherited %s", e)
		}
	}
	// add sub reads the email from the profile it just logged into, never the global file.
	if v, _ := envGet(env, "CLAUDE_CONFIG_DIR"); v != "/tmp/p" {
		t.Fatalf("auth status must run against the given profile dir")
	}
}

func TestAPassOffAProviderStopsItsBridge(t *testing.T) {
	// The zsh driver stopped every helper after each pass, so a hop off a
	// provider began with its bridge gone. The Go launcher keeps one process
	// across passes, so the pass itself must stop a bridge it will not use.
	log := filepath.Join(t.TempDir(), "core.log")
	t.Setenv("CCM_FAKE_LOG", log)
	dir := t.TempDir()
	core := filepath.Join(dir, "ccmorph-core")
	if err := os.WriteFile(core, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$CCM_FAKE_LOG\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := func() *launcher {
		return &launcher{libexec: dir, providerPID: "4242", providerTD: "/td", providerAuth: "/auth",
			providerActive: "codex", providerPort: "1", providerKey: "k"}
	}
	logged := func() string {
		b, _ := os.ReadFile(log)
		return strings.TrimSpace(string(b))
	}

	l := fresh()
	l.stopForeignBridge("codex", true)
	if logged() != "" || l.providerActive != "codex" || l.providerPID != "4242" {
		t.Fatalf("staying on the provider must keep its bridge: %q active=%q", logged(), l.providerActive)
	}

	l.stopForeignBridge("gw", false)
	if got := logged(); got != "util bridge-stop 4242 /td /auth" {
		t.Fatalf("a pass off the provider must stop its bridge, logged %q", got)
	}
	if l.providerPID+l.providerTD+l.providerAuth+l.providerActive+l.providerPort+l.providerKey != "" {
		t.Fatalf("the stopped bridge's handle must be cleared: %+v", l)
	}

	l = fresh()
	l.stopForeignBridge("other", true)
	lines := strings.Split(logged(), "\n")
	if lines[len(lines)-1] != "util bridge-stop 4242 /td /auth" {
		t.Fatalf("switching providers must stop the old bridge first, logged %q", logged())
	}
}
