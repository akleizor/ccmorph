package util

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	// how long it actually waited.
	t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", "1")
	core := fakeCore(t, "exec sleep 30") // never answers; exec keeps it killable
	var out, errw bytes.Buffer
	started := time.Now()
	rc := adapterStart([]string{core, "p", "llmgate/claude-opus-5-5", "1", os.DevNull}, nil, &out, &errw)
	waited := time.Since(started)
	if rc != 1 {
		t.Fatalf("rc %d, stderr %q", rc, errw.String())
	}
	// The line reports the wait the loop measured (#260), not the literal
	// bound: a gate host under load can take the first poll several seconds
	// late, so pin the timeout branch and its seconds unit, never "1s".
	timedOut := regexp.MustCompile(`: adapter timed out after ([0-9]+)s; session not started\.`).FindStringSubmatch(errw.String())
	if timedOut == nil {
		t.Errorf("stderr %q, want a timeout naming the measured wait in seconds", errw.String())
	} else if said, _ := strconv.Atoi(timedOut[1]); said < 1 || time.Duration(said)*time.Second > waited {
		t.Errorf("stderr %q reports %s waited, measured %v: the line must report the wait it saw", errw.String(), timedOut[1], waited)
	}
	if waited > 10*time.Second {
		t.Errorf("env bound ignored: waited %v for a 1s bound", waited)
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
