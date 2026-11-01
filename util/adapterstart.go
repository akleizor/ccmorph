package util

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// adapterStart is `util adapter-start <core> <profile> <model or ""> <parent pid> <log>`:
// start `<core> adapter` for a launch and wait for its first readiness line. Prints
// PID=<n> and then PORT=<n> or NATIVE=1; exit 1 with the reason on stderr.
//
// stdin is /dev/null, never the tty (the 610137d wedge). stdout goes to a file the
// adapter keeps after this verb exits, so a later line can never hit a closed pipe.
func adapterStart(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 5 {
		return 2
	}
	core, profile, model, parent, logPath := args[0], args[1], args[2], args[3], args[4]
	argv := []string{"adapter", profile, "--auto"}
	if model != "" {
		argv = append(argv, "--model", strings.SplitN(model, "[", 2)[0])
	}
	argv = append(argv, "--parent-pid", parent)
	logf, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		logf, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	defer logf.Close()
	ready, err := os.CreateTemp(os.Getenv("TMPDIR"), "ccmorph-adapter-ready.")
	if err != nil {
		return 1
	}
	defer os.Remove(ready.Name())
	defer ready.Close()
	cmd := exec.Command(core, argv...)
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, ready, logf
	if cmd.Start() != nil {
		fmt.Fprintf(errw, "ccmorph: '%s': adapter exited before readiness; session not started.\n", profile)
		return 1
	}
	died := make(chan struct{})
	go func() { _ = cmd.Wait(); close(died) }()
	firstLine := func() string {
		b, _ := os.ReadFile(ready.Name())
		line, _, _ := strings.Cut(string(b), "\n")
		return line
	}
	// A selected launch has a bounded compatibility check, never a fleet sweep.
	limit := 600 * time.Second
	if model != "" {
		limit = 45 * time.Second
	}
	label := profile
	if model != "" {
		label += " (" + model + ")"
	}
	start := time.Now()
	// The readiness poll starts fast and backs off to 200 ms: a warm or stubbed
	// adapter is ready in milliseconds, and a fixed 200 ms first tick made every
	// launch wait one out.
	delay := 10 * time.Millisecond
	fail := func(msg string) int {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		fmt.Fprintln(errw, "ccmorph: "+msg)
		return 1
	}
	said := 0
	for {
		line := firstLine()
		switch {
		case strings.HasPrefix(line, "PORT=") || line == "NATIVE=1":
			fmt.Fprintf(out, "PID=%d\n%s\n", cmd.Process.Pid, line)
			return 0
		case strings.HasPrefix(line, "EXIT="):
			return fail(strings.TrimPrefix(line, "EXIT="))
		}
		select {
		case <-died:
			// The last line may have landed between the read above and the exit.
			if line = firstLine(); strings.HasPrefix(line, "EXIT=") {
				return fail(strings.TrimPrefix(line, "EXIT="))
			} else if strings.HasPrefix(line, "PORT=") || line == "NATIVE=1" {
				fmt.Fprintf(out, "PID=%d\n%s\n", cmd.Process.Pid, line)
				return 0
			}
			return fail(fmt.Sprintf("'%s': adapter exited before readiness; session not started.", profile))
		case <-time.After(delay):
			if delay < 200*time.Millisecond {
				delay *= 2
			}
		}
		waited := int(time.Since(start) / time.Second)
		if time.Since(start) >= limit {
			return fail(fmt.Sprintf("'%s': adapter timed out after %ds; session not started.", profile, int(limit.Seconds())))
		}
		if waited > said && waited == 1 {
			fmt.Fprintf(errw, "ccmorph: Starting adapter for '%s'…\n", label)
		} else if waited > said && waited%5 == 0 {
			fmt.Fprintf(errw, "ccmorph: Waiting for '%s' adapter (%ds); Ctrl-C to cancel.\n", profile, waited)
		}
		said = waited
	}
}
