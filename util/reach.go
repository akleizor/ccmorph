package util

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/netroute"
	"github.com/akleizor/ccmorph/profile"
)

// PrivateNetworkAdvice rides only on the no-answer sentence: a gateway that
// never answers is the one cause a private network the person has not joined
// explains (#260).
const PrivateNetworkAdvice = "(if it is an internal endpoint, connect your private network and try again)"

var (
	afterMillis = regexp.MustCompile(`after ([0-9]+) ?(?:ms|milliseconds)`)
	saysNoRoute = regexp.MustCompile(`(?i)no route to host|network is unreachable`)
	saysRefused = regexp.MustCompile(`(?i)connection refused`)
	// curl's timeout count of what arrived before it gave up.
	bytesReceived = regexp.MustCompile(`with ([0-9]+) (?:out of [0-9]+ )?bytes received`)
	// curl's one wording for a refused port and an unreachable network alike:
	// "Couldn't" up to curl 8.8, "Could not" from 8.9.
	saysNoConnect = regexp.MustCompile(`(?i)(?:couldn't|could not) connect to server`)
	// the door curl's own complaint says it knocked on: the host, the port ("H
	// port P" up to curl 8.20, "H:P" from 8.21) and, from 8.16, whether it went
	// through a proxy (" via NAME", then " over proxy NAME"). An older curl names
	// the proxy's own host and port instead.
	failedConnect = regexp.MustCompile(`Failed to connect to (\S+)(?: port |:)([0-9]+)( via | over |[ :]|$)`)
)

// secureFailures are curl's exit statuses for a TLS connection that could not
// be made or trusted: 35 the handshake, 51 an older curl's certificate check
// (a name mismatch among them), 53, 54 and 66 a crypto engine, 58 the client
// certificate, 59 the cipher, 60 certificate verification, 77 the CA bundle,
// 82 the CRL file, 83 the issuer check, 90 the pinned key, 91 the certificate
// status, 98 a client certificate the server refused. 80 (the TLS shutdown) is
// not among them: it comes after an answer (#260).
var secureFailures = []int{35, 51, 53, 54, 58, 59, 60, 66, 77, 82, 83, 90, 91, 98}

// connectProbe is a direct TCP connect to host:port, bounded by a second: the
// one question curl 8 no longer answers in words, since it says "Couldn't
// connect to server" for a refused port and an unreachable network alike. A
// package var so a test can answer it without a network.
var connectProbe = func(host, port string) error {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second)
	if err == nil {
		c.Close()
	}
	return err
}

// WhyUnreachable names why a request to endpoint got no HTTP answer, from
// curl's exit status, its stderr complaint and how long the request ran: name
// not found, connection refused, secure connection failed, or no answer. It
// returns "" for any other exit status, so the caller keeps its own words for
// what it cannot name. The host and port come from the redacted URL, never the
// raw one, so a credential in the URL cannot reach the sentence; curl's own
// words are read, never quoted (#260).
func WhyUnreachable(endpoint string, exit int, complaint string, waited time.Duration) string {
	return whyUnreachable(endpoint, exit, complaint, waited, false)
}

// whyUnreachable is WhyUnreachable for a request the caller sent through a
// proxy of its own choosing (viaProxy): a direct connect would then test a door
// curl never used.
func whyUnreachable(endpoint string, exit int, complaint string, waited time.Duration, viaProxy bool) string {
	host, port := hostPort(endpoint)
	where := ""
	if host != "" {
		where = host + " port " + port
	}
	at := func(cause string) string {
		if where == "" {
			return cause
		}
		return cause + ": " + where
	}
	noAnswer := func(detail string) string {
		s := "no answer"
		if where != "" {
			s += " from " + where
		}
		return s + detail + " " + PrivateNetworkAdvice
	}
	switch {
	case exit == 6:
		return at("name not found")
	case exit == 7:
		switch connectCause(complaint, host, port, viaProxy) {
		case causeRefused:
			return at("connection refused")
		case causeNoRoute:
			return noAnswer(": there is no network route to it")
		}
		// Nothing is known to have answered, and nothing says how long.
		return noAnswer("")
	case slices.Contains(secureFailures, exit):
		return at("secure connection failed")
	case exit == 28 || exit == -1:
		n := secondsWaited(complaint, waited)
		unit := "seconds"
		if n == 1 {
			unit = "second"
		}
		return noAnswer(fmt.Sprintf(" after %d %s", n, unit))
	}
	return ""
}

type connectFailure int

const (
	causeUnknown connectFailure = iota
	causeRefused
	causeNoRoute
)

// connectCause tells a refused port from a network with no route to the host
// for curl's exit 7. curl names the door it failed to connect to ("Failed to
// connect to H port P", "H:P" from 8.21): when that is another host or port, or
// curl says it went via or over a proxy (8.16 and later name the gateway even
// then), curl knocked on a proxy's door, and nothing it says is about the
// gateway. curl's own words decide where an older curl still gives them. curl 8
// says only "Couldn't connect to server" ("Could not" from 8.9) for both, so
// then one direct connect to the same host and port decides, and only when curl
// named the gateway's own door: curl dials a proxy whenever its environment
// names one that NO_PROXY does not exempt, a loopback gateway included, and its
// complaint then names the proxy. A complaint with neither (a stub's bare exit
// 7) is unknown.
func connectCause(complaint, host, port string, viaProxy bool) connectFailure {
	named := failedConnect.FindStringSubmatch(complaint)
	if named != nil {
		own := strings.EqualFold(strings.Trim(named[1], "[]"), host) && named[2] == port
		if !own || named[3] == " via " || named[3] == " over " {
			return causeUnknown
		}
	}
	switch {
	case saysNoRoute.MatchString(complaint):
		return causeNoRoute
	case saysRefused.MatchString(complaint):
		return causeRefused
	case host == "" || viaProxy || named == nil || !saysNoConnect.MatchString(complaint):
		return causeUnknown
	}
	err := connectProbe(host, port)
	switch {
	case err == nil:
		return causeUnknown
	case errors.Is(err, syscall.ECONNREFUSED):
		return causeRefused
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return causeNoRoute
	}
	return causeUnknown
}

// secondsWaited is how long nothing answered, in whole seconds and never less
// than one: curl's own count where it gives one ("after 2003 ms"), else the
// time the request ran.
func secondsWaited(complaint string, waited time.Duration) int {
	if m := afterMillis.FindStringSubmatch(complaint); m != nil {
		if ms, err := strconv.Atoi(m[1]); err == nil {
			return max((ms+500)/1000, 1)
		}
	}
	return max(int((waited+time.Second/2)/time.Second), 1)
}

// hostPort is the host and port a URL dials, read from profile.RedactURL's
// output and never from the raw URL: a password may open on digits and hold a
// /, which Go would read as this host's port (alice:2024/ss@proxy:3128). The
// port is the scheme's default when the URL names none; "" when no host is
// left after redaction.
func hostPort(endpoint string) (host, port string) {
	u, err := url.Parse(profile.RedactURL(endpoint))
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	port = u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Hostname(), port
}

// exitOf is curl's exit status from the error cmd.Run or cmd.Wait returned: -1
// when a signal ended it, 0 when there is no exit status to read.
func exitOf(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

// reachProgressAfter and reachBound are the launch probe's patience: a
// progress line after one second, and no more than doctor's own five seconds
// before it gives up. CCMORPH_REACH_PROGRESS and CCMORPH_REACH_BOUND can shorten
// them for a test, never lengthen them (#260).
func reachProgressAfter() time.Duration {
	return min(envDuration("CCMORPH_REACH_PROGRESS", 1), time.Second)
}
func reachBound() time.Duration { return min(envDuration("CCMORPH_REACH_BOUND", 5), 5*time.Second) }

func envDuration(name string, def float64) time.Duration {
	if d, err := EnvSeconds(os.Getenv, name, def); err == nil {
		return d
	}
	return time.Duration(def * float64(time.Second))
}

// ReachBoundSeconds is the give-up bound in whole seconds, for the progress
// line that says how long the wait can last.
func ReachBoundSeconds() int {
	return max(int((reachBound()+time.Second-1)/time.Second), 1)
}

// Reach is what the launch's reachability probe met.
type Reach struct {
	Why    string // the cause in plain words, "" when the gateway answered or failed in a way WhyUnreachable cannot name
	Waited bool   // nothing answered within the bound: curl timed out, or was stopped at the bound
}

// ReachListing is the launch's early question to a gateway (#260): one
// listing request with doctor's bounds (two seconds to connect, five in all),
// so a gateway that never answers cannot hold a launch silent. The request is
// the launch listing's own shape, so a stub that answers the listing answers
// this. progress runs once, if the request is still running after
// reachProgressAfter. Any HTTP answer, an error status included, is an answer:
// Why is "". via is the profile's CCMORPH_UPSTREAM_PROXY: the probe admits a
// session, so it takes the session's own route, through netroute's rules
// (GH-580): the proxy it names, or a direct dial where NO_PROXY or loopback
// exempts the gateway; empty or unusable, the environment's resolution
// stands. strip names environment keys the request must not carry (the
// Remote Control edge's proxy, which a named profile never dials through).
func ReachListing(base, key, via string, strip []string, progress func()) Reach {
	start := time.Now()
	bound := reachBound()
	connect := min(bound, 2*time.Second)
	listing := ctxwindow.APIURL(base, "/v1/models?limit=1000")
	// -D - writes the response headers to stdout the moment they arrive and
	// -N keeps curl from holding them back, so a gateway that has begun to
	// answer is seen before the probe stops curl; real curl holds a short body
	// until the transfer ends. A proxy's own CONNECT reply is not the gateway's
	// answer and stays out (--suppress-connect-headers).
	args := []string{"-K", "-", "-sS", "-f", "-N", "-D", "-", "--suppress-connect-headers",
		"--connect-timeout", seconds(connect), "--max-time", seconds(bound),
		listing, "-H", "anthropic-version: 2023-06-01"}
	cmd := exec.Command("curl", args...)
	config, viaProxy := keyedConfig(key), false
	if proxy := netroute.Parse(via); proxy != nil {
		// On curl's stdin, never its argv: the proxy URL may carry a password.
		if u, err := url.Parse(listing); err == nil && netroute.Exempt(u) {
			config += "noproxy = \"*\"\n"
		} else {
			config += "proxy = \"" + escCred(proxy.String()) + "\"\n"
			viaProxy = true
		}
	}
	cmd.Stdin = strings.NewReader(config)
	var complaint strings.Builder
	var got countedBytes
	cmd.Stdout, cmd.Stderr = &got, &complaint
	cmd.WaitDelay = time.Second
	env := os.Environ()
	if len(strip) > 0 {
		env = slices.DeleteFunc(env, func(e string) bool {
			k, _, _ := strings.Cut(e, "=")
			return slices.Contains(strip, k)
		})
		cmd.Env = env
	}
	if err := cmd.Start(); err != nil {
		return Reach{}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Both timers count from the call, not from curl's start. curl is stopped
	// with a twentieth of the bound to spare, so the probe has returned, curl
	// reaped, within the bound: never later than doctor's own request.
	tick := time.NewTimer(reachProgressAfter() - time.Since(start))
	defer tick.Stop()
	stop := time.NewTimer(bound - bound/20 - time.Since(start))
	defer stop.Stop()
	killed, said := false, false
	var err error
	for waiting := true; waiting; {
		select {
		case err = <-done:
			waiting = false
		case <-tick.C:
			if !said && progress != nil {
				progress()
			}
			said = true
		case <-stop.C:
			killed = true
			cmd.Process.Kill()
		}
	}
	if err == nil {
		return Reach{}
	}
	exit := exitOf(err)
	if killed {
		exit = -1
	}
	if exit == 22 || refused.MatchString(complaint.String()) {
		return Reach{} // an HTTP status is an answer
	}
	if m := bytesReceived.FindStringSubmatch(complaint.String()); m != nil && m[1] != "0" {
		return Reach{} // a slow answer is still an answer: the listing's own wait judges it
	}
	if killed && got > 0 {
		return Reach{} // stopped once the gateway had begun to answer: a slow answer, as above
	}
	why := whyUnreachable(base, exit, complaint.String(), time.Since(start), viaProxy)
	return Reach{Why: why, Waited: why != "" && (exit == 28 || exit == -1)}
}

// countedBytes counts what curl wrote to stdout (the response headers, then
// the listing) and keeps none of it: one byte is an answer.
type countedBytes int64

func (c *countedBytes) Write(p []byte) (int, error) {
	*c += countedBytes(len(p))
	return len(p), nil
}

// seconds is a duration as curl's timeout flags spell it: "2", or "0.5".
func seconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64)
}
