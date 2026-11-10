package ctxwindow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const windowUsage = "usage: model-window <profile> <model> | --context <profile> <model> [tokens|auto] | --migrate"

// WindowMain runs `model-window <args>` (args exclude the program name) and returns its exit code.
func WindowMain(args []string, stdout, stderr io.Writer) int {
	code, err := windowRun(args, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ccmorph: %s\n", err)
		return 1
	}
	return code
}

func windowRun(args []string, stdout, stderr io.Writer) (int, error) {
	switch {
	case len(args) == 1 && args[0] == "--recover-cache-backups":
		return RecoverCacheBackups(stderr)
	case len(args) == 2 && args[0] == "--migrate" && args[1] == "--check":
		preview, err := MigrationPreview()
		if err != nil {
			return 0, err
		}
		fmt.Fprintln(stdout, preview)
		return 0, nil
	case len(args) == 1 && args[0] == "--migrate":
		type result struct {
			code int
			err  error
		}
		done := make(chan result, 1)
		go func() {
			code, err := Migrate(stderr)
			done <- result{code, err}
		}()
		select {
		case r := <-done:
			return r.code, r.err
		case <-time.After(30 * time.Second):
			return 0, errors.New("context migration exceeded 30 seconds; retry to finish from retained backups")
		}
	case len(args) >= 3 && args[0] == "--locked":
		return runLocked(args, stderr)
	case (len(args) == 3 || len(args) == 4) && args[0] == "--context":
		profile, model := args[1], args[2]
		if len(args) == 3 {
			// The read answers the record the session will use, not the pin
			// alone (#372): the resolver's window with its source, so the
			// caller can name where the count came from. The pin's own
			// validation surface stays.
			if !ValidModel(model) {
				return 0, errors.New("context model id is invalid")
			}
			path, err := ProfilePath(profile)
			if err != nil {
				return 0, err
			}
			if _, _, err := ReadConf(path); err != nil {
				return 0, err
			}
			fmt.Fprintln(stdout, Resolve(profile, model).JSON())
			return 0, nil
		}
		pinned, numeric := ParseTokens(args[3])
		if err := SetContext(profile, model, args[3]); err != nil {
			return 0, err
		}
		what := "set to automatic"
		if numeric {
			what = "set to " + strconv.FormatInt(pinned, 10) + " tokens"
		}
		fmt.Fprintf(stdout, "%s: %s context %s (new sessions)\n", profile, model, what)
		if numeric && !(pinned >= 100000 && pinned <= oneM) {
			fmt.Fprintln(stderr, "Limit saved. New sessions use it: the model's auto-compact window is sized from it, clamped into the 100,000–1,000,000 tokens Claude Code supports, and the session's context cap is the route's largest window, this limit included — set a value in that range, or auto, for a limit that is applied without clamping.")
		}
		// A model the route's listing does not carry is the cli's to name, from
		// that listing — the one `ccmorph models` reads (#370); the discovered
		// table this helper holds says nothing about what a route lists.
		return 0, nil
	case len(args) == 2 && args[0] == "--ceiling":
		var best int64
		found := false
		windows, err := ProfileWindows(args[1])
		if err != nil {
			return 0, err
		}
		for model, v := range windows {
			if !strings.HasPrefix(model, "claude-") && (!found || v > best) {
				best, found = v, true
			}
		}
		if found {
			fmt.Fprintln(stdout, best)
		}
		return 0, nil
	case len(args) == 3 && args[0] == "--fields":
		fmt.Fprintln(stdout, strings.Join(Resolve(args[1], args[2]).Fields(), "\t"))
		return 0, nil
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, windowUsage)
		return 2, nil
	}
	rec := Resolve(args[0], args[1])
	if rec.Unprobed {
		fmt.Fprintf(stderr, "model-window: no known context limit for %s on %s; using %s-token fallback. Run: ccmorph probe %s\n",
			args[1], args[0], commas(CCDefaultWindow), args[0])
	}
	fmt.Fprintln(stdout, rec.JSON())
	return 0, nil
}

// runLocked holds every named profile's lock while it runs a command, which
// gets CCMORPH_PROFILE_LOCK_HELD so its own writers do not deadlock.
func runLocked(args []string, stderr io.Writer) (int, error) {
	set := map[string]bool{}
	for _, p := range strings.Split(args[1], ",") {
		set[p] = true
	}
	var profiles []string
	for p := range set {
		profiles = append(profiles, p)
	}
	sort.Strings(profiles)
	var locks []*os.File
	var taken []os.FileInfo
	defer func() {
		for _, f := range locks {
			f.Close()
		}
	}()
	var held []string
	for _, profile := range profiles {
		path, err := ProfilePath(profile)
		if err != nil {
			return 0, err
		}
		held = append(held, path)
		if heldLock(path) {
			continue
		}
		lock, err := lockPath(path)
		if err != nil {
			return 0, err
		}
		f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		locks = append(locks, f)
		fi, err := f.Stat()
		if err != nil {
			return 0, err
		}
		again := false
		for _, prev := range taken {
			if os.SameFile(fi, prev) {
				again = true
				break
			}
		}
		if again { // one lock file reached under two names: already held
			continue
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
			return 0, err
		}
		taken = append(taken, fi)
	}
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CCMORPH_PROFILE_LOCK_HELD="+strings.Join(held, ":"))
	if _, err := exec.LookPath(args[2]); err != nil {
		return 0, fmt.Errorf("[Errno 2] No such file or directory: '%s'", args[2])
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		select { // a SIGINT sent just before the child exited still counts as the interrupt
		case <-interrupts:
			fmt.Fprintln(stderr, "Cancelled.")
			return 130, nil
		case <-time.After(50 * time.Millisecond):
		}
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 256 - int(ws.Signal()), nil // Python's returncode is -signal, which exit() wraps
			}
			return ee.ExitCode(), nil
		}
		return 0, nil
	case <-interrupts:
		// KeyboardInterrupt: stop the child, let the locks go, report once.
		cmd.Process.Kill()
		<-exited
		fmt.Fprintln(stderr, "Cancelled.")
		return 130, nil
	}
}
