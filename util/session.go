package util

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// maskedKeys are ccmorph's own knobs: they configure the shim, never Claude Code.
var maskedKeys = []string{"CCMORPH_RANDOM_DEVICE_ID", "CCMORPH_AFFINITY_HEADER", "CCMORPH_USER_AGENT",
	"CCMORPH_QUIRKS", "CCMORPH_STALL_TIMEOUT", "CCMORPH_UPSTREAM_PROXY", "CCMORPH_SHIM_HOOKS",
	"CCMORPH_MASK", "CCMORPH_MASK_COMMANDS"}

// privateSkip stays out of a private view: the session's own transcripts and state.
var privateSkip = []string{"projects", "sessions", "session-env", "file-history", "history.jsonl",
	"shell-snapshots", "paste-cache", "debug", "todos", "tasks", "plans", "backups"}

// privateOwner is the file inside a private view's root naming the claude pid
// that runs from the view: the stale sweep reaps the view only once that
// process is gone, however long the session ran (#166).
const privateOwner = "ccmorph-owner"

// claudeSession is `util claude-session <tuning> <profile keys> <launch keys>
// <backend keys> <launch env> -- <claude args...>`, each key list space-separated.
// The launch's own KEY=value entries come from <launch env>: a 0600 file this
// process removes before anything else, or — from a launcher of v0.4.4 or earlier
// still running after an update (#137) — inline, one per operand before `--`.
// A private session keeps this process as claude's parent, and with the file form
// its argument vector stays in ps for the whole run (#112). It clears every key a
// profile could declare or a launch sets (tuning keys pass through from the shell
// unless an outer session recorded them in CCMORPH_SET_ENV), exports this launch's
// env, and execs claude. With CCMORPH_PRIVATE=1 claude runs from a throwaway view
// of the profile instead, removed when it exits. CCMORPH_NOTICE, when set, is
// printed just before. _CCMORPH_RUN_DIR, when set, is the launch's run/ root for
// that view's fallback when TMPDIR cannot be written (#140); it is unset before
// claude starts, so neither claude nor its children inherit it.
func claudeSession(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) < 5 {
		return 2
	}
	sep := slices.Index(args[4:], "--")
	if sep < 0 {
		return 2
	}
	sep += 4
	var entries []string
	if sep == 5 && !envAssignment(args[4]) {
		var err error
		if entries, err = readSessionEnv(args[4]); err != nil {
			fmt.Fprintln(errw, "ccmorph: session env: "+err.Error())
			return 1
		}
	} else {
		entries = args[4:sep] // #137: a v0.4.4-or-earlier launcher sends them inline
	}
	tuning, profileKeys := strings.Fields(args[0]), strings.Fields(args[1])
	clear := append(append([]string{"CLAUDE_CODE_EFFORT_LEVEL", "CLAUDE_CONFIG_DIR",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_SUBAGENT_MODEL"}, maskedKeys...),
		append(strings.Fields(args[2]), strings.Fields(args[3])...)...)
	for _, k := range strings.Fields(os.Getenv("CCMORPH_SET_ENV")) {
		if slices.Contains(tuning, k) {
			clear = append(clear, k)
		}
	}
	for _, k := range profileKeys {
		if !slices.Contains(tuning, k) {
			clear = append(clear, k)
		}
	}
	clear = append(clear, "CCMORPH_SET_ENV")
	// `claude --bare` sets CLAUDE_CODE_SIMPLE in its own children; a nested launch
	// must not inherit that session's value.
	if os.Getenv("CLAUDE_CODE_CHILD_SESSION") == "1" {
		clear = append(clear, "CLAUDE_CODE_SIMPLE")
	}
	for _, k := range clear {
		os.Unsetenv(k)
	}
	notice := os.Getenv("CCMORPH_NOTICE")
	os.Unsetenv("CCMORPH_NOTICE")
	runDir := os.Getenv("_CCMORPH_RUN_DIR")
	os.Unsetenv("_CCMORPH_RUN_DIR")

	rest, private := args[sep+1:], false
	var setEnv []string
	for _, kv := range entries {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case slices.Contains(maskedKeys, k):
		case kv == "CCMORPH_PRIVATE=1":
			private = true
		case k == "CCMORPH_SET_ENV":
			setEnv = append(setEnv, strings.Fields(v)...)
		case !ok || k == "" || strings.ContainsAny(k, "= \t\n"):
			return 1
		default:
			os.Setenv(k, v)
			if slices.Contains(tuning, k) {
				setEnv = append(setEnv, k)
			}
		}
	}
	if len(setEnv) > 0 {
		os.Setenv("CCMORPH_SET_ENV", strings.Join(setEnv, " "))
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(errw, "ccmorph: claude: command not found")
		return 127
	}
	say := func() {
		if notice != "" {
			// A stdout with no reader must cost only the notice: caught, SIGPIPE
			// spares this process, and exec restores the default for claude.
			signal.Notify(make(chan os.Signal, 1), syscall.SIGPIPE)
			fmt.Fprintln(out, notice)
		}
	}
	if !private || !isDir(os.Getenv("CLAUDE_CONFIG_DIR")) {
		say()
		return execErr(syscall.Exec(bin, append([]string{"claude"}, rest...), os.Environ()), errw)
	}
	return privateSession(bin, rest, runDir, say, errw)
}

// envName is the shape of an environment variable name, the only key an inline
// KEY=value entry can have. A session env file's path never matches: it holds a
// slash before any "=", so it is never mistaken for an entry (#137).
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// envAssignment reports whether s is an inline KEY=value entry rather than the
// path of the launch environment file.
func envAssignment(s string) bool {
	k, _, ok := strings.Cut(s, "=")
	return ok && envName.MatchString(k)
}

// readSessionEnv takes the launch environment from the 0600 file the launcher
// wrote and removes it at once: the file holds credentials, and its path is the
// only thing allowed on the argument vector (#112). Entries are NUL-separated
// because a value can span lines (ANTHROPIC_CUSTOM_HEADERS).
func readSessionEnv(path string) ([]string, error) {
	defer os.Remove(path)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is not a private regular file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var env []string
	for _, e := range strings.Split(string(b), "\x00") {
		if e != "" {
			env = append(env, e)
		}
	}
	return env, nil
}

func execErr(err error, errw io.Writer) int {
	fmt.Fprintln(errw, "ccmorph: claude:", err)
	return 126
}

// privateSession runs claude from a throwaway view of the profile: config and
// settings linked in, transcripts and history landing in the view and deleted
// when claude exits. The view sits under TMPDIR, or under runDir (the launch's
// state dir's run/, 0700) when TMPDIR cannot be written — the same fallback root
// the launch env file uses (#140). A launcher that sends no runDir leaves the
// state dir to be derived here. The view's root records claude's pid for gc-stale
// (#166): the defer below dies with a SIGKILL, and a view whose claude still runs
// must survive the sweep. TERM and HUP are forwarded to claude; INT and QUIT are
// claude's own (Ctrl-C keeps the session), so this process ignores them.
func privateSession(bin string, args []string, runDir string, say func(), errw io.Writer) int {
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	real := os.Getenv("CLAUDE_CONFIG_DIR")
	tmp, err := os.MkdirTemp(os.Getenv("TMPDIR"), "ccmorph-private.")
	if err != nil {
		// runDir is the root the launch env file already fell back to (#112);
		// without it a private session cannot start (#140).
		run := runDir
		if run == "" {
			run = filepath.Join(ctxwindow.StateDir(), "run")
		}
		if mkErr := os.MkdirAll(run, 0o700); mkErr != nil {
			fmt.Fprintln(errw, "ccmorph: private view:", err)
			return 1
		}
		if tmp, err = os.MkdirTemp(run, "ccmorph-private."); err != nil {
			fmt.Fprintln(errw, "ccmorph: private view:", err)
			return 1
		}
	}
	defer os.RemoveAll(tmp)
	view := filepath.Join(tmp, filepath.Base(real))
	if os.Mkdir(view, 0o700) != nil {
		return 1
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		fmt.Fprintln(errw, "ccmorph: private view:", err)
		return 1
	}
	for _, e := range entries {
		if !slices.Contains(privateSkip, e.Name()) {
			if os.Symlink(filepath.Join(real, e.Name()), filepath.Join(view, e.Name())) != nil {
				return 1
			}
		}
	}
	// Claude Code keys the keychain item by this, else by CLAUDE_CONFIG_DIR: pin it
	// to the real profile so the view reads the profile's own sign-in.
	os.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", real)
	os.Setenv("CLAUDE_CONFIG_DIR", view)
	select {
	case s := <-sigs:
		if s == syscall.SIGTERM || s == syscall.SIGHUP {
			return 128 + int(s.(syscall.Signal))
		}
	default:
	}
	say()
	cmd := exec.Command(bin, args...)
	cmd.Args[0] = "claude"
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return execErr(err, errw)
	}
	// The sweep follows claude's pid, never this helper's: a SIGKILL can take
	// this process while claude lives on, and the view must follow claude. A
	// record that never lands — the write failed, or this process died in the
	// moment — puts the view out of the sweep's reach for good: with no
	// record, age cannot tell a live session from a dead one, so the sweep
	// takes no view on age alone.
	_ = os.WriteFile(filepath.Join(tmp, privateOwner), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				if s == syscall.SIGTERM || s == syscall.SIGHUP {
					_ = cmd.Process.Signal(s)
				}
			case <-done:
				return
			}
		}
	}()
	err = cmd.Wait()
	close(done)
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if err != nil && cmd.ProcessState == nil {
		return 1
	}
	return cmd.ProcessState.ExitCode()
}
