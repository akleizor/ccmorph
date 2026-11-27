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
	return whyUnreachableWithin(endpoint, exit, complaint, waited, viaProxy, 0)
}

// whyUnreachableWithin is whyUnreachable with claim, the longest wait the
// caller's own progress line promised and its reaping grace allow: 0 when the
// caller promised none. waited counts from the call, so scheduler delay can
// carry it past that promise; the sentence then states no more than claim,
// never a number the launch never advertised (#891).
func whyUnreachableWithin(endpoint string, exit int, complaint string, waited time.Duration, viaProxy bool, claim time.Duration) string {
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
		if claim > 0 {
			n = min(n, max(int((claim+time.Second/2)/time.Second), 1))
		}
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

// reachDefaultBound is doctor's own five seconds: the promise the progress
// line advertises and the bound a shortened one is told apart from.
const reachDefaultBound = 5 * time.Second

func reachBound() time.Duration { return min(envDuration("CCMORPH_REACH_BOUND", 5), reachDefaultBound) }

// reachRetryWindow is the window a second dial may take of the probe's shared
// deadline: left itself, or 0 for none. It reads the deadline alone, so a
// start the scheduler held (#935) can neither be credited nor start a dial
// after the deadline — the past-deadline and negative-flag dial the gate
// refused (#1021). Under a shortened bound — the test knob, never the product
// — today's rule stands: less than a second left is not worth a dial.
func reachRetryWindow(left, bound time.Duration) time.Duration {
	if left <= 0 {
		return 0
	}
	if bound < reachDefaultBound && left < time.Second {
		return 0
	}
	return left
}

// reachWaitDelay is the grace the probe gives curl to be reaped after it
// stops it at the bound (cmd.WaitDelay). A loaded scheduler can also delay
// Start, so the verdict may claim the bound plus this grace, never the
// scheduler's own delay (#891).
const reachWaitDelay = time.Second

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

// reachStart starts curl. A package var, like reachStop and connectProbe, so a
// test can hold the start the way a saturated scheduler does and prove the probe
// does not blame the gateway for it (#935).
var reachStart = func(cmd *exec.Cmd) error { return cmd.Start() }

// reachStop is the probe's stop: a timer that fires after d, what is left of
// the bound counted from the call. A package var, like connectProbe, so a test
// about what the probe reads from a curl it stopped can stop curl once its stub
// has answered, not when a loaded scheduler lets the timer win (#891).
var reachStop = func(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// reachProgressTick is the channel the probe's progress line waits on: the
// progress timer's own. A package var, like reachStop, so a test about what the
// probe says of an instant failure can hold the line back instead of racing a
// loaded scheduler; the bound tests keep the real timer and own when it fires
// (#891).
var reachProgressTick = func(t *time.Timer) <-chan time.Time { return t.C }

// Reach is what the launch's reachability probe met.
type Reach struct {
	Why    string // the cause in plain words, "" when the gateway answered or failed in a way WhyUnreachable cannot name
	Waited bool   // nothing answered within the bound: curl timed out, or was stopped at the bound
}

// reachProbe is one launch question's shared setting: what to ask and the
// promise all its dials answer to, so a dial can state the whole probe's wait
// and never outlast its deadline.
type reachProbe struct {
	listing string // the listing request's URL, the launch's own shape
	config  string // curl's stdin config: the key, the profile's proxy route
	base    string // the gateway endpoint, for the sentence's host and port
	via     bool   // the request goes through the profile's own proxy
	env     []string
	start   time.Time     // when the probe began
	stopAt  time.Time     // the deadline every dial shares
	bound   time.Duration // the promise the progress line advertises
}

// ReachListing is the launch's early question to a gateway (#260): a listing
// request with doctor's bounds (two seconds to connect, five in all), so a
// gateway that never answers cannot hold a launch silent. The request is
// the launch listing's own shape, so a stub that answers the listing answers
// this. progress runs once, if the request is still running after
// reachProgressAfter. Any HTTP answer, an error status included, is an answer:
// Why is "". via is the profile's CCMORPH_UPSTREAM_PROXY: the probe admits a
// session, so it takes the session's own route, through netroute's rules
// (GH-580): the proxy it names, or a direct dial where NO_PROXY or loopback
// exempts the gateway; empty or unusable, the environment's resolution
// stands. strip names environment keys the request must not carry (the
// Remote Control edge's proxy, which a named profile never dials through).
//
// One connect timeout inside the bound is not yet the silence a launch ends a
// turn on (#1021): a route that was down for two seconds and back a second
// later lost a turn whose retry 11 s later succeeded, on the sentence "after
// 2 seconds" — three fifths of the promised bound never spent. A dial whose
// own connect timeout fires while the probe's window is still open is answered
// by a second dial given the rest of that window whole; the question, however
// many dials, stays inside the bound the progress line advertised.
func ReachListing(base, key, via string, strip []string, progress func()) Reach {
	start := time.Now()
	bound := reachBound()
	listing := ctxwindow.APIURL(base, "/v1/models?limit=1000")
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
	var env []string
	if len(strip) > 0 {
		env = slices.DeleteFunc(os.Environ(), func(e string) bool {
			k, _, _ := strings.Cut(e, "=")
			return slices.Contains(strip, k)
		})
	}
	// progress is the probe's promise, not one dial's: one line after one
	// second of the whole question, even when the probe asks it twice.
	if progress != nil {
		ask, said := progress, false
		progress = func() {
			if !said {
				said = true
				ask()
			}
		}
	}
	// curl is stopped with a twentieth of the bound to spare, so the probe has
	// returned, curl reaped, within the bound: never later than doctor's own
	// request, in one dial or two.
	p := reachProbe{listing: listing, config: config, base: base, via: viaProxy, env: env,
		start: start, stopAt: start.Add(bound - bound/20), bound: bound}
	r, again := reachDial(p, min(bound, 2*time.Second), bound, false, progress)
	if again <= 0 {
		return r
	}
	r, _ = reachDial(p, again, again, true, progress)
	return r
}

// reachDial asks the gateway once, curl's connect bound and total window as
// given, inside the probe p's shared deadline, and says what the probe would
// report if this dial ended it. again is the window of a second dial, 0 for
// none: it is set only when the dial's own connect timeout ended it — curl's
// 28, not the probe's stop, which has no time left to ask with — and the
// shared deadline still holds time (reachRetryWindow, #1021). retryAfter says
// this dial is that second dial: its silence is the whole probe's, so the
// sentence counts the probe's wait, never curl's count of one dial.
func reachDial(p reachProbe, connect, window time.Duration, retryAfter bool, progress func()) (r Reach, again time.Duration) {
	// -D - writes the response headers to stdout the moment they arrive and
	// -N keeps curl from holding them back, so a gateway that has begun to
	// answer is seen before the probe stops curl; real curl holds a short body
	// until the transfer ends. A proxy's own CONNECT reply is not the gateway's
	// answer and stays out (--suppress-connect-headers).
	args := []string{"-K", "-", "-sS", "-f", "-N", "-D", "-", "--suppress-connect-headers",
		"--connect-timeout", seconds(connect), "--max-time", seconds(window),
		p.listing, "-H", "anthropic-version: 2023-06-01"}
	cmd := exec.Command("curl", args...)
	cmd.Stdin = strings.NewReader(p.config)
	var complaint strings.Builder
	var got countedBytes
	cmd.Stdout, cmd.Stderr = &got, &complaint
	cmd.WaitDelay = reachWaitDelay
	if p.env != nil {
		cmd.Env = p.env
	}
	dialStart := time.Now()
	if err := reachStart(cmd); err != nil {
		return Reach{}, 0
	}
	// The stop is the call's deadline, as it always was; requestStart is when
	// the request could first run. cmd.Start blocks through the scheduler's
	// fork/exec, and a starved host can hold it past the whole bound, leaving
	// the request none of its window — the loaded gate's red: a 136 ms start
	// against a 142 ms window left 38 ms of curl's own run and no answer
	// (#935). That is the scheduler's time, not the gateway's silence, so the
	// stop below withholds its verdict when less than half the window was left.
	requestStart := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	// Both timers count from the call, not from curl's start.
	tick := time.NewTimer(reachProgressAfter() - time.Since(p.start))
	defer tick.Stop()
	ticked := reachProgressTick(tick)
	stop, stopTimer := reachStop(p.stopAt.Sub(time.Now()))
	defer stopTimer()
	killed, told := false, false
	var err error
	for waiting := true; waiting; {
		select {
		case err = <-done:
			waiting = false
		case <-ticked:
			if !told && progress != nil {
				progress()
			}
			told = true
		case <-stop:
			killed = true
			cmd.Process.Kill()
		}
	}
	if err == nil {
		return Reach{}, 0
	}
	// curl's own exit, never the stop's assumption: a probe the scheduler held
	// across its stop wakes after curl has answered, and rewriting that answer
	// as the stop's signal ended a healthy launch on the gateway-down class
	// (#947). ProcessState carries the exit even when Wait returned
	// ErrWaitDelay over a pipe a surviving child held.
	exit := exitOf(err)
	if state := cmd.ProcessState; state != nil {
		exit = state.ExitCode()
	}
	if exit == 22 || refused.MatchString(complaint.String()) {
		return Reach{}, 0 // an HTTP status is an answer
	}
	if m := bytesReceived.FindStringSubmatch(complaint.String()); m != nil && m[1] != "0" {
		return Reach{}, 0 // a slow answer is still an answer: the listing's own wait judges it
	}
	if got > 0 {
		// the gateway had begun to answer — its headers are in hand — no matter
		// whose timeout ended curl: ours, or its own with a count of zero body
		// bytes (low speed, --max-time just beating the stop) (#260, #1021).
		return Reach{}, 0
	}
	// A stop that left the request less than half its window never gave it a
	// fair hearing: the scheduler spent the rest before curl could run, so
	// there is no evidence of silence to name. The launch goes on and the
	// adapter's and listing's own bounds judge the route; naming the gateway
	// dead here was the false timeout of a healthy endpoint under load — a
	// loaded run measured a 136 ms start against a 142 ms window, the stop
	// firing with 38 ms run and no answer (#935). The window is this dial's:
	// a retry held by the same scheduler owes the probe no verdict either.
	if killed && p.stopAt.Sub(requestStart) < p.stopAt.Sub(dialStart)/2 {
		return Reach{}, 0
	}
	words := complaint.String()
	if retryAfter && (exit == 28 || exit == -1) {
		words = "" // the whole probe's wait counts, not one dial's curl count (#1021)
	}
	why := whyUnreachableWithin(p.base, exit, words, time.Since(p.start), p.via, p.bound+reachWaitDelay)
	// A dial's own connect timeout spent the connect bound, not the promise:
	// the second dial gets what the shared deadline still holds (#1021).
	if !retryAfter && exit == 28 {
		again = reachRetryWindow(time.Until(p.stopAt), p.bound)
	}
	return Reach{Why: why, Waited: why != "" && (exit == 28 || exit == -1)}, again
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
