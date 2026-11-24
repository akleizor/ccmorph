package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noProxy clears every proxy variable for one test: whether a request went
// through a proxy decides whether the direct connect runs, and the operator's
// own HTTPS_PROXY must not decide a test (#774).
func noProxy(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
}

// stubConnect answers connectProbe without a network for one test.
func stubConnect(t *testing.T, err error) *[]string {
	t.Helper()
	asked := &[]string{}
	saved := connectProbe
	connectProbe = func(host, port string) error {
		*asked = append(*asked, host+":"+port)
		return err
	}
	t.Cleanup(func() { connectProbe = saved })
	return asked
}

// GH-260: four transport failures were one timeout-flavoured sentence. Each
// cause is named in plain words with the host and port; only the no-answer
// sentence carries the private-network advice; anything else is left to the
// caller ("").
func TestWhyUnreachableNamesEachCauseInPlainWords(t *testing.T) {
	noProxy(t)
	const advice = " (if it is an internal endpoint, connect your private network and try again)"
	refusedErr := fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)
	for _, c := range []struct {
		name, endpoint string
		exit           int
		complaint      string
		waited         time.Duration
		dial           error
		want           string
	}{
		{"a name that does not exist", "https://gw.example.internal", 6,
			"curl: (6) Could not resolve host: gw.example.internal", 0, nil,
			"name not found: gw.example.internal port 443"},
		{"an older curl's refused port", "http://127.0.0.1:9443", 7,
			"curl: (7) Failed to connect to 127.0.0.1 port 9443: Connection refused", 0, nil,
			"connection refused: 127.0.0.1 port 9443"},
		{"curl 8's refused port, told by a connect", "http://127.0.0.1:1", 7,
			"curl: (7) Failed to connect to 127.0.0.1 port 1 after 0 ms: Couldn't connect to server", 0, refusedErr,
			"connection refused: 127.0.0.1 port 1"},
		{"curl 8's unreachable network, told by a connect", "https://10.0.0.5", 7,
			"curl: (7) Failed to connect to 10.0.0.5 port 443 after 3 ms: Couldn't connect to server", 0,
			fmt.Errorf("dial tcp: %w", syscall.ENETUNREACH),
			"no answer from 10.0.0.5 port 443: there is no network route to it" + advice},
		{"curl 8's host with no route, told by a connect", "https://10.0.0.6", 7,
			"curl: (7) Failed to connect to 10.0.0.6 port 443 after 3 ms: Couldn't connect to server", 0,
			fmt.Errorf("dial tcp: %w", syscall.EHOSTUNREACH),
			"no answer from 10.0.0.6 port 443: there is no network route to it" + advice},
		{"an older curl's no route", "http://10.0.0.7:8080", 7,
			"curl: (7) Failed to connect to 10.0.0.7 port 8080: No route to host", 0, refusedErr,
			"no answer from 10.0.0.7 port 8080: there is no network route to it" + advice},
		{"a connect failure nothing explains", "http://unreachable.invalid", 7, "", 0,
			fmt.Errorf("dial tcp: lookup unreachable.invalid: no such host"),
			"no answer from unreachable.invalid port 80" + advice},
		{"plain HTTP on an HTTPS port", "https://127.0.0.1:18080", 35,
			"curl: (35) OpenSSL/3.0.13: error:0A00010B:SSL routines::wrong version number", 0, nil,
			"secure connection failed: 127.0.0.1 port 18080"},
		{"an untrusted certificate", "https://gw.example", 60,
			"curl: (60) SSL certificate problem: self-signed certificate", 0, nil,
			"secure connection failed: gw.example port 443"},
		{"a connect that timed out", "http://10.255.255.1:8080", 28,
			"curl: (28) Failed to connect to 10.255.255.1 port 8080 after 2003 ms: Timeout was reached", 0, nil,
			"no answer from 10.255.255.1 port 8080 after 2 seconds" + advice},
		{"a listing that never came", "https://gw.example", 28,
			"curl: (28) Operation timed out after 20001 milliseconds with 0 bytes received", 0, nil,
			"no answer from gw.example port 443 after 20 seconds" + advice},
		{"a timeout curl gave no count for", "https://gw.example", 28, "", 1400 * time.Millisecond, nil,
			"no answer from gw.example port 443 after 1 second" + advice},
		{"a request stopped at the bound", "https://gw.example", -1, "", 5 * time.Second, nil,
			"no answer from gw.example port 443 after 5 seconds" + advice},
		{"a password Go would read as a port", "http://alice:2024/ss@10.0.0.9:3128", 6, "", 0, nil,
			"name not found: 10.0.0.9 port 3128"},
		{"a URL with no host left to name", "http://alice:pa?ss@", 6, "", 0, nil, "name not found"},
		{"an HTTP error is not this function's", "https://gw.example", 22,
			"curl: (22) The requested URL returned error: 500", 0, nil, ""},
		{"an empty reply is not one of the four", "https://gw.example", 52, "curl: (52) Empty reply from server", 0, nil, ""},
		{"a bad URL is not one of the four", "htp://gw.example", 1, "curl: (1) Protocol \"htp\" not supported", 0, nil, ""},
		{"no exit status", "https://gw.example", 0, "", 0, nil, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			stubConnect(t, c.dial)
			got := WhyUnreachable(c.endpoint, c.exit, c.complaint, c.waited)
			if got != c.want {
				t.Fatalf("WhyUnreachable = %q\nwant              %q", got, c.want)
			}
			if strings.Contains(got, "curl") || strings.Contains(got, "alice") || strings.Contains(got, "2024") {
				t.Fatalf("the sentence quotes curl or the URL's credential: %q", got)
			}
		})
	}
}

// The connect that tells curl 8's refused port from an unreachable network
// dials the redacted host and port, and only when curl's own words do not
// already say which.
func TestWhyUnreachableConnectsOnlyWhenCurlDoesNotSay(t *testing.T) {
	noProxy(t)
	asked := stubConnect(t, fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED))
	WhyUnreachable("http://alice:2024/ss@127.0.0.1:9", 7, "curl: (7) Failed to connect to 127.0.0.1 port 9 after 0 ms: Couldn't connect to server", 0)
	WhyUnreachable("http://127.0.0.1:9", 7, "curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused", 0)
	WhyUnreachable("http://127.0.0.1:9", 6, "", 0)
	WhyUnreachable("http://127.0.0.1:9", 7, "", 0) // a stub's bare exit 7
	if strings.Join(*asked, ",") != "127.0.0.1:9" {
		t.Fatalf("connects asked %v, want only the curl 8 case on the redacted host", *asked)
	}
	// Through a proxy, a direct connect tests a door curl never used.
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:3128")
	got := WhyUnreachable("http://127.0.0.1:9", 7, "curl: (7) Failed to connect to 127.0.0.1 port 3128 after 0 ms: Couldn't connect to server", 0)
	if len(*asked) != 1 || !strings.HasPrefix(got, "no answer from 127.0.0.1 port 9") {
		t.Fatalf("under a proxy: connects %v, sentence %q", *asked, got)
	}
}

// reachCurl puts a curl on PATH that logs its arguments and environment and
// then runs body.
func reachCurl(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"printf 'PROXY=%s\\n' \"${HTTPS_PROXY-<unset>}\" >> '" + dir + "/request'\n" +
		"for a do printf 'ARG=%s\\n' \"$a\"; done >> '" + dir + "/request'\n" +
		"cat >> '" + dir + "/request'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	return dir
}

// GH-260: a gateway that never answers gets a progress line after the
// progress bound and a give-up at the bound, and the verdict says it waited.
func TestReachListingPrintsProgressAndGivesUpOnASilentGateway(t *testing.T) {
	reachCurl(t, "exec sleep 30")
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	said := 0
	start := time.Now()
	r := ReachListing("http://10.255.255.1:8080", "k", nil, func() { said++ })
	took := time.Since(start)
	if said != 1 {
		t.Fatalf("progress ran %d times, want once", said)
	}
	if took < 700*time.Millisecond || took > 3*time.Second {
		t.Fatalf("gave up after %v, want the 0.8s bound", took)
	}
	if !r.Waited || !strings.HasPrefix(r.Why, "no answer from 10.255.255.1 port 8080 after 1 second") ||
		!strings.Contains(r.Why, "private network") {
		t.Fatalf("verdict %+v, want a waited no-answer naming the host and port", r)
	}
}

// An instant failure is named at once, with no progress line, and is not a
// wait the launch must end on.
func TestReachListingNamesAnInstantFailureWithoutProgress(t *testing.T) {
	noProxy(t)
	stubConnect(t, nil)
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.5")
	for _, c := range []struct{ name, body, want string }{
		{"refused", "printf '%s\\n' 'curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused' >&2; exit 7",
			"connection refused: 127.0.0.1 port 9"},
		{"name not found", "printf '%s\\n' 'curl: (6) Could not resolve host: 127.0.0.1' >&2; exit 6",
			"name not found: 127.0.0.1 port 9"},
		{"secure connection", "exit 35", "secure connection failed: 127.0.0.1 port 9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reachCurl(t, c.body)
			said := 0
			r := ReachListing("http://127.0.0.1:9", "k", nil, func() { said++ })
			if said != 0 || r.Waited || r.Why != c.want {
				t.Fatalf("got %+v with %d progress lines, want %q at once", r, said, c.want)
			}
		})
	}
}

// Any HTTP answer is an answer, and a failure none of the four causes names is
// left to the launch's own words: the probe says nothing about either.
func TestReachListingStepsAsideForAnAnswer(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"a listing", `printf '{"data":[]}'`},
		{"a rejected key", "printf '%s\\n' 'curl: (22) The requested URL returned error: 401' >&2; exit 22"},
		{"an error status in other words", "printf '%s\\n' 'curl: (56) The requested URL returned error: 403' >&2; exit 56"},
		{"curl -f's error with no words", "exit 22"},
		{"a failure the four do not name", "exit 1"},
		{"a slow answer cut at the bound", "printf '%s\\n' 'curl: (28) Operation timed out after 5001 milliseconds with 1200 out of 9000 bytes received' >&2; exit 28"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reachCurl(t, c.body)
			if r := ReachListing("http://127.0.0.1:9", "k", nil, nil); r != (Reach{}) {
				t.Fatalf("got %+v, want nothing to say", r)
			}
		})
	}
}

// The probe's patience is doctor's: the same connect and total bounds as
// ProbeListing's request, a progress line after one second, the key only on
// curl's stdin, and the edge's proxy dropped when asked.
func TestReachListingAsksWithDoctorsBoundsAndKeepsTheKeyOffArgv(t *testing.T) {
	if reachProgressAfter() != time.Second || reachBound() != 5*time.Second || ReachBoundSeconds() != 5 {
		t.Fatalf("defaults %v/%v, want 1s progress and doctor's 5s", reachProgressAfter(), reachBound())
	}
	bounds := func(request string) string {
		var out []string
		lines := strings.Split(request, "\n")
		for i, line := range lines {
			if (line == "ARG=--connect-timeout" || line == "ARG=--max-time") && i+1 < len(lines) {
				out = append(out, line, lines[i+1])
			}
		}
		return strings.Join(out, " ")
	}
	dir := reachCurl(t, "exit 7")
	stubConnect(t, nil)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47999")
	ProbeListing("http://127.0.0.1:9", "sk-reach-260")
	doctor, _ := os.ReadFile(filepath.Join(dir, "request"))
	os.Remove(filepath.Join(dir, "request"))
	ReachListing("http://127.0.0.1:9", "sk-reach-260", []string{"HTTPS_PROXY"}, nil)
	reach, _ := os.ReadFile(filepath.Join(dir, "request"))
	if bounds(string(reach)) != bounds(string(doctor)) || bounds(string(reach)) == "" {
		t.Fatalf("reach bounds %q, doctor's %q", bounds(string(reach)), bounds(string(doctor)))
	}
	if strings.Contains(strings.Join(argLines(string(reach)), "\n"), "sk-reach-260") || !strings.Contains(string(reach), "sk-reach-260") {
		t.Fatalf("the key must travel on stdin only:\n%s", reach)
	}
	if !strings.Contains(string(reach), "PROXY=<unset>") {
		t.Fatalf("the stripped proxy reached curl:\n%s", reach)
	}
}

func argLines(request string) []string {
	var out []string
	for _, line := range strings.Split(request, "\n") {
		if strings.HasPrefix(line, "ARG=") {
			out = append(out, line)
		}
	}
	return out
}

// doctor's and models' requests name the cause the same way; an answered or
// refused-status request names none.
func TestProbeAndModelsAnswersNameTheCause(t *testing.T) {
	noProxy(t)
	stubConnect(t, nil)
	fakeCurl(t, "", 7, "curl: (7) Failed to connect to 127.0.0.1 port 9: Connection refused")
	if _, code, why := ProbeListingWhy("http://127.0.0.1:9", "k"); code != "000" || why != "connection refused: 127.0.0.1 port 9" {
		t.Fatalf("ProbeListingWhy = %q, %q", code, why)
	}
	if _, status, why, ok := ModelsAnswerWhy("http://127.0.0.1:9", "k"); ok || status != 0 || why != "connection refused: 127.0.0.1 port 9" {
		t.Fatalf("ModelsAnswerWhy = %d, %q, %v", status, why, ok)
	}
	fakeCurl(t, "", 22, "curl: (22) The requested URL returned error: 401")
	if _, status, why, _ := ModelsAnswerWhy("http://127.0.0.1:9", "k"); status != 401 || why != "" {
		t.Fatalf("a refused status is an answer: %d, %q", status, why)
	}
	fakeCurl(t, catalogue, 0)
	if _, _, why, ok := ModelsAnswerWhy("http://127.0.0.1:9", "k"); !ok || why != "" {
		t.Fatalf("a served listing names no cause: %q, %v", why, ok)
	}
}
