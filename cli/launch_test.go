package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/util"
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
// now refuses to start, and says why.
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
	if l.setupDir("p", "proxy") {
		t.Fatal("a launch that could not take its live lock must not start claude")
	}
	if _, err := os.Stat(filepath.Join(profiles, "p")); !os.IsNotExist(err) {
		t.Fatalf("a refused launch must not provision the profile either: %v", err)
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
	if !l.setupDir("a", "proxy") {
		t.Fatal("the first launch must start")
	}
	if !l.setupDir("b", "proxy") {
		t.Fatal("the hop target must start")
	}
	if !canTakeExclusive(t, util.LiveLockPath(profiles, "a")) {
		t.Fatal("after a hop, a launch of the hopped-away profile must find no live session")
	}
	if canTakeExclusive(t, util.LiveLockPath(profiles, "b")) {
		t.Fatal("the hop target must stay covered by its live lock")
	}
}
