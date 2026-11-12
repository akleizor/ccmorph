package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
)

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

// route points Claude Code at the local bridge with its key, in process: the key
// is never on any argument vector (#112).
func TestRoutePointsAtTheBridgeWithItsKey(t *testing.T) {
	l := &launcher{}
	got, ok := l.route([]string{"A=1"}, "9", "bridge-key", false)
	if !ok {
		t.Fatal("route failed")
	}
	if !slices.Equal(got, []string{"A=1", "ANTHROPIC_BASE_URL=http://127.0.0.1:9", "ANTHROPIC_API_KEY=bridge-key"}) {
		t.Fatalf("route returned %q", got)
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

// A launch still starts Claude Code when TMPDIR cannot be written: the env file
// falls back to the state dir, never to the argument vector (#112).
func TestRunClaudeStartsWhenTmpdirCannotBeWritten(t *testing.T) {
	const key = "canary-key-112"
	ro := t.TempDir()
	os.Chmod(ro, 0o555)
	t.Cleanup(func() { os.Chmod(ro, 0o755) })
	dir, argv, _ := stubCore(t, "")
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("TMPDIR", ro)
	l := &launcher{libexec: dir, dir: state}
	if rc := l.runClaude([]string{"ANTHROPIC_API_KEY=" + key}, nil); rc != 0 {
		t.Fatalf("claude did not start with an unwritable TMPDIR: rc %d", rc)
	}
	if strings.Contains(argv(), key) {
		t.Fatalf("credential on the command line: %q", argv())
	}
	copied, err := os.Stat(filepath.Join(dir, "file.log"))
	if err != nil || copied.Mode().Perm() != 0o600 {
		t.Fatalf("session env file: %v, %v", err, copied)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "file.log"))
	if !strings.Contains(string(b), "ANTHROPIC_API_KEY="+key) {
		t.Fatalf("session env file holds %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(state, "run", "ccmorph-session-env.*")); len(left) != 0 {
		t.Fatalf("env file left behind: %q", left)
	}
}

// readEndedSession returns the session id Claude Code's SessionEnd hook left
// for the claude that ran as pid, and consumes the record: a launcher that
// died is swept by gc-stale, never re-read. The file sits in a shared TMPDIR
// anyone can write, so a record that is not a session id reads as no session
// rather than reaching the terminal (#285).
func TestReadEndedSession(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := filepath.Join(tmp, "ccmorph-session-end.4194304")
	if err := os.WriteFile(f, []byte(" sid-285 \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readEndedSession(4194304); got != "sid-285" {
		t.Fatalf("read %q", got)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("record left behind: %v", err)
	}
	if err := os.WriteFile(f, []byte("not a session id"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readEndedSession(4194304); got != "" {
		t.Fatalf("a record that is not a session id read as %q", got)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatalf("record left behind: %v", err)
	}
	if got := readEndedSession(4194305); got != "" {
		t.Fatalf("another session's record read as %q", got)
	}
}

// runClaude reads the record the SessionEnd hook of the claude it waited on
// leaves behind, so the launcher can print the command that resumes that
// session on its profile (#285). The stub is the whole child: its own pid is
// the pid the record is keyed by.
func TestRunClaudeReadsTheEndedSessionRecord(t *testing.T) {
	dir := t.TempDir()
	body := "#!/bin/sh\nprintf %s sid-285 > \"$TMPDIR/ccmorph-session-end.$$\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ccmorph-core"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", t.TempDir())
	l := &launcher{libexec: dir}
	if rc := l.runClaude(nil, nil); rc != 0 {
		t.Fatalf("rc %d", rc)
	}
	if l.ended != "sid-285" {
		t.Fatalf("ended session %q", l.ended)
	}
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "ccmorph-session-end.*")); len(left) != 0 {
		t.Fatalf("records left behind: %q", left)
	}
}

// The hint may only offer a session Claude Code can resume. Claude Code names
// the transcript of the session its SessionEnd hook reports: one that holds no
// conversation — empty, or never written — belongs to a session closed before
// its first message (or a resume of an id Claude Code could not find), whose id
// draws Claude Code's own "No conversation found", and the failed resume run
// would print the same unusable command again (#359). A session that spoke keeps
// its hint (#285), and so does one whose record names no transcript: nothing
// says it is empty.
func TestResumeHintNeedsAConversationToOffer(t *testing.T) {
	dir := t.TempDir()
	// The record a first message writes: the identity a --resume looks for (#288).
	spoken := `{"type":"user","sessionId":"sid-359","uuid":"u1","timestamp":"2026-01-01T00:00:00.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "spoken.jsonl"), []byte(spoken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quiet.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	hint := "\nccmorph: resume this session with: ccmorph run work --resume sid-359\n"
	for _, c := range []struct {
		name       string
		transcript string
		want       string
	}{
		{"a session that spoke", filepath.Join(dir, "spoken.jsonl"), hint},
		{"a session that ended before its first message", filepath.Join(dir, "quiet.jsonl"), ""},
		{"a session whose transcript was never written", filepath.Join(dir, "ghost.jsonl"), ""},
		{"a record that names no transcript", "", hint},
	} {
		l := &launcher{ended: "sid-359", endedTranscript: c.transcript}
		if out, _ := captureStdout(t, func() int { l.resumeHint("work"); return 0 }); out != c.want {
			t.Errorf("%s: hint printed %q, want %q", c.name, out, c.want)
		}
		if l.ended != "" || l.endedTranscript != "" {
			t.Errorf("%s: the hint prints once, but left %q %q", c.name, l.ended, l.endedTranscript)
		}
	}
}

// readEndedTranscript returns the transcript path Claude Code gave the
// SessionEnd hook for the claude that ran as pid, and consumes the file the
// hook kept it in: a launcher that died is swept by gc-stale, never re-read.
// Input that is not Claude Code's JSON reads as no transcript (#359).
func TestReadEndedTranscript(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	f := filepath.Join(tmp, "ccmorph-session-end.4194304.json")
	for _, c := range []struct{ name, body, want string }{
		{"claude code's input", `{"session_id":"sid-359","transcript_path":"/p/sid-359.jsonl","hook_event_name":"SessionEnd"}`, "/p/sid-359.jsonl"},
		{"input that is not json", "not json", ""},
		{"empty input", "", ""},
	} {
		if err := os.WriteFile(f, []byte(c.body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := readEndedTranscript(4194304); got != c.want {
			t.Errorf("%s: read %q, want %q", c.name, got, c.want)
		}
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s: file left behind: %v", c.name, err)
		}
	}
	if got := readEndedTranscript(4194305); got != "" {
		t.Fatalf("another session's input read as %q", got)
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

// GH-138: with HOME unset a launch reads the account's home (the passwd entry),
// never a .claude in the cwd.
func TestHomeUnsetReadsTheAccountHomeNotTheCwd(t *testing.T) {
	root := t.TempDir()
	acct := filepath.Join(root, "acct")
	cwd := filepath.Join(root, "cwd")
	state := filepath.Join(root, "state")
	for _, d := range []string{
		filepath.Join(acct, ".claude"),
		filepath.Join(cwd, ".claude", "projects", "planted"),
		filepath.Join(state, "profiles", "p1"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(acct, ".claude", "settings.json"), `{"model":"from-account-home"}`)
	write(filepath.Join(cwd, ".claude", "settings.json"), `{"model":"planted-from-cwd"}`)
	write(filepath.Join(cwd, ".claude", "projects", "planted", "sess-138.jsonl"), "{}\n")
	write(filepath.Join(state, "p1.conf"), "type=token\n")

	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	t.Setenv("CCMORPH_STATE_DIR", state)
	t.Setenv("CCMORPH_CONTEXT_POLICY", "")
	t.Setenv("_CCMORPH_DIR", "")
	t.Setenv("_CCMORPH_PROFILE_DIRS", "")
	t.Setenv("_CCMORPH_CONTEXT_POLICY", "")
	t.Chdir(cwd)
	old := accountHome
	accountHome = func() string { return acct }
	t.Cleanup(func() { accountHome = old })

	l := fromEnv()
	if got := l.accountModel("p1"); got != "from-account-home" {
		t.Fatalf("model = %q, want the account home's, not the cwd's", got)
	}
	if _, ok := l.sessionFile("sess-138"); ok {
		t.Fatal("session found in the cwd's .claude/projects")
	}
	if want := filepath.Join(acct, ".agents", "runtime", "policies", "context.json"); l.policy != want {
		t.Fatalf("policy = %q, want %q", l.policy, want)
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

// INTENT promise 8: doctor never hangs. A claude that never answers `auth
// status` (a keychain prompt, a wedged install) must be cut off at the
// deadline and read as "cannot tell", not block the report forever. The stub's
// child keeps the output pipe open, as a real wedged claude's helpers do.
func TestSignInStatusStopsWaitingForAClaudeThatNeverAnswers(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	old := authStatusDeadline
	authStatusDeadline = 100 * time.Millisecond
	t.Cleanup(func() { authStatusDeadline = old })
	l := &launcher{profileDirs: t.TempDir()}
	start := time.Now()
	out, answered := l.signInStatus("demo")
	if answered || len(out) != 0 {
		t.Fatalf("a claude that never answered read as answered: %q", out)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("signInStatus waited %v for a claude that never answers", took)
	}
}

func TestAPassOffAProviderStopsItsBridge(t *testing.T) {
	// The zsh driver stopped every helper after each pass, so a hop off a
	// provider began with its bridge gone. The Go launcher keeps one process
	// across passes, so the pass itself must stop a bridge it will not use.
	// No pid: stopping removes the rendered config dir and signals nothing.
	fresh := func() (*launcher, string) {
		td := t.TempDir()
		return &launcher{providerTD: td, providerActive: "codex", providerPort: "1", providerKey: "k"}, td
	}

	l, td := fresh()
	l.stopForeignBridge("codex", true)
	if _, err := os.Stat(td); err != nil || l.providerActive != "codex" {
		t.Fatalf("staying on the provider must keep its bridge: %v active=%q", err, l.providerActive)
	}

	l, td = fresh()
	l.stopForeignBridge("gw", false)
	if _, err := os.Stat(td); err == nil {
		t.Fatal("a pass off the provider must stop its bridge")
	}
	if l.providerPID+l.providerTD+l.providerAuth+l.providerActive+l.providerPort+l.providerKey != "" {
		t.Fatalf("the stopped bridge's handle must be cleared: %+v", l)
	}

	l, td = fresh()
	l.stopForeignBridge("other", true)
	if _, err := os.Stat(td); err == nil {
		t.Fatal("switching providers must stop the old bridge first")
	}
}

// routeFixture is a provider profile with what a route needs: a state dir
// holding codex.conf and its auth directory, cliproxyapi on PATH (only its
// presence is asked for), and a stub ccmorph-core that logs and fakes a
// bridge-start and runs whatever hold the test asks route-hold to take.
type routeFixture struct {
	l     *launcher
	state string
	log   string
}

// coreCopy is this test binary under the name ccmorph-core: a hard link, or a
// copy where the temp dir is on another volume. Never a symlink: darwin names a
// process after the file a symlink resolves to, so a linked holder would read
// as cli.test, never as the core (#408).
func coreCopy(t *testing.T) string {
	t.Helper()
	core := filepath.Join(t.TempDir(), "ccmorph-core")
	if os.Link(os.Args[0], core) == nil {
		return core
	}
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(core, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	return core
}

// TestMain lets the route fixture run this test binary as the core's route
// hold: started as `<core> util route-hold <users>` with
// CCMORPH_TEST_HOLD_USERS set, it runs util's own route-hold, so the process
// table shows the fixture's holder the way it shows the real one (#408).
func TestMain(m *testing.M) {
	if os.Getenv("CCMORPH_TEST_HOLD_USERS") != "" && len(os.Args) > 1 && os.Args[1] == "util" {
		os.Exit(util.Main(os.Args[2:], os.Stdin, os.Stdout, io.Discard))
	}
	os.Exit(m.Run())
}

// The holder is this test binary under the name ccmorph-core running util's
// route-hold, so its executable and argv are what the real holder's are, and a
// retirement reading the process table can prove the PID is the route's holder
// before it signals it (#408).
func newRouteFixture(t *testing.T) *routeFixture {
	return newRouteFixtureWithHold(t,
		"CCMORPH_TEST_HOLD_USERS=\"$3\" exec '"+coreCopy(t)+"' util route-hold \"$3\"")
}

// TestRouteHoldChild is the working holder the route fixture's stub
// ccmorph-core runs for `util route-hold`: the test binary re-run, so no test
// depends on an interpreter being installed. It takes the shared lock the real
// holder takes, answers one byte and waits to be signaled. Run on its own it
// does nothing.
func TestRouteHoldChild(t *testing.T) {
	users := os.Getenv("CCMORPH_TEST_HOLD_USERS")
	if users == "" {
		return
	}
	f, err := os.OpenFile(users, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		os.Exit(1)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		os.Exit(1)
	}
	os.Stdout.WriteString("1")
	time.Sleep(24 * time.Hour)
}

// newRouteFixtureWithHold builds the fixture with route-hold running hold: the
// working holder takes the shared lock the real one takes and answers one byte.
func newRouteFixtureWithHold(t *testing.T, hold string) *routeFixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CCMORPH_PRELAUNCH", "")
	// macOS hands a session a TMPDIR ending in '/'. Every runner gets that
	// shape here, so a stub building paths from $TMPDIR fails in the sandbox
	// as it would under the gate.
	t.Setenv("TMPDIR", t.TempDir()+string(os.PathSeparator))
	state := t.TempDir()
	profiles := filepath.Join(state, "profiles")
	auth := filepath.Join(state, "auth", "codex")
	for _, d := range []string{profiles, auth} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "codex.conf"), []byte("type=provider\nprovider=codex\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "cliproxyapi"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	libexec := t.TempDir()
	starts := filepath.Join(libexec, "starts")
	// The stub bridge's config dir comes from a clean directory of the test's
	// own, never from $TMPDIR as given: ours() matches the cleaned
	// <tmp>/config.yaml against the bridge's argv, and a '//' from a TMPDIR
	// ending in '/' would make a live, held bridge read as dead.
	bridges := filepath.Clean(t.TempDir())
	body := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"'util route-hold')\n" +
		hold + "\n" +
		";;\n" +
		"'util bridge-start')\n" +
		"printf x >> '" + starts + "'\n" +
		"d=\"$(mktemp -d '" + bridges + "/gh300bridge.XXXXXX')\"\n" +
		"ln -s \"$(command -v sleep)\" \"$d/config.yaml\"\n" +
		"\"$d/config.yaml\" 60 >/dev/null 2>&1 &\n" +
		"printf 'PID=%d\\nPORT=4199\\nKEY=stub\\nTMPDIR=%s\\n' \"$!\" \"$d\"\n" +
		";;\n" +
		"*)\n" +
		"printf 'PID=99999\\nPORT=4199\\nKEY=stub\\nTMPDIR=" + libexec + "\\n'\n" +
		";;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(libexec, "ccmorph-core"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	fx := &routeFixture{
		l:     &launcher{dir: state, profileDirs: profiles, libexec: libexec},
		state: state,
		log:   starts,
	}
	// Backstop: whatever the record names when a test ends must not outlive it.
	t.Cleanup(func() {
		b, err := os.ReadFile(filepath.Join(state, "auth", ".codex.bridge"))
		if err != nil || len(strings.Fields(string(b))) == 0 {
			return
		}
		if n, err := strconv.Atoi(strings.Fields(string(b))[0]); err == nil {
			syscall.Kill(n, syscall.SIGTERM)
		}
	})
	return fx
}

// freshLauncher is another process's launcher over the same state: a session or
// a second route, with holds of its own.
func (fx *routeFixture) freshLauncher() *launcher {
	return &launcher{dir: fx.l.dir, profileDirs: fx.l.profileDirs, libexec: fx.l.libexec}
}

// holdUsers takes the shared lock on the bridge a session holds, without a
// bridge of its own; release ends it.
func (fx *routeFixture) holdUsers(t *testing.T) (release func()) {
	t.Helper()
	users, err := os.OpenFile(filepath.Join(fx.state, "auth", ".codex.bridge.users"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(users.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	return func() {
		syscall.Flock(int(users.Fd()), syscall.LOCK_UN)
		users.Close()
	}
}

// bridgeStarts is how many bridge-starts the stub core ran.
func (fx *routeFixture) bridgeStarts() int {
	b, _ := os.ReadFile(fx.log)
	return strings.Count(string(b), "x")
}

// sessionBridge starts the shared bridge a live session of the profile holds:
// a sleeper whose argv names its rendered config, as cliproxyapi's -config
// does, recorded in the handle a session would find. release ends that hold.
func (fx *routeFixture) sessionBridge(t *testing.T) (pid int, handle string, release func()) {
	t.Helper()
	tmp := t.TempDir()
	cmd := exec.Command("sleep", "60")
	cmd.Args[0] = filepath.Join(tmp, "config.yaml")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	t.Cleanup(func() { cmd.Process.Kill() })
	auth := filepath.Join(fx.state, "auth", "codex")
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	if err := os.WriteFile(record, []byte(strconv.Itoa(cmd.Process.Pid)+" 4100 skey "+tmp), 0o600); err != nil {
		t.Fatal(err)
	}
	users, err := os.OpenFile(filepath.Join(fx.state, "auth", ".codex.bridge.users"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(users.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid,
		"provider|" + strconv.Itoa(cmd.Process.Pid) + "|" + tmp + "|" + auth,
		func() {
			syscall.Flock(int(users.Fd()), syscall.LOCK_UN)
			users.Close()
		}
}

// captureStdout runs f with os.Stdout swapped for a pipe and returns what it
// printed, with f's exit status.
func captureStdout(t *testing.T, f func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	rc := f()
	os.Stdout = old
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), rc
}

// handleOf is the CCMORPH_ROUTE_HANDLE line a route env printed.
func handleOf(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(line, "CCMORPH_ROUTE_HANDLE="); ok {
			return v
		}
	}
	return ""
}

// holderOf is the holder pid that handle names — the process owning the route's
// use of the bridge — "" when the handle names none.
func holderOf(out string) string {
	if parts := strings.Split(handleOf(out), "|"); len(parts) >= 5 {
		return parts[4]
	}
	return ""
}

// stopHolder ends a holder a route env spawned, so a test that does not retire
// its route leaves nothing behind.
func stopHolder(holder string) {
	if n, err := strconv.Atoi(holder); err == nil && n > 0 {
		syscall.Kill(n, syscall.SIGTERM)
	}
}

// A route env for a provider profile runs on the profile's one shared bridge.
// It used to start a private one beside it, and two bridges on one auth
// directory spend the same single-use refresh token twice, signing the profile
// out (#300).
func TestARouteEnvForAProviderRunsOnTheSharedBridge(t *testing.T) {
	t.Run("beside a session's shared bridge", func(t *testing.T) {
		fx := newRouteFixture(t)
		pid, handle, release := fx.sessionBridge(t)
		defer release()
		out, rc := captureStdout(t, func() int { return fx.l.routeEnv([]string{"codex"}) })
		if rc != 0 {
			t.Fatalf("route env refused: rc=%d out=%q", rc, out)
		}
		if n := fx.bridgeStarts(); n != 0 {
			t.Fatalf("a route env beside the shared bridge started %d private bridge(s)", n)
		}
		if !strings.Contains(out, "CCMORPH_ROUTE_HANDLE="+handle) {
			t.Fatalf("the route must hand back the shared bridge's handle %q: %q", handle, out)
		}
		if !strings.Contains(out, "ANTHROPIC_BASE_URL=http://127.0.0.1:4100") || !strings.Contains(out, "ANTHROPIC_API_KEY=skey") {
			t.Fatalf("the route must point at the shared bridge: %q", out)
		}
		if syscall.Kill(pid, 0) != nil {
			t.Fatal("the shared bridge must keep running for the route")
		}
		if h := holderOf(out); h == "" {
			t.Fatalf("the route must own its use of the shared bridge through a holder: %q", out)
		} else {
			t.Cleanup(func() { stopHolder(h) })
		}
	})
	t.Run("with nothing running", func(t *testing.T) {
		fx := newRouteFixture(t)
		out, rc := captureStdout(t, func() int { return fx.l.routeEnv([]string{"codex"}) })
		if rc != 0 {
			t.Fatalf("route env refused: rc=%d out=%q", rc, out)
		}
		if n := fx.bridgeStarts(); n != 1 {
			t.Fatalf("a route env with nothing running must start one bridge, started %d", n)
		}
		record := filepath.Join(fx.state, "auth", ".codex.bridge")
		b, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the bridge a route starts must be the recorded shared one: %v", err)
		}
		if !strings.Contains(out, "CCMORPH_ROUTE_HANDLE=provider|"+strings.Fields(string(b))[0]+"|") {
			t.Fatalf("the handle must name the recorded bridge: %q vs record %q", out, b)
		}
		if h := holderOf(out); h == "" {
			t.Fatalf("the route must own its use of the shared bridge through a holder: %q", out)
		} else {
			t.Cleanup(func() { stopHolder(h) })
		}
	})
}

// A session ending leaves the bridge a route uses running: the route's hold —
// a holder process that lives until the route retires — owns the bridge with
// the sessions, so the last session out cannot stop it under a live route
// (#300). Before the hold, that release stopped the shared bridge the route
// ran on.
func TestASessionEndingLeavesABridgeARouteUsesRunning(t *testing.T) {
	fx := newRouteFixture(t)
	out, rc := captureStdout(t, func() int { return fx.l.routeEnv([]string{"codex"}) })
	if rc != 0 {
		t.Fatalf("route env refused: rc=%d out=%q", rc, out)
	}
	holder := holderOf(out)
	t.Cleanup(func() { stopHolder(holder) })
	if n := fx.bridgeStarts(); n != 1 {
		t.Fatalf("the route must start the one shared bridge, started %d", n)
	}
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the bridge a route starts must be recorded: %v", err)
	}
	bridgePID, _ := strconv.Atoi(strings.Fields(string(before))[0])
	// A session of the profile joins the bridge the route started, then ends.
	s := fx.freshLauncher()
	if !s.providerStart("codex") {
		t.Fatal("the session must join the shared bridge")
	}
	if n := fx.bridgeStarts(); n != 1 {
		t.Fatalf("the session must join, not replace, the route's bridge; %d started", n)
	}
	fx.l.providerStop() // the route env process ends with its print
	if syscall.Kill(bridgePID, 0) != nil {
		t.Fatal("the route env process ending must not stop the bridge the route uses")
	}
	s.providerStop()
	if syscall.Kill(bridgePID, 0) != nil {
		t.Fatal("a session ending must not stop the bridge a live route uses")
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatalf("the route's bridge and its record must outlive the session: %q err=%v", after, err)
	}
	if holder == "" {
		t.Fatalf("the route must own its use of the shared bridge through a holder: %q", out)
	}
	// With the route itself gone, its retirement owns the stop.
	if rc := fx.freshLauncher().routeStop([]string{handleOf(out)}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(bridgePID, 0) == nil {
		t.Fatal("once nothing uses it, retiring the route must stop the bridge")
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the handle must be removed with the bridge")
	}
}

// One of two routes retiring leaves the bridge the other uses running: each
// route owns its own hold, so the first retirement must not stop the bridge
// under the second (#300).
func TestOneOfTwoRoutesRetiringLeavesTheOthersBridgeRunning(t *testing.T) {
	fx := newRouteFixture(t)
	out1, rc := captureStdout(t, func() int { return fx.l.routeEnv([]string{"codex"}) })
	if rc != 0 {
		t.Fatalf("route env refused: rc=%d out=%q", rc, out1)
	}
	holder1 := holderOf(out1)
	// A session of the profile uses the bridge while the second route is built,
	// then leaves: both routes, and the bridge, outlive it.
	release := fx.holdUsers(t)
	r2 := fx.freshLauncher()
	out2, rc := captureStdout(t, func() int { return r2.routeEnv([]string{"codex"}) })
	if rc != 0 {
		t.Fatalf("route env refused: rc=%d out=%q", rc, out2)
	}
	holder2 := holderOf(out2)
	t.Cleanup(func() { stopHolder(holder1); stopHolder(holder2) })
	if n := fx.bridgeStarts(); n != 1 {
		t.Fatalf("two routes must share one bridge; %d started", n)
	}
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("the shared bridge must be recorded: %v", err)
	}
	bridgePID, _ := strconv.Atoi(strings.Fields(string(before))[0])
	fx.l.providerStop() // each route env process ends with its print
	r2.providerStop()
	release() // the session leaves
	if rc := fx.freshLauncher().routeStop([]string{handleOf(out1)}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(bridgePID, 0) != nil {
		t.Fatal("one route retiring must not stop the bridge the other still uses")
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatalf("the surviving route's bridge and record must stay: %q err=%v", after, err)
	}
	if holder1 == "" || holder2 == "" || holder2 == holder1 {
		t.Fatalf("each route must own its own hold: %q vs %q", out1, out2)
	}
	if rc := fx.freshLauncher().routeStop([]string{handleOf(out2)}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(bridgePID, 0) == nil {
		t.Fatal("once no route uses it, retiring the last one must stop the bridge")
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the handle must be removed with the bridge")
	}
}

// Retiring a route whose bridge was replaced keeps the replacement: the handle
// names a bridge that is gone and the record now names a newer one, so removing
// it would leave that bridge unrecorded and let the next acquire start a second
// bridge beside it on the same auth directory (#300).
func TestRetiringARouteWhoseBridgeWasReplacedKeepsTheReplacement(t *testing.T) {
	fx := newRouteFixture(t)
	pid, _, release := fx.sessionBridge(t)
	release() // the bridge is recorded and running, but nothing holds it
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	stale := "provider|777777|/gh300-replaced|" + filepath.Join(fx.state, "auth", "codex")
	if rc := (&launcher{}).routeStop([]string{stale}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("retiring a replaced route must not stop the replacement bridge")
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatalf("the replacement's record must survive the stale handle's retirement: %q err=%v", after, err)
	}
}

// A route whose hold on the bridge never lands is refused with a plain reason
// and leaves nothing behind: running it unprotected would hand the route a
// bridge any session's exit could stop (#300, INTENT promise 8).
func TestARouteWhoseHoldNeverLandsIsRefused(t *testing.T) {
	fx := newRouteFixtureWithHold(t, "printf '0\\n'")
	out, rc := captureStdout(t, func() int { return fx.l.routeEnv([]string{"codex"}) })
	if rc == 0 {
		t.Fatalf("a route whose hold never lands must be refused: %q", out)
	}
	if strings.Contains(out, "CCMORPH_ROUTE_HANDLE=") {
		t.Fatalf("a refused route must hand back no handle: %q", out)
	}
	if n := fx.bridgeStarts(); n != 1 {
		t.Fatalf("the refused route still started its bridge first, started %d", n)
	}
	if _, err := os.Stat(filepath.Join(fx.state, "auth", ".codex.bridge")); err == nil {
		t.Fatal("a refused route must leave no bridge recorded")
	}
}

// TestRouteEnvChild is the separate _route_env process
// TestARouteEnvsCapturedOutputEndsWhenItExits captures: the test binary re-run
// into it. Run on its own it does nothing.
func TestRouteEnvChild(t *testing.T) {
	state := os.Getenv("CCMORPH_TEST_ROUTE_ENV_STATE")
	if state == "" {
		return
	}
	l := &launcher{dir: state, profileDirs: filepath.Join(state, "profiles"), libexec: os.Getenv("CCMORPH_TEST_ROUTE_ENV_LIBEXEC")}
	os.Exit(l.routeEnv([]string{"codex"}))
}

// A caller that captures _route_env's output — Remote Control's
// subprocess.run(capture_output=True), probe's runZsh — sees it end when the
// env process exits. The holder outlives that process by design, so it must
// keep none of the caller's streams: holding the inherited stderr left Remote
// Control waiting out its 180-second timeout, and probe its WaitDelay, on a
// route already built, whose retirement is registered only after the capture
// returns (#300).
func TestARouteEnvsCapturedOutputEndsWhenItExits(t *testing.T) {
	fx := newRouteFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRouteEnvChild$")
	cmd.Env = append(os.Environ(),
		"CCMORPH_TEST_ROUTE_ENV_STATE="+fx.state,
		"CCMORPH_TEST_ROUTE_ENV_LIBEXEC="+fx.l.libexec)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	h := holderOf(out.String())
	t.Cleanup(func() { stopHolder(h) })
	if errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("the route env exited, but a process it left running kept its caller's capture open: holder %q, stdout %q", h, out.String())
	}
	if err != nil {
		t.Fatalf("route env failed: %v stdout=%q stderr=%q", err, out.String(), errb.String())
	}
	if h == "" {
		t.Fatalf("the route must own its use of the shared bridge through a holder: %q", out.String())
	}
	if n, _ := strconv.Atoi(h); syscall.Kill(n, 0) != nil {
		t.Fatal("the holder must outlive the env process: only _route_stop ends the route's hold")
	}
	users, err := os.OpenFile(filepath.Join(fx.state, "auth", ".codex.bridge.users"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer users.Close()
	if syscall.Flock(int(users.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		t.Fatal("the route's hold on the bridge must outlive the env process")
	}
}

// Retiring a route leaves the shared bridge a session still uses running: only
// the last user out — a session, or a route whose hold has died with its
// holder — owns the stop (#300).
func TestRetiringARouteLeavesABridgeASessionUsesRunning(t *testing.T) {
	fx := newRouteFixture(t)
	pid, handle, release := fx.sessionBridge(t)
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	if rc := (&launcher{}).routeStop([]string{handle}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("retiring a route must not stop a bridge a session still uses")
	}
	if _, err := os.Stat(record); err != nil {
		t.Fatal("retiring a route must leave the handle a later session joins")
	}
	release()
	if rc := (&launcher{}).routeStop([]string{handle}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Fatal("once no session uses it, retiring the route must stop the bridge")
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("the handle must be removed with the bridge")
	}
}

// A route's handle names its holder by number alone. When the system has
// reused that number for a session of the profile, retirement must read the
// process table before it signals, or it ends the session's hold and stops
// the bridge under it (#408).
func TestRetiringARouteWhoseHolderPidWasReusedLeavesTheSessionsBridge(t *testing.T) {
	fx := newRouteFixture(t)
	pid, handle, release := fx.sessionBridge(t)
	release() // only the session below holds the bridge now
	users := filepath.Join(fx.state, "auth", ".codex.bridge.users")
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	session := exec.Command(os.Args[0], "-test.run=^TestRouteHoldChild$")
	session.Env = append(os.Environ(), "CCMORPH_TEST_HOLD_USERS="+users)
	session.Stdout = pw
	if err := session.Start(); err != nil {
		t.Fatal(err)
	}
	pw.Close()
	go session.Wait()
	t.Cleanup(func() { session.Process.Kill() })
	pr.SetReadDeadline(time.Now().Add(10 * time.Second))
	one := make([]byte, 1)
	if _, err := io.ReadFull(pr, one); err != nil || one[0] != '1' {
		t.Fatalf("the session never took its hold on the bridge: %v %q", err, one)
	}
	pr.Close()
	record := filepath.Join(fx.state, "auth", ".codex.bridge")
	before, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if rc := (&launcher{}).routeStop([]string{handle + "|" + strconv.Itoa(session.Process.Pid)}); rc != 0 {
		t.Fatalf("route stop failed: rc=%d", rc)
	}
	if syscall.Kill(pid, 0) != nil {
		t.Fatal("retiring a route must not stop a bridge a session still uses")
	}
	if after, err := os.ReadFile(record); err != nil || string(after) != string(before) {
		t.Fatalf("the record must stay for the session's bridge: %v %q", err, after)
	}
	if syscall.Kill(session.Process.Pid, 0) != nil {
		t.Fatal("retiring a route must never signal a PID that does not run its hold")
	}
}

// TestEveryStageNamesItsOperands keeps the stage table and its usage lines in step:
// a stage added without one would still panic on a short argv (#162).
func TestEveryStageNamesItsOperands(t *testing.T) {
	for name := range stages {
		if _, ok := stageOperands[name]; !ok {
			t.Errorf("stage %s has no usage line", name)
		}
	}
	for name := range stageOperands {
		if stages[name] == nil {
			t.Errorf("usage line for %s names no stage", name)
		}
	}
	if got, short := stageShort([]string{"--model-known"}); !short || got != "usage: ccmorph-core launch --model-known <profile> <model>" {
		t.Fatalf("short --model-known: %q %v", got, short)
	}
	if _, short := stageShort([]string{"--model-known", "gw", "glm-5.2"}); short {
		t.Fatal("a full --model-known still refused")
	}
}

// The usage hook is the transcript reader a launch installs through settings:
// it changes no network path, so it covers sub, token and provider profiles
// alike, and never touches an API-key or direct one. Building it must not start
// the models.dev refresh: that belongs to the launch, and a unit test that took
// this path would reach the network and leave a temp home behind.
func TestUsageHookCoversSubTokenAndProvider(t *testing.T) {
	refreshed := 0
	prev := refreshOwnedPrices
	refreshOwnedPrices = func(string) { refreshed++ }
	t.Cleanup(func() { refreshOwnedPrices = prev })
	l := &launcher{libexec: "/opt/cc/libexec", profileDirs: "/p"}
	cases := []struct{ name, typ, route, wantPricing string }{
		{"sub", "sub", "", "anthropic"},
		{"token", "token", "", "anthropic"},
		{"codex", "provider", "", "any"},
		{"other", "sub", "direct", ""},
		{"api", "apikey", "", ""},
		{"api", "apikey", "direct", ""},
	}
	for _, c := range cases {
		hook := l.usageHookFor(c.name, c.typ, c.route)
		if c.wantPricing == "" {
			if hook != "" {
				t.Fatalf("%s/%s/%s: no hook expected, got %q", c.name, c.typ, c.route, hook)
			}
			continue
		}
		want := "/opt/cc/libexec/ccmorph-core usage-row --profile " + c.name + " --pricing " + c.wantPricing
		if hook != want {
			t.Fatalf("%s/%s: hook = %q, want %q", c.name, c.typ, hook, want)
		}
	}
	if refreshed != 0 {
		t.Fatalf("building a usage hook started %d price refreshes", refreshed)
	}
}

// #287: under Homebrew the running binary sits in the versioned Cellar dir,
// which a `brew upgrade` moves and a `brew cleanup` deletes, so a hook that
// names a file under it dies with the version that wrote it. The hook names
// the stable opt/ccmorph path brew keeps pointed at the current version
// instead; only a Cellar-shaped path that is not a brew install (no opt link,
// so no core behind it) keeps naming the binary that wrote it.
func TestUsageHookUnderACellarPathNamesTheStableOptPath(t *testing.T) {
	home := t.TempDir()
	cellar := filepath.Join(home, "Cellar", "ccmorph", "0.5.7", "libexec", "libexec")
	if err := os.MkdirAll(cellar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cellar, "ccmorph-core"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	opt := filepath.Join(home, "opt", "ccmorph")
	if err := os.MkdirAll(filepath.Dir(opt), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "Cellar", "ccmorph", "0.5.7"), opt); err != nil {
		t.Fatal(err)
	}
	l := &launcher{libexec: cellar, profileDirs: "/p"}
	want := filepath.Join(opt, "libexec", "libexec", "ccmorph-core") +
		" usage-row --profile sub --pricing anthropic"
	if hook := l.usageHookFor("sub", "sub", ""); hook != want {
		t.Fatalf("hook = %q, want %q", hook, want)
	}
	// A Cellar-shaped path with no stable link behind it is not a brew
	// install: the hook keeps naming the binary that wrote it.
	if err := os.Remove(opt); err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(cellar, "ccmorph-core") + " usage-row --profile sub --pricing anthropic"
	if hook := l.usageHookFor("sub", "sub", ""); hook != want {
		t.Fatalf("without the opt link, hook = %q, want %q", hook, want)
	}
}

func TestSettingsArgvCarriesTheHookOnlyWhenOneIsSet(t *testing.T) {
	l := &launcher{profileDirs: "/p", mapping: []string{"ANTHROPIC_AUTH_TOKEN"}}
	if got := l.settingsArgv("materialize", "sub"); slices.Contains(got, "--usage-hook=") {
		t.Fatalf("a launch without a hook carried one: %v", got)
	}
	l.usageHook = "ccmorph-core usage-row --profile sub --pricing anthropic"
	got := l.settingsArgv("materialize", "sub")
	want := "--usage-hook=" + l.usageHook
	if !slices.Contains(got, want) {
		t.Fatalf("argv = %v, want %q", got, want)
	}
	if !slices.Contains(got, "--mapping=ANTHROPIC_AUTH_TOKEN") {
		t.Fatalf("argv lost the mapping: %v", got)
	}
}

// #205: a launch that cannot take its profile's live lock (a stuck exclusive
// holder outlasted the wait) discarded the nil HoldLive returned and started
// claude with no lock while the other launch's merge was still in flight. It
// now refuses to start, and says why. #217 kept this on the paths that start
// claude only: setupDir's other callers no longer refuse.
func TestSetupDirRefusesALaunchWithoutItsLiveLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	lock := util.LiveLockPath(profiles, "p")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(util.SetLockWait(0))
	l := &launcher{profileDirs: profiles}
	if l.setupDir("p", "proxy", liveRefuse) {
		t.Fatal("a launch that could not take its live lock must not start claude")
	}
	if _, err := os.Stat(filepath.Join(profiles, "p")); !os.IsNotExist(err) {
		t.Fatalf("a refused launch must not provision the profile either: %v", err)
	}
}

// #258: seeding rewrites the profile's .claude.json under its live lock taken
// exclusively, and a failed upgrade can leave the launch holding nothing (Linux
// drops the shared lock first). Claude must not start in that window — the same
// re-check setup does after a merge (#185, #205): the answers wait, and a launch
// that cannot take its live lock again refuses to start.
func TestPersonAnswersRequiresTheLiveLockBeforeClaudeStarts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	lock := util.LiveLockPath(profiles, "p")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(util.SetLockWait(0))
	l := &launcher{profileDirs: profiles}
	saved := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	ok := l.personAnswers("p", false)
	w.Close()
	os.Stderr = saved
	note, _ := io.ReadAll(r)
	if ok {
		t.Fatal("a launch that could not take its live lock must not start claude")
	}
	if len(note) == 0 {
		t.Fatal("the refusal must name what blocks the session")
	}
}

// #217: the refusal #205 wired into setupDir ran in every caller, so `use`
// failed over a lock it never needed and `add` rolled back the profile it had
// just written. The verbs that start no session proceed under a lock they
// cannot take: silently for `use`, which only sets state, and naming the real
// cause for `add`, whose profile must survive a stuck holder or a lock file
// that cannot be made.
func TestSetupDirProceedsForVerbsThatStartNoSession(t *testing.T) {
	for _, tc := range []struct {
		policy   livePolicy
		wantNote bool
	}{
		{liveSilent, false},
		{liveNote, true},
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		profiles := filepath.Join(home, ".config/ccmorph/profiles")
		lock := util.LiveLockPath(profiles, "p")
		if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
			t.Fatal(err)
		}
		holder, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(util.SetLockWait(0))
		l := &launcher{profileDirs: profiles}
		saved := os.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		ok := l.setupDir("p", "proxy", tc.policy)
		w.Close()
		os.Stderr = saved
		noteBytes, _ := io.ReadAll(r)
		holder.Close()
		if !ok {
			t.Fatalf("policy %d: a verb that starts no session must proceed without the lock", tc.policy)
		}
		note := string(noteBytes)
		if tc.wantNote && note == "" {
			t.Fatalf("policy %d: the cause must be named, not passed silently", tc.policy)
		}
		if !tc.wantNote && note != "" {
			t.Fatalf("policy %d: a verb that only sets state must say nothing: %q", tc.policy, note)
		}
		if fi, err := os.Stat(filepath.Join(profiles, "p")); err != nil || !fi.IsDir() {
			t.Fatalf("policy %d: the profile must be provisioned, not refused: %v", tc.policy, err)
		}
	}
}

// canTakeExclusive reports whether a fresh probe can take the lock at path
// exclusively right now.
func canTakeExclusive(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// #205: the launcher loops in-process across a hop, and HoldLive cached one fd
// per lock path for the process's life. After A -> B, A's live lock was still
// held while only B ran, so another launch of A was told a session was live and
// deferred A's merges for the whole hopped session.
func TestSetupDirReleasesTheLiveLockOfTheProfileAHopLeaves(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	t.Cleanup(func() {
		util.ReleaseLive(profiles, "a")
		util.ReleaseLive(profiles, "b")
	})
	l := &launcher{profileDirs: profiles}
	if !l.setupDir("a", "proxy", liveRefuse) {
		t.Fatal("the first launch must start")
	}
	if !l.setupDir("b", "proxy", liveRefuse) {
		t.Fatal("the hop target must start")
	}
	if !canTakeExclusive(t, util.LiveLockPath(profiles, "a")) {
		t.Fatal("after a hop, a launch of the hopped-away profile must find no live session")
	}
	if canTakeExclusive(t, util.LiveLockPath(profiles, "b")) {
		t.Fatal("the hop target must stay covered by its live lock")
	}
}

// coreCalls is a stub ccmorph-core that appends each invocation's argument
// vector to a log, so a test can see every call, not only the last one.
func coreCalls(t *testing.T) (dir string, calls func() string) {
	t.Helper()
	dir = t.TempDir()
	log := filepath.Join(dir, "calls.log")
	body := "#!/bin/sh\n{ for a do printf '%s\\n' \"$a\"; done; printf '.\\n'; } >> '" + log + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "ccmorph-core"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
}

// A gateway that rejects the key refuses the launch before any adapter starts,
// names the key and the one command that replaces it — doctor's own — whichever
// model is named or none, and carries the rejected key to the listing once
// (#374).
func TestAGatewayThatRejectsTheKeyRefusesTheLaunchBeforeAnyAdapter(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"a named model", []string{"--model", "alpha-opus-1", "-p", "hi"}},
		{"no model", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			state, profiles := t.TempDir(), t.TempDir()
			libexec, calls := coreCalls(t)
			os.MkdirAll(filepath.Join(profiles, "gw374"), 0o755)
			os.WriteFile(filepath.Join(state, "gw374.conf"), []byte(
				"type=apikey\nbase_url=http://127.0.0.1:18081\napi_key=sk-wrong-374\n"), 0o600)

			probes := 0
			old := probeListing
			probeListing = func(base, key string, headers ...string) (string, string) {
				probes++
				if base != "http://127.0.0.1:18081" || key != "sk-wrong-374" {
					t.Errorf("the pre-flight asked %q with key %q", base, key)
				}
				return "", "401"
			}
			t.Cleanup(func() { probeListing = old })

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			saved := os.Stderr
			os.Stderr = w
			l := &launcher{dir: state, profileDirs: profiles, libexec: libexec, launchEnv: profile.LaunchEnv, memo: map[string]string{}}
			_, rc := l.launch("gw374", false, false, tc.argv)
			w.Close()
			os.Stderr = saved
			outB, _ := io.ReadAll(r)
			out := string(outB)

			if rc != 1 {
				t.Fatalf("rc %d, want 1: a launch whose key the gateway rejected must refuse", rc)
			}
			if !strings.Contains(out, "rejected the API key") || !strings.Contains(out, "HTTP 401") {
				t.Fatalf("the failure's first line must say the key was rejected, got: %s", out)
			}
			if !strings.Contains(out, "ccmorph config gw374") {
				t.Fatalf("the failure must name the command that replaces the key, got: %s", out)
			}
			if strings.Contains(out, "adapter") {
				t.Fatalf("a profile doctor says needs no translator must not mention an adapter, got: %s", out)
			}
			if log := calls(); strings.Contains(log, "adapter-start") {
				t.Fatalf("the adapter started for a rejected key:\n%s", log)
			}
			if probes != 1 {
				t.Fatalf("%d listing request(s) carried the rejected key; want 1", probes)
			}
		})
	}
}

// A refusal the adapter worded itself — the cause and the one command that
// repairs it — is the whole failure: the launcher must not repeat the cause or
// name a command that disagrees with it. A bare death still gets the route
// checks (#286, #374).
func TestAnAdapterRefusalStandsAlone(t *testing.T) { // issue #374
	refusal := "ccmorph: the endpoint rejected the API key (HTTP 401); replace it: ccmorph config gw374b"
	for _, tc := range []struct {
		name    string
		body    string
		why     string
		refused bool
	}{
		{"a refused key", "echo " + util.Refused + "; echo '" + refusal + "' >&2; exit 1", "rejected the API key", true},
		{"a bare death", "echo 'ccmorph: gw374b: adapter exited before readiness; session not started.' >&2; exit 1", "exited before readiness", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, profiles, libexec := t.TempDir(), t.TempDir(), t.TempDir()
			os.MkdirAll(filepath.Join(profiles, "gw374b"), 0o755)
			os.WriteFile(filepath.Join(state, "gw374b.conf"), []byte(
				"type=apikey\nbase_url=http://127.0.0.1:9\napi_key=k"), 0o600)
			os.WriteFile(filepath.Join(libexec, "ccmorph-core"), []byte("#!/bin/sh\n"+tc.body+"\n"), 0o755)

			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			saved := os.Stderr
			os.Stderr = w
			l := &launcher{dir: state, profileDirs: profiles, libexec: libexec, launcherPID: os.Getpid()}
			ok := l.adapterStart("gw374b", "")
			w.Close()
			os.Stderr = saved
			outB, _ := io.ReadAll(r)
			out := string(outB)

			if ok {
				t.Fatal("the adapter start reported success for a failed adapter")
			}
			if !strings.Contains(out, tc.why) {
				t.Fatalf("the failure must say why in plain words, got: %s", out)
			}
			if tc.refused {
				if strings.Count(out, tc.why) != 1 {
					t.Fatalf("the refusal must appear once, not once per surface, got: %s", out)
				}
				if strings.Contains(out, "Check the route") {
					t.Fatalf("a refusal that names its own repair must not point at doctor as well, got: %s", out)
				}
			} else if !strings.Contains(out, "Check the route: ccmorph doctor gw374b") {
				t.Fatalf("a bare death still names the route check, got: %s", out)
			}
		})
	}
}

// The probe verdict is the one status switch every probe of a credential reads
// (#488): the branches #255, #286 and #374 each landed in, spelled four times
// before. Pinned here once — including the three-digit guard, so a status a
// probe can never answer still reads as no verdict — so the sites that call it
// cannot drift apart.
func TestProbeVerdictSortsEveryStatusIntoItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		code string
		want probeStatus
	}{
		{"200", probeAccepted},
		{"204", probeAccepted},
		{"299", probeAccepted},
		{"401", probeRejected},
		{"403", probeRejected},
		{"000", probeSilent},
		{"", probeSilent},
		{"302", probeOther},
		{"500", probeOther},
		{"503", probeOther},
		{"20", probeOther},
		{"2000", probeOther},
	} {
		if got := probeVerdict(tc.code); got != tc.want {
			t.Errorf("probeVerdict(%q) = %d, want %d", tc.code, got, tc.want)
		}
	}
}

// nativeBase owns Anthropic's own endpoint (#488): the URL an empty base_url
// means, and the spellings that count as that endpoint — the bare URL and its
// trailing-slash form — which run's and inspect's switches each spelled three
// ways because no one place normalized them. A base that is not Anthropic's
// own comes back untouched, so no message ever rewrites a typed endpoint.
func TestNativeBaseOwnsTheDefaultAndItsSpellings(t *testing.T) {
	for _, tc := range []struct {
		base   string
		wantB  string
		native bool
	}{
		{"", "https://api.anthropic.com", true},
		{"https://api.anthropic.com", "https://api.anthropic.com", true},
		{"https://api.anthropic.com/", "https://api.anthropic.com/", true},
		{"https://gateway.example", "https://gateway.example", false},
		{"http://127.0.0.1:18081", "http://127.0.0.1:18081", false},
	} {
		if b, native := nativeBase(tc.base); b != tc.wantB || native != tc.native {
			t.Errorf("nativeBase(%q) = (%q, %t), want (%q, %t)", tc.base, b, native, tc.wantB, tc.native)
		}
	}
}
