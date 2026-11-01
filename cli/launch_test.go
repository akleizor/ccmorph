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

// A stub ccmorph-core that records its argument vector and one environment value.
func stubCore(t *testing.T, envVar string) (dir string, argv func() string, env func() string) {
	t.Helper()
	dir = t.TempDir()
	read := func(name string) func() string {
		return func() string {
			b, _ := os.ReadFile(filepath.Join(dir, name))
			return string(b)
		}
	}
	body := "#!/bin/sh\n"
	body += "for a do printf '%s\\n' \"$a\"; done > '" + filepath.Join(dir, "argv.log") + "'\n"
	if envVar != "" {
		body += "printf '%s' \"${" + envVar + "-}\" > '" + filepath.Join(dir, "env.log") + "'\n"
		body += "printf 'A=1\\000.'\n" // launch-env's NUL-joined answer
	}
	// A session env file (the 7th operand) is copied, with its mode, while it exists.
	// cp -p keeps the mode: the Go side then reads it back, so the check is the
	// same on BSD and GNU userlands.
	body += "if [ -n \"${7-}\" ]; then cp -p \"$7\" '" + filepath.Join(dir, "file.log") + "'; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "ccmorph-core"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	envf := read("env.log")
	if envVar == "" {
		envf = func() string { return "" }
	}
	return dir, read("argv.log"), envf
}

// route passes the bridge key to launch-env through the environment: a credential
// on the argument vector is visible to anything running as this user (#112).
func TestRouteKeepsTheBridgeKeyOffTheCommandLine(t *testing.T) {
	dir, argv, env := stubCore(t, "_CCM_BRIDGE_KEY")
	l := &launcher{libexec: dir}
	got, ok := l.route([]string{"A=1"}, "9", "bridge-canary", false)
	if !ok {
		t.Fatal("route failed")
	}
	if !slices.Equal(got, []string{"A=1"}) {
		t.Fatalf("route returned %q", got)
	}
	if strings.Contains(argv(), "bridge-canary") {
		t.Fatalf("bridge key on the command line: %q", argv())
	}
	if env() != "bridge-canary" {
		t.Fatalf("launch-env saw _CCM_BRIDGE_KEY=%q", env())
	}
}

// runClaude hands the launch environment to claude-session through a 0600 file
// whose path is the only thing on the argument vector (#112).
func TestRunClaudeKeepsTheLaunchEnvironmentOffTheCommandLine(t *testing.T) {
	const key = "sk-ARGV-CANARY-launch-112"
	dir, argv, _ := stubCore(t, "")
	l := &launcher{libexec: dir}
	if rc := l.runClaude([]string{"ANTHROPIC_API_KEY=" + key, "FOO=1"}, []string{"--model", "x"}); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	seen := argv()
	if strings.Contains(seen, key) {
		t.Fatalf("credential on the command line: %q", seen)
	}
	// `util claude-session <tuning> <profile> <launch> <backend> <env file> -- ...`
	fields := strings.Split(strings.TrimSuffix(seen, "\n"), "\n")
	if len(fields) < 9 || fields[1] != "claude-session" || fields[7] != "--" {
		t.Fatalf("argv %q", seen)
	}
	envFile := fields[6]
	copied, err := os.Stat(filepath.Join(dir, "file.log"))
	if err != nil {
		t.Fatalf("the stub did not see the session env file: %v", err)
	}
	if copied.Mode().Perm() != 0o600 {
		t.Fatalf("session env file mode %o", copied.Mode().Perm())
	}
	body, _ := os.ReadFile(filepath.Join(dir, "file.log"))
	if !strings.Contains(string(body), "ANTHROPIC_API_KEY="+key) || !strings.Contains(string(body), "FOO=1") {
		t.Fatalf("session env file holds %q", body)
	}
	if _, err := os.Stat(envFile); !os.IsNotExist(err) {
		t.Fatalf("session env file %q left behind: %v", envFile, err)
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
