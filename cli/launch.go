// Package cli is the `ccmorph` command in the core. `ccmorph-core launch` is the
// run loop that was zsh's
// _ccm_launch_driver and _ccm_launch. It resolves the profile and model, starts
// the provider bridge or adapter, projects settings, runs Claude Code as a child
// with TERM/HUP forwarded, and relaunches on a hop's relay request. Each step it
// owns calls the same core verbs the zsh did, so their behaviour is unchanged.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
)

type launcher struct {
	dir, libDir, libexec, profileDirs, policy, logPath string
	tuning, profileEnv, launchEnv, backend, mapping    []string
	logSet                                             bool
	launcherPID                                        int

	red, yellow, bold, dim, accent, cyan, reset string

	memo                map[string]string
	version, zshVersion string
	migrated            bool

	adapterPID              int
	adapterProfile          string
	adapterPort             string
	providerPID, providerTD string
	providerAuth            string
	providerActive          string
	providerPort            string
	providerKey             string
	usageHook               string

	mu        sync.Mutex
	child     *os.Process
	forwarded int
	notice    string
	chosen    map[string]bool
}

// relayNext is what one launch hands the driver when Claude Code exited for a hop.
type relayNext struct {
	args             []string
	profile, session string
	crossed          bool
}

// Main runs one `ccmorph run`: the zsh wrapper exports the paths and env-key
// lists it owns as _CCMORPH_* and the run's own state as _CCMORPH_LAUNCH_*.
func Main(args []string) int {
	l := fromEnv()
	l.theme()
	return l.launchMain(args)
}

func (l *launcher) launchMain(args []string) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go l.signals(sigs)
	defer l.stopHelpers()

	// One stage of the launch, for the zsh entry points and the tests.
	if len(args) > 0 {
		if stage, ok := stages[args[0]]; ok {
			if usage, short := stageShort(args); short {
				fmt.Fprintln(os.Stderr, usage)
				return 2
			}
			return stage(l, args)
		}
	}
	profileName := os.Getenv("_CCMORPH_LAUNCH_PROFILE")
	interactive := os.Getenv("_CCMORPH_INTERACTIVE") == "1"
	native := os.Getenv("_CCMORPH_NATIVE_ARGS") == "1"
	for _, k := range []string{"_CCMORPH_LAUNCH_PROFILE", "_CCMORPH_INTERACTIVE", "_CCMORPH_NATIVE_ARGS"} {
		os.Unsetenv(k)
	}
	for {
		next, rc := l.launch(profileName, interactive, native, args)
		l.notice = ""
		if next == nil {
			return rc
		}
		args, profileName, interactive, native = next.args, next.profile, false, true
		l.log("relay resume profile=" + next.profile + " session=" + next.session)
		fmt.Printf("\n%sccmorph:%s resuming as %s%s%s...\n\n", l.accent, l.reset, l.bold, next.profile, l.reset)
		if next.crossed {
			l.notice = "Switching accounts or backends may rebuild the conversation cache on your first message and charge for the history again."
		}
	}
}

func fromEnv() *launcher {
	l := &launcher{memo: map[string]string{}}
	get := func(k string) string { v := os.Getenv(k); os.Unsetenv(k); return v }
	l.dir = get("_CCMORPH_DIR")
	l.libDir = get("_CCMORPH_LIB_DIR")
	l.libexec = get("_CCMORPH_LIBEXEC_DIR")
	l.profileDirs = get("_CCMORPH_PROFILE_DIRS")
	l.policy = get("_CCMORPH_CONTEXT_POLICY")
	l.logPath, l.logSet = os.LookupEnv("_CCMORPH_LOG")
	os.Unsetenv("_CCMORPH_LOG")
	l.tuning, l.profileEnv, l.launchEnv = profile.TuningEnv, profile.ProfileEnv, profile.LaunchEnv
	l.backend, l.mapping = profile.BackendEnv, profile.MappingEnv
	l.notice = get("_CCMORPH_NOTICE")
	l.version = get("_CCMORPH_VERSION")
	l.zshVersion = get("_CCMORPH_ZSH_VERSION")
	l.defaultPaths()
	l.launcherPID, _ = strconv.Atoi(get("_CCMORPH_LAUNCHER_PID"))
	if l.launcherPID <= 0 {
		l.launcherPID = os.Getpid()
	}
	return l
}

// handoff is the _CCMORPH_* env fromEnv consumed, for a core child that reads it again.
func (l *launcher) handoff() []string {
	return []string{"_CCMORPH_DIR=" + l.dir, "_CCMORPH_LIB_DIR=" + l.libDir,
		"_CCMORPH_LIBEXEC_DIR=" + l.libexec, "_CCMORPH_PROFILE_DIRS=" + l.profileDirs,
		"_CCMORPH_CONTEXT_POLICY=" + l.policy, "_CCMORPH_LOG=" + l.logPath}
}

// accountHome is the passwd entry's home, for a launch started with HOME unset.
var accountHome = func() string {
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// defaultPaths fills what no shell handed over, the way lib/ccmorph.sh derives it, so
// the core also runs on its own: state from CCMORPH_STATE_DIR or XDG, code beside
// this executable (libexec/ccmorph-core, with lib/ next to libexec/).
func (l *launcher) defaultPaths() {
	home, err := os.UserHomeDir()
	if err != nil { // no HOME: the account's own, never a path relative to the cwd
		if home = accountHome(); home == "" {
			fmt.Fprintln(os.Stderr, "ccmorph: HOME is unset and no home directory is known; set HOME")
			os.Exit(1)
		}
		// Every reader (canonical settings, the projects stores, doctor, rm,
		// setup-profile, settings-compose, Claude Code) reads HOME: give them this one.
		os.Setenv("HOME", home)
	}
	if l.dir == "" {
		if l.dir = os.Getenv("CCMORPH_STATE_DIR"); l.dir == "" {
			cfg := os.Getenv("XDG_CONFIG_HOME")
			if cfg == "" {
				cfg = filepath.Join(home, ".config")
			}
			l.dir = filepath.Join(cfg, "ccmorph")
		}
	}
	if l.libexec == "" {
		if exe, err := os.Executable(); err == nil {
			if r, err := filepath.EvalSymlinks(exe); err == nil {
				exe = r
			}
			l.libexec = filepath.Dir(exe)
		}
	}
	if l.libDir == "" {
		if l.libDir = os.Getenv("CCMORPH_LIB_DIR"); l.libDir == "" {
			l.libDir = filepath.Join(filepath.Dir(l.libexec), "lib")
		}
	}
	if l.profileDirs == "" {
		l.profileDirs = filepath.Join(l.dir, "profiles")
	}
	if l.version == "" { // lib/ccmorph.sh is the one place the version is written
		b, _ := os.ReadFile(filepath.Join(l.libDir, "ccmorph.sh"))
		if _, v, ok := strings.Cut(string(b), "\n_CCMORPH_VERSION=\""); ok {
			l.version, _, _ = strings.Cut(v, "\"")
		}
	}
	if l.policy == "" {
		if l.policy = os.Getenv("CCMORPH_CONTEXT_POLICY"); l.policy == "" {
			l.policy = filepath.Join(home, ".agents/runtime/policies/context.json")
		}
	}
	if !l.logSet {
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		l.logPath = filepath.Join(tmp, fmt.Sprintf("ccmorph-%d.log", os.Getuid()))
	}
}

// theme is _ccm_ui_theme: truecolor roles when stdout is a terminal.
func (l *launcher) theme() {
	if !isTTY(1) || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return
	}
	l.bold, l.dim, l.reset = "\033[1m", "\033[2m", "\033[0m"
	l.accent, l.yellow, l.red, l.cyan = rgb("d77757"), rgb("ffc107"), rgb("ff6b80"), rgb("b1b9f9")
}

// signals is _ccm_launch's traps and _ccm_fork_forward's: INT and QUIT belong to
// Claude Code (it shares the terminal's process group), TERM and HUP are handed to
// it while it runs, and otherwise stop the helpers and end this launch by them.
func (l *launcher) signals(ch chan os.Signal) {
	for s := range ch {
		sig := s.(syscall.Signal)
		if sig == syscall.SIGINT || sig == syscall.SIGQUIT {
			continue
		}
		l.mu.Lock()
		child := l.child
		if child != nil {
			l.forwarded = int(sig)
			l.mu.Unlock()
			child.Signal(sig)
			continue
		}
		l.mu.Unlock()
		l.stopHelpers()
		signal.Reset(sig)
		syscall.Kill(os.Getpid(), sig)
		time.Sleep(time.Second)
		os.Exit(128 + int(sig))
	}
}

func (l *launcher) stopHelpers() {
	l.providerStop()
	l.adapterStop()
}

// core runs a ccmorph-core verb as the zsh did: stdout with trailing newlines
// stripped like $(…), stderr kept for the callers that report it.
func (l *launcher) core(env []string, stdin string, args ...string) (string, string, int) {
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), args...)
	cmd.Env = append(os.Environ(), env...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	rc := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
			if rc < 0 {
				rc = 1
			}
		} else {
			rc = 127
		}
	}
	return strings.TrimRight(out.String(), "\n"), strings.TrimRight(errb.String(), "\n"), rc
}

// try runs a util call whose failure must degrade a launch, never end it: a
// panic is a failed call, logged, as a crashed helper process once was.
func try[T any](l *launcher, what string, f func() T) (v T, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			l.log(fmt.Sprintf("panic in %s: %v\n%s", what, r, debug.Stack()))
			ok = false
		}
	}()
	return f(), true
}

func (l *launcher) err(msg string) {
	fmt.Fprintf(os.Stderr, "%sccmorph:%s %s\n", l.red, l.reset, msg)
	l.log("ERROR " + msg)
}

// log is _ccm_log: best effort, 0600, rotated to its last 500 lines past 100KB.
func (l *launcher) log(msg string) {
	if l.logPath == "" {
		return
	}
	l.logInit()
	f, err := os.OpenFile(l.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return
	}
	fmt.Fprintf(f, "%s [%d] %s\n", time.Now().Format("2006-01-02 15:04:05"), l.launcherPID, msg)
	f.Close()
	if fi, err := os.Stat(l.logPath); err == nil && fi.Mode().IsRegular() && fi.Size() > 102400 {
		data, err := os.ReadFile(l.logPath)
		if err != nil {
			return
		}
		lines := strings.SplitAfter(string(data), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if len(lines) > 500 {
			lines = lines[len(lines)-500:]
		}
		tmp, err := os.CreateTemp(filepath.Dir(l.logPath), filepath.Base(l.logPath)+".")
		if err != nil {
			return
		}
		tmp.WriteString(strings.Join(lines, ""))
		tmp.Close()
		if os.Rename(tmp.Name(), l.logPath) != nil {
			os.Remove(tmp.Name())
		}
	}
}

// logInit creates the log 0600, and tightens an own regular file that is looser.
func (l *launcher) logInit() {
	fi, err := os.Lstat(l.logPath)
	if os.IsNotExist(err) {
		if f, err := os.OpenFile(l.logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600); err == nil {
			f.Close()
		}
		return
	}
	if err == nil && fi.Mode().IsRegular() && owned(fi) && fi.Mode().Perm()&0o077 != 0 {
		os.Chmod(l.logPath, fi.Mode().Perm()&^0o077)
	}
}

func owned(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

func (l *launcher) conf(name string) string { return filepath.Join(l.dir, name+".conf") }

func (l *launcher) exists(name string) bool {
	fi, err := os.Lstat(l.conf(name))
	return err == nil && fi.Mode().IsRegular()
}

// readActive is _ccm_read_active: the default profile, "" when none is set.
func (l *launcher) readActive() (string, bool) {
	f := filepath.Join(l.dir, "active")
	fi, err := os.Lstat(f)
	if err != nil {
		return "", true
	}
	if !fi.Mode().IsRegular() {
		l.err("invalid active-profile file")
		return "", false
	}
	if os.Chmod(f, 0o600) != nil {
		return "", false
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return "", false
	}
	name := strings.TrimRight(string(b), "\n")
	if !profile.ValidName(name) {
		l.err("invalid active profile '" + name + "'")
		return "", false
	}
	return name, true
}

// readRaw is _ccm_read: the profile's fields, or false after the refusal has
// been printed.
func (l *launcher) readRaw(name string) (map[string]string, bool) {
	f, err := l.readProfile(name, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if err != nil {
		l.err(err.Error())
		return nil, false
	}
	return f, true
}

// readQuiet is `_ccm_read 2>/dev/null`.
func (l *launcher) readQuiet(name string) (map[string]string, bool) {
	f, err := l.readProfile(name, func(string) {})
	return f, err == nil
}

func (l *launcher) readProfile(name string, warn func(string)) (map[string]string, error) {
	return profile.Read(l.dir, name, true, profile.Defaults(), warn)
}

// read is _ccm_read + _ccm_validate_loaded.
func (l *launcher) read(name string) (map[string]string, bool) {
	fields, ok := l.readRaw(name)
	if !ok {
		return nil, false
	}
	if err := profile.Validate(name, fields); err != nil {
		l.err(err.Error())
		return nil, false
	}
	return fields, true
}

func (l *launcher) profileLayer(name string) string {
	d := filepath.Join(l.profileDirs, name)
	for _, p := range []string{filepath.Join(d, "settings.profile.json"), filepath.Join(d, "cache", "settings.base.json")} {
		if _, err := os.Lstat(p); err == nil {
			return filepath.Join(d, "settings.profile.json")
		}
	}
	return filepath.Join(d, "settings.json")
}

func (l *launcher) canonical() string {
	return filepath.Join(os.Getenv("HOME"), ".claude", "settings.json")
}

// settingsSources is _ccm_settings_sources: canon, policy when present, profile layer.
func (l *launcher) settingsSources(name string) []string {
	s := []string{l.canonical()}
	if p := filepath.Join(l.profileDirs, name, "settings.policy.json"); fileExists(p) {
		s = append(s, p)
	}
	return append(s, l.profileLayer(name))
}

// withEnv runs f with the KEY=VALUE entries of set in the process environment and
// the names in drop unset, then puts the environment back. For the readers that
// take their state from the environment (the window resolver).
func withEnv(set, drop []string, f func()) {
	type old struct {
		v   string
		had bool
	}
	saved := map[string]old{}
	remember := func(k string) {
		if _, done := saved[k]; !done {
			v, had := os.LookupEnv(k)
			saved[k] = old{v, had}
		}
	}
	for _, k := range drop {
		remember(k)
		os.Unsetenv(k)
	}
	for _, kv := range set {
		k, v, _ := strings.Cut(kv, "=")
		remember(k)
		os.Setenv(k, v)
	}
	defer func() {
		for k, o := range saved {
			if o.had {
				os.Setenv(k, o.v)
			} else {
				os.Unsetenv(k)
			}
		}
	}()
	f()
}

func (l *launcher) storeArgs(verb, name, key string) util.StoreArgs {
	return util.StoreArgs{Op: verb, Conf: l.conf(name), Settings: l.profileLayer(name), Key: key, Shared: l.canonical(), Mapping: l.mapping}
}

// runStore is a store operation: its text without trailing newlines and its status;
// reasons go to errw. A crash is status 2, as a crashed helper process gave.
func (l *launcher) runStore(a util.StoreArgs, errw io.Writer) (string, int) {
	type res struct {
		out string
		rc  int
	}
	r, ok := try(l, "config-store "+a.Op, func() res { out, rc := util.ConfigStore(a, errw); return res{out, rc} })
	if !ok {
		return "", 2
	}
	return strings.TrimRight(r.out, "\n"), r.rc
}

func (l *launcher) configStore(verb, name, key string) (string, int) {
	return l.runStore(l.storeArgs(verb, name, key), os.Stderr)
}

func (l *launcher) configStoreQ(verb, name, key string) (string, int) {
	return l.runStore(l.storeArgs(verb, name, key), io.Discard)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func isDir(p string) bool { fi, err := os.Stat(p); return err == nil && fi.IsDir() }

func isFile(p string) bool { fi, err := os.Stat(p); return err == nil && fi.Mode().IsRegular() }

var digits = regexp.MustCompile(`^[0-9]+$`)

func posInt(s string) (int64, bool) {
	if !digits.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// envGet is the last value of key in an env list, as the zsh loops kept it.
func envGet(env []string, key string) (string, bool) {
	v, ok := "", false
	for _, e := range env {
		if k, val, found := strings.Cut(e, "="); found && k == key {
			v, ok = val, true
		}
	}
	return v, ok
}

func envHas(env []string, key string) bool { _, ok := envGet(env, key); return ok }

func envDrop(env []string, key string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, key+"=") })
}

// claudeHelp is the installed claude's --help, read once for the verb and arity tables.
func (l *launcher) claudeHelp() string {
	if v, ok := l.memo["claude --help"]; ok {
		return v
	}
	help, _ := exec.Command("claude", "--help").Output()
	l.memo["claude --help"] = string(help)
	return string(help)
}

// claudeArity is the option arity table the argv walkers read, from claudeHelp.
func (l *launcher) claudeArity() map[string]string { return util.ClaudeOptArities(l.claudeHelp()) }

func splitNUL(s string) []string {
	s = strings.TrimSuffix(s, ".")
	var out []string
	for strings.Contains(s, "\x00") {
		i := strings.IndexByte(s, 0)
		out = append(out, s[:i])
		s = s[i+1:]
	}
	return out
}

func isTTY(fd int) bool {
	_, err := ioctlTermios(fd)
	return err == nil
}
