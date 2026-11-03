package ctxwindow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const windowUsage = "usage: model-window <profile> <model> | --context <profile> <model> [tokens|auto] | --migrate"

// WindowMain runs `model-window <args>` (args exclude the program name) and returns its exit code.
func WindowMain(args []string, stdout, stderr io.Writer) int {
	code, err := windowRun(args, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "model-window: %s\n", err)
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
			value, err := ContextValue(profile, model)
			if err != nil {
				return 0, err
			}
			fmt.Fprintln(stdout, value)
			return 0, nil
		}
		if err := SetContext(profile, model, args[3]); err != nil {
			return 0, err
		}
		what := "set to " + args[3] + " tokens"
		if args[3] == "auto" {
			what = "set to automatic"
		}
		fmt.Fprintf(stdout, "%s: %s context %s (new sessions)\n", profile, model, what)
		if v, ok := Positive(args[3]); ok && !(v >= 100000 && v <= oneM) {
			fmt.Fprintln(stderr, "Limit saved. Claude Code's autoCompactWindow setting supports 100,000–1,000,000 tokens; this value is outside that range.")
		}
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
		if !contains(strings.Split(os.Getenv("CCMORPH_PROFILE_LOCK_HELD"), ":"), path) {
			lock, err := lockPath(path)
			if err != nil {
				return 0, err
			}
			f, err := os.OpenFile(lock, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return 0, err
			}
			locks = append(locks, f)
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				return 0, err
			}
		}
		held = append(held, path)
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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
