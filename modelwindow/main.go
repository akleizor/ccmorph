package modelwindow

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/pycompat"
)

const usageText = "usage: model-window <profile> <model> | --context <profile> <model> [tokens|auto] | --migrate"

// Main runs `model-window <args>` (args exclude the program name) and returns its exit code.
func Main(args []string, stdout, stderr io.Writer) (code int) {
	if e := pycompat.Catch(func() { code = run(args, stdout, stderr) }); e != nil {
		if e.Kind == pycompat.Other {
			fmt.Fprintf(stderr, "model-window: %s: %s\n", e.Class, e.Msg)
		} else {
			fmt.Fprintf(stderr, "model-window: %s\n", e.Msg)
		}
		return 1
	}
	return code
}

func run(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 1 && args[0] == "--recover-cache-backups":
		return RecoverCacheBackups(stderr)
	case len(args) == 2 && args[0] == "--migrate" && args[1] == "--check":
		fmt.Fprintln(stdout, MigrationPreview())
		return 0
	case len(args) == 1 && args[0] == "--migrate":
		type result struct {
			code int
			exc  *pycompat.Exc
		}
		done := make(chan result, 1)
		go func() {
			var r result
			r.exc = pycompat.Catch(func() { r.code = Migrate(stderr) })
			done <- r
		}()
		select {
		case r := <-done:
			if r.exc != nil {
				panic(r.exc)
			}
			return r.code
		case <-time.After(30 * time.Second):
			pycompat.RaiseOSMsg("context migration exceeded 30 seconds; retry to finish from retained backups")
		}
	case len(args) >= 3 && args[0] == "--locked":
		return runLocked(args, stderr)
	case (len(args) == 3 || len(args) == 4) && args[0] == "--context":
		profile, model := args[1], args[2]
		if len(args) == 3 {
			fmt.Fprintln(stdout, ContextValue(profile, model))
			return 0
		}
		SetContext(profile, model, args[3])
		what := "set to " + args[3] + " tokens"
		if args[3] == "auto" {
			what = "set to automatic"
		}
		fmt.Fprintf(stdout, "%s: %s context %s (new sessions)\n", profile, model, what)
		if v, ok := Positive(args[3]); ok && !(v >= 100000 && v <= oneM) {
			fmt.Fprintln(stderr, "Limit saved. Claude Code's autoCompactWindow setting supports 100,000–1,000,000 tokens; this value is outside that range.")
		}
		return 0
	case len(args) == 2 && args[0] == "--ceiling":
		var best int64
		found := false
		for model, v := range ProfileWindows(args[1]) {
			if !strings.HasPrefix(model, "claude-") && (!found || v > best) {
				best, found = v, true
			}
		}
		if found {
			fmt.Fprintln(stdout, best)
		}
		return 0
	case len(args) == 3 && args[0] == "--fields":
		fmt.Fprintln(stdout, strings.Join(Resolve(args[1], args[2]).Fields(), "\t"))
		return 0
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, usageText)
		return 2
	}
	rec := Resolve(args[0], args[1])
	if rec.Unprobed {
		fmt.Fprintf(stderr, "model-window: no known context limit for %s on %s; using %s-token fallback. Run: ccmorph probe %s\n",
			args[1], args[0], commas(CCDefaultWindow), args[0])
	}
	fmt.Fprintln(stdout, rec.JSON())
	return 0
}

func commas(n int64) string {
	s := itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// runLocked holds every named profile's lock while it runs a command, which
// gets CCMORPH_PROFILE_LOCK_HELD so its own writers do not deadlock.
func runLocked(args []string, stderr io.Writer) int {
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
		path := ProfilePath(profile)
		if !contains(strings.Split(os.Getenv("CCMORPH_PROFILE_LOCK_HELD"), ":"), path) {
			f, err := os.OpenFile(path+".lock", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
			if err != nil {
				pycompat.RaiseOS(err)
			}
			locks = append(locks, f)
			if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
				pycompat.RaiseOS(err)
			}
		}
	}
	for _, profile := range profiles {
		held = append(held, ProfilePath(profile))
	}
	cmd := exec.Command(args[2], args[3:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "CCMORPH_PROFILE_LOCK_HELD="+strings.Join(held, ":"))
	if _, err := exec.LookPath(args[2]); err != nil {
		pycompat.RaiseOSMsg("[Errno 2] No such file or directory: '%s'", args[2])
	}
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		pycompat.RaiseOS(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		select { // a SIGINT sent just before the child exited still counts as the interrupt
		case <-interrupts:
			fmt.Fprintln(stderr, "Cancelled.")
			return 130
		case <-time.After(50 * time.Millisecond):
		}
		if ee, ok := err.(*exec.ExitError); ok {
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				return 256 - int(ws.Signal()) // Python's returncode is -signal, which exit() wraps
			}
			return ee.ExitCode()
		}
		return 0
	case <-interrupts:
		// KeyboardInterrupt: stop the child, let the locks go, report once.
		cmd.Process.Kill()
		<-exited
		fmt.Fprintln(stderr, "Cancelled.")
		return 130
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
