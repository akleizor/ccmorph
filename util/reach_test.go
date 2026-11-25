package util

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// noProxy clears every proxy variable for one test, so the operator's own
// HTTPS_PROXY cannot route a test's curl (#774).
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
	r := ReachListing("http://10.255.255.1:8080", "k", "", nil, func() { said++ })
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
		{"an older curl's certificate name mismatch", "exit 51", "secure connection failed: 127.0.0.1 port 9"},
	} {
		t.Run(c.name, func(t *testing.T) {
			reachCurl(t, c.body)
			said := 0
			r := ReachListing("http://127.0.0.1:9", "k", "", nil, func() { said++ })
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
			if r := ReachListing("http://127.0.0.1:9", "k", "", nil, nil); r != (Reach{}) {
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
	ReachListing("http://127.0.0.1:9", "sk-reach-260", "", []string{"HTTPS_PROXY"}, nil)
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

// GH-260: the probe admits a session, so it takes the session's route: the
// profile's CCMORPH_UPSTREAM_PROXY when it names one (on curl's stdin, since it
// may carry a password), a direct dial where NO_PROXY exempts the gateway, and
// the environment's own resolution when the knob is empty or unusable.
func TestReachListingTakesTheProfilesProxyRoute(t *testing.T) {
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	for _, c := range []struct {
		name, base, via, noProxy, want, absent string
	}{
		{"the profile's proxy", "http://10.0.0.5:8080", "http://bridge:s3cr3t@127.0.0.1:3128", "",
			`proxy = "http://bridge:s3cr3t@127.0.0.1:3128"`, "noproxy"},
		{"a gateway NO_PROXY exempts", "http://10.0.0.5:8080", "http://127.0.0.1:3128", "10.0.0.5",
			`noproxy = "*"`, "\nproxy = "},
		{"a loopback gateway", "http://127.0.0.1:9", "http://127.0.0.1:3128", "",
			`noproxy = "*"`, "\nproxy = "},
		{"no knob", "http://10.0.0.5:8080", "", "", "", "proxy"},
		{"an unusable knob", "http://10.0.0.5:8080", "not a url", "", "", "proxy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("NO_PROXY", c.noProxy)
			dir := reachCurl(t, "exit 0")
			ReachListing(c.base, "k", c.via, nil, nil)
			request, _ := os.ReadFile(filepath.Join(dir, "request"))
			if c.want != "" && !strings.Contains(string(request), c.want+"\n") {
				t.Fatalf("want %q on curl's stdin:\n%s", c.want, request)
			}
			if strings.Contains(string(request), c.absent) {
				t.Fatalf("%q must not be asked for:\n%s", c.absent, request)
			}
			if strings.Contains(strings.Join(argLines(string(request)), "\n"), "3128") {
				t.Fatalf("the proxy must not reach curl's argv:\n%s", request)
			}
		})
	}
	// Through the profile's proxy, a direct connect would test a door curl never used.
	asked := stubConnect(t, fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED))
	reachCurl(t, `printf '%s\n' "curl: (7) Failed to connect to 127.0.0.1 port 3128 after 0 ms: Couldn't connect to server" >&2; exit 7`)
	r := ReachListing("http://10.0.0.5:8080", "k", "http://127.0.0.1:3128", nil, nil)
	if len(*asked) != 0 || !strings.HasPrefix(r.Why, "no answer from 10.0.0.5 port 8080") {
		t.Fatalf("through the profile's proxy: connects %v, verdict %+v", *asked, r)
	}
}

// GH-260 (gate finding): every curl exit that means a TLS connection could not
// be made or trusted is "secure connection failed" with the host and port, an
// older curl's 51 for a certificate name mismatch among them, and none of them
// blames a network. 80 (a TLS shutdown that failed) comes after an answer and
// is left to the caller.
func TestWhyUnreachableNamesEveryTLSFailureAsSecure(t *testing.T) {
	for _, exit := range []int{35, 51, 53, 54, 58, 59, 60, 66, 77, 82, 83, 90, 91, 98} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			got := WhyUnreachable("https://gw.example", exit, fmt.Sprintf("curl: (%d) SSL: no alternative certificate subject name matches target hostname", exit), 0)
			if got != "secure connection failed: gw.example port 443" {
				t.Fatalf("exit %d: WhyUnreachable = %q", exit, got)
			}
		})
	}
	if got := WhyUnreachable("https://gw.example", 80, "curl: (80) Failed to shut down the SSL connection", 0); got != "" {
		t.Fatalf("exit 80 comes after an answer: WhyUnreachable = %q, want \"\"", got)
	}
}

// GH-260 (gate finding): whether curl went through a proxy is read from the
// door its complaint names, never guessed from proxy variables. NO_PROXY, an
// upper-case HTTP_PROXY curl ignores for http://, or a proxy for the other
// scheme leaves curl knocking on the gateway's own door: a refused port is then
// "connection refused". curl dials a proxy even for a loopback gateway, and a
// proxy's door says nothing about the gateway: an older curl's "Connection
// refused" or "No route to host" from the proxy is not the gateway's, and no
// direct connect is made.
func TestWhyUnreachableReadsWhichDoorCurlKnockedOn(t *testing.T) {
	const advice = " (if it is an internal endpoint, connect your private network and try again)"
	curl8 := func(host, port string) string {
		return "curl: (7) Failed to connect to " + host + " port " + port + " after 0 ms: Couldn't connect to server"
	}
	for _, c := range []struct {
		name, endpoint, complaint string
		env                       []string // NAME=value pairs
		wantAsked, want           string
	}{
		{"NO_PROXY exempts the gateway", "https://127.0.0.1:9", curl8("127.0.0.1", "9"),
			[]string{"HTTPS_PROXY=http://127.0.0.1:3128", "NO_PROXY=127.0.0.1"},
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"no_proxy in lower case", "https://127.0.0.1:9", curl8("127.0.0.1", "9"),
			[]string{"https_proxy=http://127.0.0.1:3128", "no_proxy=127.0.0.1"},
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"an upper-case HTTP_PROXY curl ignores for http://", "http://127.0.0.1:9", curl8("127.0.0.1", "9"),
			[]string{"HTTP_PROXY=http://127.0.0.1:3128"},
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"a proxy for the other scheme", "https://127.0.0.1:9", curl8("127.0.0.1", "9"),
			[]string{"http_proxy=http://127.0.0.1:3128"},
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"a host curl spells as typed", "http://GW.example:9", curl8("GW.example", "9"), nil,
			"gw.example:9", "connection refused: gw.example port 9"},
		{"an IPv6 gateway", "http://[::1]:9", curl8("::1", "9"), nil,
			"::1:9", "connection refused: ::1 port 9"},
		{"a proxy on the gateway's own host", "http://127.0.0.1:9", curl8("127.0.0.1", "3128"),
			[]string{"http_proxy=http://127.0.0.1:3128"},
			"", "no answer from 127.0.0.1 port 9" + advice},
		{"a loopback gateway curl still proxies", "http://localhost:9", curl8("127.0.0.1", "3128"),
			[]string{"http_proxy=http://127.0.0.1:3128"},
			"", "no answer from localhost port 9" + advice},
		{"an older curl's refused proxy", "https://gw.example",
			"curl: (7) Failed to connect to proxy.corp port 3128: Connection refused",
			[]string{"HTTPS_PROXY=http://proxy.corp:3128"},
			"", "no answer from gw.example port 443" + advice},
		{"an older curl's proxy with no route", "https://gw.example",
			"curl: (7) Failed to connect to proxy.corp port 3128: No route to host",
			[]string{"HTTPS_PROXY=http://proxy.corp:3128"},
			"", "no answer from gw.example port 443" + advice},
		{"curl 8's words naming no door", "http://127.0.0.1:9", "curl: (7) Couldn't connect to server", nil,
			"", "no answer from 127.0.0.1 port 9" + advice},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
				t.Setenv(k, "")
			}
			for _, kv := range c.env {
				k, v, _ := strings.Cut(kv, "=")
				t.Setenv(k, v)
			}
			asked := stubConnect(t, fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED))
			got := WhyUnreachable(c.endpoint, 7, c.complaint, 0)
			if strings.Join(*asked, ",") != c.wantAsked || got != c.want {
				t.Fatalf("connects %v, sentence %q\nwant connects [%s], sentence %q", *asked, got, c.wantAsked, c.want)
			}
		})
	}
}

// GH-260: curl has worded one connect failure four ways, and each is read. The
// words are curl's own source: "Couldn't connect to server" up to 8.8, "Could
// not" from 8.9; the gateway named even through a proxy, "via NAME", in 8.16 to
// 8.20; "H:P" and "over proxy NAME" from 8.21. A refused port is "connection
// refused" in all of them, and a proxy's door is never called the gateway's.
func TestWhyUnreachableReadsEveryCurlWording(t *testing.T) {
	const advice = " (if it is an internal endpoint, connect your private network and try again)"
	refusedErr := fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED)
	for _, c := range []struct {
		name, endpoint, complaint string
		dial                      error
		wantAsked, want           string
	}{
		{"8.8 refused", "http://127.0.0.1:9", "Failed to connect to 127.0.0.1 port 9 after 0 ms: Couldn't connect to server", refusedErr,
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"8.9 to 8.20 refused", "http://127.0.0.1:9", "Failed to connect to 127.0.0.1 port 9 after 0 ms: Could not connect to server", refusedErr,
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"8.21 refused", "http://127.0.0.1:9", "Failed to connect to 127.0.0.1:9 after 0 ms: Could not connect to server", refusedErr,
			"127.0.0.1:9", "connection refused: 127.0.0.1 port 9"},
		{"8.21 refused, a host name in another case", "http://GW.example", "Failed to connect to GW.example:80 after 0 ms: Could not connect to server", refusedErr,
			"gw.example:80", "connection refused: gw.example port 80"},
		{"8.21 refused, IPv6", "http://[::1]:9", "Failed to connect to ::1:9 after 0 ms: Could not connect to server", refusedErr,
			"::1:9", "connection refused: ::1 port 9"},
		{"8.21 refused, IPv6 in brackets", "http://[::1]:9", "Failed to connect to [::1]:9 after 0 ms: Could not connect to server", refusedErr,
			"::1:9", "connection refused: ::1 port 9"},
		{"8.9 no route", "http://10.0.0.5", "Failed to connect to 10.0.0.5 port 80 after 3 ms: Could not connect to server",
			fmt.Errorf("dial tcp: %w", syscall.EHOSTUNREACH),
			"10.0.0.5:80", "no answer from 10.0.0.5 port 80: there is no network route to it" + advice},
		{"8.21 no route", "http://10.0.0.5", "Failed to connect to 10.0.0.5:80 after 3 ms: Could not connect to server",
			fmt.Errorf("dial tcp: %w", syscall.ENETUNREACH),
			"10.0.0.5:80", "no answer from 10.0.0.5 port 80: there is no network route to it" + advice},
		{"8.9 to 8.15 proxy refused", "https://gw.example", "Failed to connect to proxy.corp port 3128 after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from gw.example port 443" + advice},
		{"8.16 to 8.20 proxy refused", "https://gw.example", "Failed to connect to gw.example port 443 via proxy.corp after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from gw.example port 443" + advice},
		{"8.21 proxy refused", "https://gw.example", "Failed to connect to gw.example:443 over proxy proxy.corp after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from gw.example port 443" + advice},
		{"8.21 proxy refused on the gateway's own host and port", "http://127.0.0.1:9", "Failed to connect to 127.0.0.1:9 over proxy 127.0.0.1 after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from 127.0.0.1 port 9" + advice},
		{"8.21 another peer", "https://gw.example", "Failed to connect to gw.example:443 via 10.0.0.9:8443 after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from gw.example port 443" + advice},
		{"8.21 another port", "https://gw.example", "Failed to connect to gw.example:8443 after 0 ms: Could not connect to server", refusedErr,
			"", "no answer from gw.example port 443" + advice},
	} {
		t.Run(c.name, func(t *testing.T) {
			noProxy(t)
			asked := stubConnect(t, c.dial)
			got := WhyUnreachable(c.endpoint, 7, "curl: (7) "+c.complaint, 0)
			if strings.Join(*asked, ",") != c.wantAsked || got != c.want {
				t.Fatalf("connects %v, sentence %q\nwant connects [%s], sentence %q", *asked, got, c.wantAsked, c.want)
			}
		})
	}
}

// GH-260 (gate finding): CCMORPH_REACH_PROGRESS and CCMORPH_REACH_BOUND can
// shorten the probe's patience for a test, never stretch it past the 1 s
// progress line and doctor's 5 s, and the progress line never advertises more.
func TestReachBoundsOnlyShorten(t *testing.T) {
	for _, c := range []struct {
		progress, bound string
		wantProgress    time.Duration
		wantBound       time.Duration
		wantSeconds     int
	}{
		{"", "", time.Second, 5 * time.Second, 5},
		{"2.1", "5.1", time.Second, 5 * time.Second, 5},
		{"60", "3600", time.Second, 5 * time.Second, 5},
		{"0.2", "0.8", 200 * time.Millisecond, 800 * time.Millisecond, 1},
		{"0.5", "2.5", 500 * time.Millisecond, 2500 * time.Millisecond, 3},
		{"soon", "-1", time.Second, 5 * time.Second, 5},
		{"0", "0", time.Second, 5 * time.Second, 5},
	} {
		t.Run(c.progress+"/"+c.bound, func(t *testing.T) {
			t.Setenv("CCMORPH_REACH_PROGRESS", c.progress)
			t.Setenv("CCMORPH_REACH_BOUND", c.bound)
			if reachProgressAfter() != c.wantProgress || reachBound() != c.wantBound || ReachBoundSeconds() != c.wantSeconds {
				t.Fatalf("got %v / %v / %ds, want %v / %v / %ds", reachProgressAfter(), reachBound(), ReachBoundSeconds(),
					c.wantProgress, c.wantBound, c.wantSeconds)
			}
		})
	}
}

// GH-260 (gate finding): with the knobs asking for more than the contract and
// a curl that ignores its own --max-time, the progress line still comes within
// two seconds and the probe has returned, curl stopped and reaped, within
// doctor's five, counted from the call.
func TestReachListingReturnsWithinDoctorsBoundWhenCurlIgnoresItsOwn(t *testing.T) {
	reachCurl(t, "exec sleep 30")
	t.Setenv("CCMORPH_REACH_PROGRESS", "2.1")
	t.Setenv("CCMORPH_REACH_BOUND", "5.1")
	start := time.Now()
	var progressAt time.Duration
	said := 0
	r := ReachListing("http://10.255.255.1:8080", "k", "", nil, func() { said++; progressAt = time.Since(start) })
	took := time.Since(start)
	if said != 1 || progressAt < 900*time.Millisecond || progressAt > 2*time.Second {
		t.Fatalf("progress ran %d time(s), at %v; want once, after about one second", said, progressAt)
	}
	if took > 5*time.Second {
		t.Fatalf("gave up after %v, past doctor's 5 s", took)
	}
	if !r.Waited || !strings.HasPrefix(r.Why, "no answer from 10.255.255.1 port 8080 after 5 seconds") {
		t.Fatalf("verdict %+v, want a waited no-answer after 5 seconds", r)
	}
}

// A shortened bound is kept the same way: the probe returns before it.
func TestReachListingReturnsBeforeAShortenedBound(t *testing.T) {
	reachCurl(t, "exec sleep 30")
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	start := time.Now()
	r := ReachListing("http://10.255.255.1:8080", "k", "", nil, nil)
	if took := time.Since(start); took >= 800*time.Millisecond || !r.Waited {
		t.Fatalf("returned after %v with %+v, want a waited verdict before the 0.8 s bound", took, r)
	}
}

// GH-260 (gate finding): with real curl, which holds a short body back until
// the transfer ends, a gateway that has sent its status line is answering:
// headers alone, or headers and the start of the listing, then nothing until
// the probe stops curl, is not "no answer" and carries no private-network
// advice. A gateway that accepts the connection and sends nothing is silence.
func TestReachListingWithRealCurlCountsAnyResponseAsAnAnswer(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl on PATH")
	}
	noProxy(t)
	t.Setenv("NO_PROXY", "*")
	t.Setenv("CURL_HOME", t.TempDir()) // no operator .curlrc
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	for _, c := range []struct {
		name, body string
		headers    bool
		answered   bool
	}{
		{"the status line and headers, then nothing", "", true, true},
		{"the start of the listing, then nothing", `{"data":[`, true, true},
		{"a connection that never answers", "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.headers {
					w.Header().Set("Content-Length", "11")
					w.WriteHeader(http.StatusOK)
					fmt.Fprint(w, c.body)
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done() // held until the probe stops curl
			}))
			defer srv.Close()
			r := ReachListing(srv.URL, "k", "", nil, nil)
			if c.answered && r != (Reach{}) {
				t.Fatalf("got %+v, want nothing to say about a gateway that answered", r)
			}
			if !c.answered && (!r.Waited || !strings.HasPrefix(r.Why, "no answer from 127.0.0.1 port ")) {
				t.Fatalf("got %+v, want a waited no-answer", r)
			}
		})
	}
}

// A listing still arriving when the probe stops curl is a slow answer, not
// silence: the launch goes on and the listing's own wait judges it.
func TestReachListingCountsAListingCutShortAsAnAnswer(t *testing.T) {
	reachCurl(t, `printf '{"data":['; exec sleep 30`)
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.2")
	t.Setenv("CCMORPH_REACH_BOUND", "0.8")
	if r := ReachListing("http://10.255.255.1:8080", "k", "", nil, nil); r != (Reach{}) {
		t.Fatalf("got %+v, want nothing to say about a gateway that was answering", r)
	}
}
