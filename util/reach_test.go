package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// #260: a gateway that cannot be reached gets the one sentence its cause
// deserves, in plain words with the host and port — never curl's words, never
// the credential the URL may carry.
func TestWhyUnreachableNamesEachCauseInPlainWords(t *testing.T) {
	for _, c := range []struct {
		name, endpoint, complaint string
		exit, bound               int
		want                      string
	}{
		{"a name that does not exist", "https://gw.example.internal", "curl: (6) Could not resolve host: gw.example.internal", 6, 5,
			"name not found: gw.example.internal port 443"},
		{"a refused connection", "http://127.0.0.1:9443", "curl: (7) Failed to connect to 127.0.0.1 port 9443 after 0 ms: Couldn't connect to server", 7, 5,
			"connection refused: 127.0.0.1 port 9443"},
		{"a failed connection curl gave no reason for", "https://old.example", "", 7, 5,
			"no answer from old.example port 443 (if it is an internal endpoint, connect your private network and try again)"},
		{"no route to the host", "https://10.255.255.1:8443", "curl: (7) Failed to connect to 10.255.255.1 port 8443 after 2 ms: No route to host", 7, 5,
			"no route to host: 10.255.255.1 port 8443"},
		{"an unreachable network", "http://[fd00::1]:8080", "curl: (7) Failed to connect to fd00::1 port 8080: Network is unreachable", 7, 5,
			"no route to host: fd00::1 port 8080"},
		{"a plain-HTTP answer on an HTTPS port", "https://127.0.0.1:18080", "curl: (35) LibreSSL/3.3.6: error:1404B42E:SSL routines::ST_CONNECT:tlsv1 alert protocol version", 35, 5,
			"secure connection failed: 127.0.0.1 port 18080"},
		{"an untrusted certificate", "https://self-signed.example", "curl: (60) SSL certificate problem: self-signed certificate", 60, 5,
			"secure connection failed: self-signed.example port 443"},
		{"no answer, in milliseconds", "http://10.255.255.1:8080", "curl: (28) Failed to connect to 10.255.255.1 port 8080 after 2003 ms: Timeout was reached", 28, 5,
			"no answer from 10.255.255.1 port 8080 after 2 seconds (if it is an internal endpoint, connect your private network and try again)"},
		{"no answer, in whole milliseconds", "https://old.example", "curl: (28) Operation timed out after 20001 milliseconds", 28, 5,
			"no answer from old.example port 443 after 20 seconds (if it is an internal endpoint, connect your private network and try again)"},
		{"no answer, killed at the bound", "https://old.example", "", -1, 1,
			"no answer from old.example port 443 after 1 second (if it is an internal endpoint, connect your private network and try again)"},
		{"no answer, killed at a five-second bound", "https://old.example", "", -1, 5,
			"no answer from old.example port 443 after 5 seconds (if it is an internal endpoint, connect your private network and try again)"},
		{"an unclassified failure keeps its counsel", "https://old.example", "curl: (56) Recv failure: Connection was reset", 56, 5,
			"the request failed without an answer"},
		{"a credential in the URL is never repeated", "https://sk-live@example.internal:8443/ignored?q=1", "curl: (6) Could not resolve host: example.internal", 6, 5,
			"name not found: example.internal port 8443"},
		{"a password is not read as the port", "http://alice:2024/ss@proxy:3128", "curl: (7) Failed to connect to proxy port 3128: Connection refused", 7, 5,
			"connection refused: proxy port 3128"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := WhyUnreachable(c.endpoint, c.exit, c.complaint, c.bound); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// SaysNoAnswer separates silence from the instant failures, which is what a
// launch hangs on: a refused port or a name that does not resolve is answered
// at once, while nothing downstream of a silent gateway ever comes back — so
// only silence is a give-up (#260). "no route to host" is the boundary: an
// instant failure that starts with the same word.
func TestSaysNoAnswerSeparatesSilenceFromInstantFailures(t *testing.T) {
	for _, c := range []struct {
		why  string
		want bool
	}{
		{"no answer from old.example port 443 (if it is an internal endpoint, connect your private network and try again)", true},
		{"no answer from old.example port 443 after 20 seconds (if it is an internal endpoint, connect your private network and try again)", true},
		{"name not found: gw.example.internal port 443", false},
		{"connection refused: 127.0.0.1 port 9443", false},
		{"no route to host: 10.255.255.1 port 8443", false},
		{"secure connection failed: self-signed.example port 443", false},
		{"the request failed without an answer", false},
		{"", false},
	} {
		if got := SaysNoAnswer(c.why); got != c.want {
			t.Errorf("SaysNoAnswer(%q) = %v, want %v", c.why, got, c.want)
		}
	}
}

// A remembered window excuses only a failure that came back at once; silence the
// probe sat through is a give-up whatever the launch remembers (#260).
func TestSaysWaitedNoAnswerSeparatesSilenceSatThroughFromAnInstantFailure(t *testing.T) {
	for _, c := range []struct {
		why  string
		want bool
	}{
		{"no answer from old.example port 443 after 5 seconds (if it is an internal endpoint, connect your private network and try again)", true},
		{"no answer from old.example port 443 after 1 second (if it is an internal endpoint, connect your private network and try again)", true},
		{"no answer from old.example port 443 (if it is an internal endpoint, connect your private network and try again)", false},
		{"connection refused: old.example port 443", false},
	} {
		if got := SaysWaitedNoAnswer(c.why); got != c.want {
			t.Errorf("SaysWaitedNoAnswer(%q) = %v, want %v", c.why, got, c.want)
		}
	}
}

// The probe's patience is doctor's own: a progress line after one second, no
// answer named after five — never the ~21 seconds a silent gateway cost a
// launch before #260.
func TestReachBoundsDefaultToDoctorsPatience(t *testing.T) {
	if reachProgressAfter() != 1*time.Second {
		t.Errorf("progress comes after %v, want 1s", reachProgressAfter())
	}
	if reachGiveUpAfter() != 5*time.Second {
		t.Errorf("give-up comes after %v, want 5s", reachGiveUpAfter())
	}
}

// silentCurl is a gateway that never answers: the probe must say so while it
// waits, then give up within its bound and name the silence.
func TestReachListingPrintsProgressAndGivesUpOnASilentGateway(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "curl"), []byte("#!/bin/sh\nwhile :; do :; done\n"), 0o755)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("CCMORPH_REACH_PROGRESS", "0.02")
	t.Setenv("CCMORPH_REACH_BOUND", "0.1")
	progress := 0
	body, ok, why := ReachListing("https://old.example", "test-key", func() { progress++ })
	if ok || body != "" {
		t.Fatalf("a silent gateway is not reachable, got ok=%v body=%q", ok, body)
	}
	if progress != 1 {
		t.Errorf("the wait must be said exactly once, got %d times", progress)
	}
	want := "no answer from old.example port 443 after 1 second (if it is an internal endpoint, connect your private network and try again)"
	if why != want {
		t.Errorf("got why %q, want %q", why, want)
	}
}

// A gateway that refuses the connection answers at once: no progress line, the
// refusal named. The window is widened so a slow spawn can never race it — the
// assertion is about the probe's behavior, not this machine's clock.
func TestReachListingNamesARefusedGatewayWithoutAProgressLine(t *testing.T) {
	fakeCurl(t, "", 7, "curl: (7) Failed to connect to old.example port 443 after 0 ms: Connection refused")
	t.Setenv("CCMORPH_REACH_PROGRESS", "30")
	t.Setenv("CCMORPH_REACH_BOUND", "60")
	progress := 0
	body, ok, why := ReachListing("https://old.example", "test-key", func() { progress++ })
	if ok || body != "" {
		t.Fatalf("a refused gateway is not reachable, got ok=%v body=%q", ok, body)
	}
	if progress != 0 {
		t.Errorf("a refusal needs no progress line, got %d", progress)
	}
	if why != "connection refused: old.example port 443" {
		t.Errorf("got why %q", why)
	}
}

// An HTTP error is an answer: the probe steps aside and lets the listing paths
// name the status, so a gateway that 404s its model list still launches the way
// it did before #260.
func TestReachListingStepsAsideForAnHTTPError(t *testing.T) {
	fakeCurl(t, "", 56, "curl: (56) The requested URL returned error: 404")
	t.Setenv("CCMORPH_REACH_PROGRESS", "30")
	t.Setenv("CCMORPH_REACH_BOUND", "60") // a slow spawn must never race the bound
	body, ok, why := ReachListing("https://old.example", "test-key", nil)
	if !ok || body != "" || why != "" {
		t.Fatalf("an HTTP error is an answer, got ok=%v body=%q why=%q", ok, body, why)
	}
}

// A failure outside the four causes is not this probe's to name: the launch
// path's own words say it and point at doctor (#286), so the probe steps aside
// exactly as it does for an HTTP error.
func TestReachListingStepsAsideForACauseItCannotPlace(t *testing.T) {
	fakeCurl(t, "", 1, "curl: (1) Protocol \"htp\" not supported")
	t.Setenv("CCMORPH_REACH_PROGRESS", "30")
	t.Setenv("CCMORPH_REACH_BOUND", "60") // a slow spawn must never race the bound
	body, ok, why := ReachListing("htp://old.example", "test-key", nil)
	if !ok || body != "" || why != "" {
		t.Fatalf("an unplaced cause is left to the launch path, got ok=%v body=%q why=%q", ok, body, why)
	}
}

// A gateway that answers is the launch route's own listing, kept for it: the
// probe costs the launch its request, not an extra one.
func TestReachListingKeepsTheAnswerForTheLaunchRoute(t *testing.T) {
	fakeCurl(t, catalogue+"\n", 0)
	t.Setenv("CCMORPH_REACH_PROGRESS", "30")
	t.Setenv("CCMORPH_REACH_BOUND", "60") // a slow spawn must never race the bound
	body, ok, why := ReachListing("https://old.example", "test-key", nil)
	if !ok || why != "" {
		t.Fatalf("a served listing is reachable, got ok=%v why=%q", ok, why)
	}
	if !strings.Contains(body, "served-target") || strings.HasSuffix(body, "\n") {
		t.Errorf("the launch route's listing is the probe's answer, trimmed, got %q", body)
	}
}
