package util

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// virtualStart is one model-scoped adapterStart against a core that never
// answers, run on a clock the test sets instead of the machine's (#895).
type virtualStart struct {
	mu     sync.Mutex
	now    time.Time
	tick   chan time.Time
	reads  chan struct{}
	result chan int
	errw   bytes.Buffer
}

// startVirtual swaps adapterNow and adapterTicker for a clock the test moves
// and an unbuffered tick, starts adapterStart on the model its cases use, and
// returns once the loop has read its start time.
func startVirtual(t *testing.T) *virtualStart {
	return startVirtualModel(t, "llmgate/claude-opus-5-5")
}

// startVirtualModel is startVirtual for any model: a case about the progress
// line's label needs the id it names, not the one another case uses.
func startVirtualModel(t *testing.T, model string) *virtualStart {
	t.Helper()
	core := fakeCore(t, "exec sleep 300") // never answers; exec keeps it killable
	v := &virtualStart{
		now:    time.Unix(0, 0),
		tick:   make(chan time.Time), // unbuffered: a send lands only on a loop still waiting
		reads:  make(chan struct{}, 1),
		result: make(chan int, 1),
	}
	started := make(chan struct{})
	savedNow, savedTicker := adapterNow, adapterTicker
	t.Cleanup(func() { adapterNow, adapterTicker = savedNow, savedTicker })
	adapterNow = func() time.Time {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.reads <- struct{}{}
		return v.now
	}
	adapterTicker = func() (<-chan time.Time, func()) {
		close(started)
		return v.tick, func() {}
	}
	var out bytes.Buffer
	go func() {
		v.result <- adapterStart([]string{core, "p", model, "1", os.DevNull}, nil, &out, &v.errw)
	}()
	select {
	case <-started:
	case rc := <-v.result:
		t.Fatalf("rc %d before the first poll, stderr %q", rc, v.errw.String())
	}
	<-v.reads // the loop's start, read before its first poll
	return v
}

// poll sets the clock to at past the start and ticks the loop once. It
// returns the exit status and true when the loop had already returned
// instead of taking the tick; false means the loop read at and decided.
func (v *virtualStart) poll(at time.Duration) (int, bool) {
	v.mu.Lock()
	v.now = time.Unix(0, 0).Add(at)
	v.mu.Unlock()
	select {
	case v.tick <- time.Time{}:
		<-v.reads // the poll's own reading, taken before it decides
		return 0, false
	case rc := <-v.result:
		return rc, true
	}
}

func TestAdapterStartModelScopeHonorsTheEnvBoundAndSaysTheWait(t *testing.T) {
	// #766: a --model-scoped start ignored any bound the caller passed and
	// died at a fixed 45 s; it now waits the bound and the failure line says
	// how long it actually waited. The clock is the test's, not the
	// machine's (#895): a starved gate host runs polls late, and only a
	// controlled clock tells that starvation from an ignored bound.
	t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", "1")
	timedOut := regexp.MustCompile(`: adapter timed out after ([0-9]+)s; session not started\.`)
	t.Run("the env bound ends the wait at its deadline", func(t *testing.T) {
		v := startVirtual(t)
		if rc, done := v.poll(999 * time.Millisecond); done {
			t.Fatalf("rc %d after a 999ms poll, before the 1s bound; stderr %q", rc, v.errw.String())
		}
		if rc, done := v.poll(time.Second); done {
			t.Fatalf("rc %d before the 1s poll; stderr %q", rc, v.errw.String())
		}
		rc, done := v.poll(601 * time.Second)
		if !done {
			t.Fatal("the 1s poll did not end the wait: the CCMORPH_ADAPTER_START_TIMEOUT=1 bound was not the one used")
		}
		m := timedOut.FindStringSubmatch(v.errw.String())
		if rc != 1 || m == nil || m[1] != "1" {
			t.Fatalf("rc %d, stderr %q; want rc 1 and a timeout after 1s", rc, v.errw.String())
		}
	})
	// A starved host polls late; the line reports the measured wait.
	for _, late := range []time.Duration{2 * time.Second, 4 * time.Second, 11 * time.Second} {
		t.Run("a first poll "+late.String()+" late", func(t *testing.T) {
			v := startVirtual(t)
			if rc, done := v.poll(late); done {
				t.Fatalf("rc %d before the first poll; stderr %q", rc, v.errw.String())
			}
			rc, done := v.poll(601 * time.Second)
			if !done {
				t.Fatalf("a poll %s past the start did not end the wait under a 1s bound", late)
			}
			m := timedOut.FindStringSubmatch(v.errw.String())
			if want := strconv.Itoa(int(late / time.Second)); rc != 1 || m == nil || m[1] != want {
				t.Fatalf("rc %d, stderr %q; want rc 1 and a timeout after %ss", rc, v.errw.String(), want)
			}
		})
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

// GH-971: the progress line printed the launch's model with its context
// window marker — "Starting adapter for 'work (claude-sonnet-5-5[1m])'" — an
// internal no other screen shows (`inspect` renders the window as a count).
// The line names the bare id, the same one the adapter's --model gets.
func TestAdapterStartNamesTheBareModelInItsProgressLine(t *testing.T) {
	t.Setenv("CCMORPH_ADAPTER_START_TIMEOUT", "5")
	for _, c := range []struct{ model, label string }{
		{"claude-sonnet-5-5[1m]", "p (claude-sonnet-5-5)"},
		{"llmgate/claude-opus-5-5", "p (llmgate/claude-opus-5-5)"},
		{"", "p"},
	} {
		t.Run(c.label, func(t *testing.T) {
			v := startVirtualModel(t, c.model)
			// The first full second is when the line is said, and the next
			// poll is what makes it visible: the loop only takes that tick
			// once it is back at its select, after the line's write. The bound
			// is five, so neither poll ends the wait.
			for _, at := range []time.Duration{time.Second, 2 * time.Second} {
				if rc, done := v.poll(at); done {
					t.Fatalf("rc %d before the line was due; stderr %q", rc, v.errw.String())
				}
			}
			if got := v.errw.String(); !strings.Contains(got, "Starting adapter for '"+c.label+"'…") {
				t.Fatalf("stderr %q does not name %q", got, c.label)
			}
			if strings.Contains(v.errw.String(), "[1m]") {
				t.Fatalf("stderr carries the window marker: %q", v.errw.String())
			}
			// The poll past the bound ends the wait; the next one collects
			// its status, as the bounded cases above do.
			if rc, done := v.poll(10 * time.Second); done {
				t.Fatalf("rc %d on the poll past the bound", rc)
			}
			if _, done := v.poll(20 * time.Second); !done {
				t.Fatal("the wait did not end past the bound")
			}
		})
	}
}
