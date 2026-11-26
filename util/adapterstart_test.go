package util

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCore writes a stub `core adapter` script answering with body.
func fakeCore(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "core")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdapterStartHandsBackPortAndNeverTheTTY(t *testing.T) {
	stdin := filepath.Join(t.TempDir(), "stdin")
	// The 610137d wedge: the adapter holding the terminal on fd0 froze the TUI.
	core := fakeCore(t, `if [ -t 0 ]; then echo tty; else echo not-a-tty; fi > '`+stdin+`'; echo PORT=4242; sleep 5`)
	var out, errw bytes.Buffer
	if rc := adapterStart([]string{core, "p", "m", "1", os.DevNull}, nil, &out, &errw); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errw.String())
	}
	if !strings.Contains(out.String(), "PORT=4242") || !strings.Contains(out.String(), "PID=") {
		t.Fatalf("out %q", out.String())
	}
	if b, _ := os.ReadFile(stdin); strings.TrimSpace(string(b)) != "not-a-tty" {
		t.Fatalf("adapter fd0: %q", b)
	}
}

func TestAdapterReadyLimit(t *testing.T) {
	// One bound for either scope (#766): the model-scoped 45 s only held on a
	// quiet machine, and a loaded host killed every --model run with it. The
	// env is emptied first so the default never reads the caller's own value.
	t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", "")
	if got := adapterReadyLimit(); got != 600*time.Second {
		t.Errorf("default limit %v, want 600s", got)
	}
	// A caller on a loaded host may wait longer; anything unusable keeps the
	// default rather than arming a zero or negative deadline — including a
	// count whose nanoseconds would not fit a duration (#9223372036 = 292
	// years), which would wrap negative and abort the very first poll.
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"900", 900 * time.Second},
		{"9223372036", 9223372036 * time.Second},
		{"9223372037", 600 * time.Second},
		{"99999999999999999999999", 600 * time.Second},
		{"0", 600 * time.Second},
		{"-5", 600 * time.Second},
		{"soon", 600 * time.Second},
		{"", 600 * time.Second},
	} {
		t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", tc.env)
		if got := adapterReadyLimit(); got != tc.want {
			t.Errorf("env %q: limit %v, want %v", tc.env, got, tc.want)
		}
	}
}

func TestAdapterStartModelScopeHonorsTheEnvBoundAndSaysTheWait(t *testing.T) {
	// #766: a --model-scoped start ignored any bound the caller passed and
	// died at a fixed 45 s; it now waits the bound and the failure line says
	// how long it actually waited. The clock here is the test's, not the
	// machine's: a starved gate host can run the first poll seconds late, and
	// a wall-clock ceiling read that starvation as an ignored bound (#895).
	// The poll lands 11 s late in virtual time — the pause that failed the old
	// ceiling — and a second tick past the default proves it was the 1 s env
	// bound, not the 600 s default, that ended the wait.
	t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", "1")
	core := fakeCore(t, "exec sleep 30") // never answers; exec keeps it killable

	var mu sync.Mutex
	now := time.Unix(0, 0)
	tick := make(chan time.Time, 2)
	reads := make(chan struct{}, 8)
	started := make(chan struct{})
	savedNow, savedTicker := adapterNow, adapterTicker
	t.Cleanup(func() { adapterNow, adapterTicker = savedNow, savedTicker })
	adapterNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		reads <- struct{}{}
		return now
	}
	adapterTicker = func() (<-chan time.Time, func()) {
		close(started)
		return tick, func() {}
	}
	advance := func(to time.Duration) {
		mu.Lock()
		now = time.Unix(0, 0).Add(to)
		mu.Unlock()
		tick <- time.Time{}
	}

	var out, errw bytes.Buffer
	result := make(chan int, 1)
	go func() {
		result <- adapterStart([]string{core, "p", "llmgate/claude-opus-5-5", "1", os.DevNull}, nil, &out, &errw)
	}()
	select {
	case <-started:
	case rc := <-result:
		t.Fatalf("rc %d before the first poll, stderr %q", rc, errw.String())
	}
	<-reads // the loop's start, read before it made the poll
	advance(11 * time.Second)
	// Wait for the poll's own reading so the next advance cannot change the
	// decision it is about to make; a loop that ignored the env reaches this
	// read and keeps waiting, and the 601 s tick then ends it.
	<-reads
	advance(601 * time.Second)
	if rc := <-result; rc != 1 {
		t.Fatalf("rc %d, stderr %q", rc, errw.String())
	}
	timedOut := regexp.MustCompile(`: adapter timed out after ([0-9]+)s; session not started\.`).FindStringSubmatch(errw.String())
	if timedOut == nil {
		t.Fatalf("stderr %q, want a timeout naming the measured wait in seconds", errw.String())
	}
	if timedOut[1] != "11" {
		t.Errorf("stderr %q reports %ss, want the 11s the controlled clock measured", errw.String(), timedOut[1])
	}
}

func TestAdapterStartSaysWhyItFailed(t *testing.T) {
	// Every failure exits 1, a refusal included: the Refused line on stdout is
	// what tells the launcher the adapter worded the whole message itself, so
	// it adds nothing to it, where a bare death still gets the route checks
	// (#374). A child dying with the refusal's own status is still a bare
	// death: only the EXIT line refuses.
	for _, tc := range []struct {
		body    string
		want    string
		refused bool
	}{
		{"echo 'EXIT=no route for p'", "no route for p", true},
		{"exit 3", "exited before readiness", false},
	} {
		var out, errw bytes.Buffer
		if rc := adapterStart([]string{fakeCore(t, tc.body), "p", "m", "1", os.DevNull}, nil, &out, &errw); rc != 1 || !strings.Contains(errw.String(), tc.want) || tc.refused != strings.Contains(out.String(), Refused) {
			t.Errorf("%q: rc %d, out %q, stderr %q", tc.body, rc, out.String(), errw.String())
		}
	}
}
