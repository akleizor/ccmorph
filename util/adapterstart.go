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

// Refused is the line adapter-start prints on its stdout when the adapter
// itself refused with a complete cause-and-repair line: the launcher adds
// nothing to it, where a bare death or a timeout still gets the route checks
// (#374). The exit status stays 1 either way; only this line says "refused".
const Refused = "REFUSED"

// adapterReadyLimit is the readiness wait adapterStart keeps: the same six
// hundred seconds for either scope. The model-scoped 45 s bound only held on
// a quiet machine — on a loaded host the translator was not ready in it and
// the run died before it started (#766). A caller that must wait longer sets
// CCMORPH_ADAPTER_START_TIMEOUT to a positive number of seconds that fits a
// duration; anything else keeps the default. Parsing and the range check are
// EnvSeconds' (#770).
func adapterReadyLimit() time.Duration {
	if limit, err := EnvSeconds(os.Getenv, "CCMORPH_ADAPTER_START_TIMEOUT", 600); err == nil {
		return limit
	}
	return 600 * time.Second
}

// adapterNow and adapterTicker are the readiness loop's clock and poll, package
// vars so a test can drive the bound in virtual time: a starved host stretches
// the real gap between polls, and only a controlled clock can tell that
// starvation from an ignored bound (#895). Production keeps the real clock and
// a 200 ms poll; a test supplies a clock it sets and a channel it ticks.
var (
	adapterNow    = time.Now
	adapterTicker = func() (<-chan time.Time, func()) {
		tick := time.NewTicker(200 * time.Millisecond)
		return tick.C, tick.Stop
	}
)

// adapterStart is `util adapter-start <core> <profile> <model or ""> <parent pid> <log>`:
// start `<core> adapter` for a launch and wait for its first readiness line. Prints
// PID=<n> and then PORT=<n> or NATIVE=1; exit 1 with the reason on stderr —
// after a Refused line on stdout when the adapter's own refusal is the whole
// message.
//
// stdin is /dev/null, never the tty (the 610137d wedge). stdout goes to a file the
// adapter keeps after this verb exits, so a later line can never hit a closed pipe.
func adapterStart(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 5 {
		return 2
	}
	core, profile, model, parent, logPath := args[0], args[1], args[2], args[3], args[4]
	// The bare id is the one the adapter is asked for and the one the
	// progress line names: a trailing [1m] is ccmorph's own context-window
	// marker, an internal that a person saw in "Starting adapter for
	// 'work (claude-sonnet-5-5[1m])'" and in no other screen (#971).
	bare := strings.SplitN(model, "[", 2)[0]
	argv := []string{"adapter", profile, "--auto"}
	if bare != "" {
		argv = append(argv, "--model", bare)
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
	limit := adapterReadyLimit()
	label := profile
	if bare != "" {
		label += " (" + bare + ")"
	}
	start := adapterNow()
	tick, stopTick := adapterTicker()
	defer stopTick()
	fail := func(msg string) int {
		_ = syscall.Kill(cmd.Process.Pid, syscall.SIGTERM)
		fmt.Fprintln(errw, "ccmorph: "+msg)
		return 1
	}
	refuse := func(msg string) int {
		fmt.Fprintln(out, Refused)
		return fail(msg)
	}
	said := 0
	for {
		line := firstLine()
		switch {
		case strings.HasPrefix(line, "PORT=") || line == "NATIVE=1":
			fmt.Fprintf(out, "PID=%d\n%s\n", cmd.Process.Pid, line)
			return 0
		case strings.HasPrefix(line, "EXIT="):
			return refuse(strings.TrimPrefix(line, "EXIT="))
		}
		select {
		case <-died:
			// The last line may have landed between the read above and the exit.
			if line = firstLine(); strings.HasPrefix(line, "EXIT=") {
				return refuse(strings.TrimPrefix(line, "EXIT="))
			} else if strings.HasPrefix(line, "PORT=") || line == "NATIVE=1" {
				fmt.Fprintf(out, "PID=%d\n%s\n", cmd.Process.Pid, line)
				return 0
			}
			return fail(fmt.Sprintf("'%s': adapter exited before readiness; session not started.", profile))
		case <-tick:
		}
		elapsed := adapterNow().Sub(start)
		waited := int(elapsed / time.Second)
		if elapsed >= limit {
			return fail(fmt.Sprintf("'%s': adapter timed out after %ds; session not started.", profile, waited))
		}
		if waited > said && waited == 1 {
			fmt.Fprintf(errw, "ccmorph: Starting adapter for '%s'…\n", label)
		} else if waited > said && waited%5 == 0 {
			fmt.Fprintf(errw, "ccmorph: Waiting for '%s' adapter (%ds); Ctrl-C to cancel.\n", profile, waited)
		}
		said = waited
	}
}
