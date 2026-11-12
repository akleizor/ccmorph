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
	liveProfile                                        string // whose live lock this process holds (#205)

	red, yellow, bold, dim, accent, cyan, reset string

	memo                map[string]string
	storeReads          map[string]*storeMemo // one profile's store, parsed once per command (#478)
	version, zshVersion string
	migrated            bool

	adapterPID              int
	adapterProfile          string
	adapterPort             string
	providerPID, providerTD string
	providerAuth            string
	providerHolder          string
	providerBridge          *util.SharedBridge
	providerActive          string
	providerPort            string
	providerKey             string
	usageHook               string

	mu        sync.Mutex
	child     *os.Process
	forwarded int
	notice    string
	chosen    map[string]bool
	ranClaude bool   // claude started on the pass that just ran (else it never began)
	launched  string // the profile the pass that just ran resolved to, "" for none
	ended     string // the id of the session the exited claude recorded (#285)
	// where Claude Code told its SessionEnd hook that session's transcript is;
	// "" when it said nothing (#359)
	endedTranscript string

	// relayCrossed marks the pass that follows a hop onto another profile's
	// config: only there can first-run questions meet a person mid-session, so
	// only there does the launch say which answer is safe (#258).
	relayCrossed bool
}

// relayNext is what one launch hands the driver when Claude Code exited for a hop.
// fresh marks a hop with no conversation to carry: the target starts a new one.
type relayNext struct {
	args             []string
	profile, session string
	crossed, fresh   bool
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
	// The relay pass's origin: which profile the conversation came from, and the
	// session a failed switch must point back at.
	from, relaySession := "", ""
	for {
		l.ranClaude = false
		next, rc := l.launch(profileName, interactive, native, args)
		l.notice = ""
		if next == nil {
			// A relay pass that failed before claude began leaves the person at
			// the shell with the conversation on the profile it came from (#286).
			// Claude Code prints its own way back when it ran at all, so this is
			// only for the switch that never started.
			if from != "" && relaySession != "" && rc != 0 && !l.ranClaude {
				l.err("the switch to '" + profileName + "' did not finish; the conversation is still on '" + from + "'.")
				fmt.Fprintf(os.Stderr, "  Resume it there: ccmorph run %s --resume %s\n", from, relaySession)
			}
			return rc
		}
		from, relaySession = l.launched, next.session
		args, profileName, interactive, native = next.args, next.profile, false, true
		l.relayCrossed = next.crossed
		if next.fresh {
			// No conversation to carry: one line says so, and the cache notice a
			// resume owes stays unsaid (#288).
			// If this pass cannot start either, there is no conversation to point
			// back at: --resume on the empty id would end at Claude Code's own
			// "No conversation found".
			relaySession = ""
			l.log("relay fresh profile=" + next.profile + " session=" + next.session)
			fmt.Printf("\n%sccmorph:%s starting a new conversation as %s%s%s...\n\n", l.accent, l.reset, l.bold, next.profile, l.reset)
			continue
		}
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
	// A core child can rewrite the store's files (a settings fold, a context
	// migration, a probe's discoveries), so the memoized reads read them again (#478).
	l.storeReads = nil
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

// exists reports whether name is a profile spelled exactly as stored. On a
// volume that ignores letter case com/Tok.conf opens tok.conf, so the conf is
// only this profile's when a directory entry names it as typed.
func (l *launcher) exists(name string) bool {
	fi, err := os.Lstat(l.conf(name))
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.Name() == name+".conf" && e.Type().IsRegular() {
			return true
		}
	}
	return false
}

// caseTwin is the profile whose name differs from name only by letter case, or
// "" when there is none. An exact match always wins.
func (l *launcher) caseTwin(name string) string {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".conf")
		if !ok || stem == name || !strings.EqualFold(stem, name) {
			continue
		}
		if e.Type().IsRegular() && profile.ValidName(stem) {
			return stem
		}
	}
	return ""
}

// readActiveFile is the one read of the active file: the name a regular,
// un-symlinked active file spells — trimmed, an older ccmorph's valid wrong-case
// spelling rescued to the profile that is really there, "" when the file is
// empty — and whether such a file answered at all: missing, a symlink, a
// directory and an unreadable file all answer no. What the name is worth is
// each reader's own judgment: readActive refuses the invalid spellings and
// re-tightens the file to 0600; doctor reads an empty default as no profile
// selected and never writes.
func (l *launcher) readActiveFile() (string, bool) {
	f := filepath.Join(l.dir, "active")
	if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return "", false
	}
	name := strings.TrimRight(string(b), "\n")
	// An older ccmorph wrote the spelling the person typed; read it as the
	// profile that is really there. The file is only rewritten by use or mv.
	// Only a spelling that is itself a name is rescued: case folding maps
	// invalid ones (the Kelvin sign, U+212A, folds to k) onto valid names, and
	// each reader refuses an invalid spelling as written.
	if profile.ValidName(name) && !l.exists(name) {
		if twin := l.caseTwin(name); twin != "" {
			name = twin
		}
	}
	return name, true
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
	name, read := l.readActiveFile()
	if !read {
		return "", false
	}
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
// reasons go to errw. A crash is status 2, as a crashed helper process gave. A save
// drops every memoized read — what it wrote is not what they saw (#478).
func (l *launcher) runStore(a util.StoreArgs, errw io.Writer) (string, int) {
	type res struct {
		out string
		rc  int
	}
	r, ok := try(l, "config-store "+a.Op, func() res { out, rc := util.ConfigStore(a, errw); return res{out, rc} })
	if storeSaves(a.Op) {
		l.storeReads = nil
	}
	if !ok {
		return "", 2
	}
	return strings.TrimRight(r.out, "\n"), r.rc
}

// storeSaves names the ops that write the store's files: set, configure,
// seed-model-defaults, prepare-launch and migrate-native (util/store.go).
func storeSaves(op string) bool {
	return slices.Contains([]string{"set", "configure", "seed-model-defaults", "prepare-launch", "migrate-native"}, op)
}

// storeMemo is one profile's store as this command read it: every read answer,
// or the refusal a broken profile gave, held until a write drops it. The verbs
// stack three to six reads of one profile; all but the first are answered here (#478).
type storeMemo struct {
	read *util.StoreRead // nil when the read was refused
	why  string          // the refusal, as the store words it
}

// storeRead is the read half of the store, memoized per profile: one read and
// parse of the profile's files serves every get, model-settings and snapshot a
// command asks for. A refusal is kept too — a broken profile refuses every read
// the same way — and replayed worded as the store words it. A panic is not
// kept; the next read tries again, as each store call once did.
func (l *launcher) storeRead(name string, errw io.Writer) (util.StoreRead, int) {
	if m, ok := l.storeReads[name]; ok {
		if m.read == nil {
			fmt.Fprint(errw, m.why)
			return util.StoreRead{}, 1
		}
		return *m.read, 0
	}
	type answer struct {
		r   util.StoreRead
		err error
	}
	a, ok := try(l, "config-store read", func() answer { r, err := util.ConfigStoreRead(l.storeArgs("", name, "")); return answer{r, err} })
	if !ok {
		return util.StoreRead{}, 2
	}
	m := &storeMemo{}
	if a.err != nil {
		m.why = fmt.Sprintf("ccmorph: could not save/read configuration: %v\n", a.err)
	} else {
		m.read = &a.r
	}
	if l.storeReads == nil {
		l.storeReads = map[string]*storeMemo{}
	}
	l.storeReads[name] = m
	if m.read == nil {
		fmt.Fprint(errw, m.why)
		return util.StoreRead{}, 1
	}
	return a.r, 0
}

// storeCall is the one way the cli reaches the store: a read op is answered from
// the per-profile memo, anything else runs the store — a save, which drops the
// memo so the next read sees what it wrote (#478).
func (l *launcher) storeCall(verb, name, key string, errw io.Writer) (string, int) {
	switch verb {
	case "get", "model-settings", "snapshot":
		r, rc := l.storeRead(name, errw)
		if rc != 0 {
			return "", rc
		}
		switch verb {
		case "get":
			return r.Values[key], 0
		case "model-settings":
			return r.Projection, 0
		}
		return r.Snapshot, 0
	}
	return l.runStore(l.storeArgs(verb, name, key), errw)
}

func (l *launcher) configStore(verb, name, key string) (string, int) {
	return l.storeCall(verb, name, key, os.Stderr)
}

func (l *launcher) configStoreQ(verb, name, key string) (string, int) {
	return l.storeCall(verb, name, key, io.Discard)
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

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

func isTTY(fd int) bool {
	_, err := ioctlTermios(fd)
	return err == nil
}
