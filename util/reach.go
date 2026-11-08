package util

import (
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// The reachability probe's patience, overridable for tests the way the adapter
// deadlines are: a person waiting on a silent gateway reads a progress line
// after one second, and the probe gives up after five — no longer than doctor's
// own patience with the same endpoint.
func reachProgressAfter() time.Duration { return envSecondsF("CCMORPH_REACH_PROGRESS", 1) }
func reachGiveUpAfter() time.Duration   { return envSecondsF("CCMORPH_REACH_BOUND", 5) }

// envSecondsF is the adapter deadlines' envSeconds for a bound that may be
// shorter than a second.
func envSecondsF(name string, def float64) time.Duration {
	if f, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && f > 0 {
		return time.Duration(f * float64(time.Second))
	}
	return time.Duration(def * float64(time.Second))
}

// ReachGiveUpSeconds is the give-up bound in whole seconds, for the progress
// line that tells a person how long the wait can still be.
func ReachGiveUpSeconds() int { return giveUpSeconds() }

// ReachListing is launch's early answer to an unreachable gateway:
// LaunchListing's own request, run with a deadline of its own so a gateway that
// never answers cannot hold a launch silent. ok is false only when nothing
// answered — an HTTP error is an answer, the listing paths name its status —
// and then why names the failure in plain words. progress runs once, when the
// wait has gone on long enough to need saying.
func ReachListing(base, key string, progress func()) (body string, ok bool, why string) {
	bound := reachGiveUpAfter()
	args := []string{"-sS", "-f", "--connect-timeout", "2", "--max-time", secondsArg(bound),
		apiURL(base, "/v1/models?limit=1000"), "-H", "anthropic-version: 2023-06-01"}
	var complaint strings.Builder
	body, err := runCurlKeyed(key, args, &complaint, progress)
	if err == nil {
		return undisguise(strings.TrimRight(body, "\n")), true, ""
	}
	// curl -f reports an HTTP error in its own words; an endpoint that answered
	// is reachable, whatever it said.
	if refused.FindStringSubmatch(complaint.String()) != nil {
		return "", true, ""
	}
	// The probe speaks only for the four transport failures the issue names;
	// anything else is left to the launch path's own words, which name the route
	// and point at doctor (#286), so a cause this probe cannot place keeps its
	// established sentence.
	if exit := exitOf(err); !namesTheCause(exit) {
		return "", true, ""
	}
	return "", false, WhyUnreachable(base, exitOf(err), complaint.String(), giveUpSeconds())
}

// namesTheCause is whether curl's exit status is one of the four failures the
// probe can name: the name does not exist, the connection was refused (or has no
// route), the secure connection failed, or nothing answered.
func namesTheCause(exit int) bool {
	switch exit {
	case 6, 7, 28, 35, 60:
		return true
	}
	return exit < 0 // killed at the bound: no answer
}

// runCurlKeyed is curlKeyed with a watch on the wait: progress says so once the
// request has run long enough, and a request still running at the bound is
// killed and read as no answer.
func runCurlKeyed(key string, args []string, errw io.Writer, progress func()) (string, error) {
	cmd := exec.Command("curl", append([]string{"-K", "-"}, args...)...)
	cmd.Stdin = strings.NewReader(credConfig(key))
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, errw
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(reachProgressAfter()):
		if progress != nil {
			progress()
		}
		select {
		case err := <-done:
			return out.String(), err
		case <-time.After(reachGiveUpAfter() - reachProgressAfter()):
			cmd.Process.Kill()
			return out.String(), <-done
		}
	}
}

// giveUpSeconds is the give-up bound in whole seconds, never less than one: the
// number a person reads in the no-answer sentence.
func giveUpSeconds() int {
	return max(int(math.Ceil(reachGiveUpAfter().Seconds())), 1)
}

// secondsArg is a duration as curl's --max-time words it: "5", or "0.5" when
// the bound is shorter than a second.
func secondsArg(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}
