package router

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeWorker puts a ccmorph on PATH that runs body, so Delegate's worker meets
// a predictable process without launching a model.
func fakeWorker(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ccmorph"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeGrader answers every decider question with the same pass score; the raw
// answer is what a passing grade needs.
func fakeGrader(t *testing.T, score float64) *Decider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"answers":{"pass":{"noul":%g}}}`, score)
	}))
	t.Cleanup(srv.Close)
	return &Decider{URL: srv.URL}
}

// Delegate has no PostToolUse caller to carry complete()'s hint, so the
// dispatch answer itself must say the run failed quality; a passing answer
// stays byte-identical.
func TestDispatchCarriesTheQualityFailEscalationAndLeavesAPassingAnswerAlone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		score     float64
		escalates bool
		flags     []string
	}{
		{"quality fail", 0.1, true, nil},
		{"pass", 0.9, false, nil},
		{"no decider", 0.1, false, []string{"--no-decider"}}, // the caller judges the outcome itself (harness-doctor)
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, true)
			fakeWorker(t, "printf '%s' 'a decent answer'")
			if err := writeJSON(ConfigPath(), Config{Enabled: true, Decider: fakeGrader(t, tc.score)}); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			args := append(append([]string{"dispatch"}, tc.flags...), "--kind", "code", "--tier", "mid", "fix the bug")
			if code := Main(args, strings.NewReader(""), &out, &errOut); code != 0 {
				t.Fatalf("dispatch exit %d: %s", code, errOut.String())
			}
			if got := out.String(); got != "a decent answer" {
				t.Fatalf("the worker's answer changed: %q", got)
			}
			if strings.Contains(errOut.String(), "Escalate once") != tc.escalates {
				t.Fatalf("escalation note on stderr is %q, want it only on a quality fail", errOut.String())
			}
		})
	}
}

// A first route that dies on quota is retried; when the retry answers, the
// caller still sees the backoff note that tells it the route moved.
func TestDispatchCarriesTheBackoffNoteOfARetriedRoute(t *testing.T) {
	hookEnv(t, true)
	fakeWorker(t, `HERE=${0%/*}
if [ -f "$HERE/ran" ]; then printf '%s' 'a decent answer'; exit 0; fi
: > "$HERE/ran"; printf 'quota exceeded' ; exit 1`)
	var out, errOut bytes.Buffer
	if code := Main([]string{"dispatch", "--kind", "code", "--tier", "mid", "fix the bug"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("dispatch exit %d: %s", code, errOut.String())
	}
	if out.String() != "a decent answer" || !strings.Contains(errOut.String(), "retry the same brief at the same tier") {
		t.Fatalf("the caller never learns the route backed off: out %q err %q", out.String(), errOut.String())
	}
}

// A service dispatches through the router like a person: its claude flags reach
// the worker in order, its task tag stays its own, the worker learns its
// deadline, and --timeout kills the whole run, retry included.
func TestDispatchPassesClaudeFlagsTaskTagAndDeadline(t *testing.T) {
	hookEnv(t, true)
	dir := t.TempDir()
	fakeWorker(t, `printf '%s\n' "$@" > `+dir+`/argv; echo "task=$CCMORPH_TASK deadline=$CCMORPH_DEADLINE"`)
	t.Setenv("CCMORPH_TASK", "log-triage")
	var out, errOut bytes.Buffer
	code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "--timeout", "60", "brief",
		"--", "--tools", "Read", "--max-turns", "1"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	if !strings.Contains(string(argv), "--tools\nRead\n--max-turns\n1\n-p\nbrief\n") {
		t.Fatalf("claude flags did not reach the worker before -p: %q", argv)
	}
	if !strings.Contains(out.String(), "task=log-triage deadline=1") {
		t.Fatalf("the worker lost the caller's tag or its deadline: %q", out.String())
	}
	for _, bad := range []string{"--model", "--effort=high", "-p"} {
		if Main([]string{"dispatch", "x", "--", bad, "y"}, strings.NewReader(""), io.Discard, io.Discard) != 2 {
			t.Errorf("%s after -- is the dispatch's own and must be refused", bad)
		}
	}
	for _, bad := range []string{"0", "-1", "NaN", "Inf", "x"} {
		if Main([]string{"dispatch", "--timeout", bad, "x"}, strings.NewReader(""), io.Discard, io.Discard) != 2 {
			t.Errorf("--timeout %s must be refused", bad)
		}
	}
}

func TestDispatchTimeoutKillsTheWorkerAndNeverRetries(t *testing.T) {
	hookEnv(t, true)
	dir := t.TempDir()
	fakeWorker(t, `echo run >> `+dir+`/runs; exec sleep 30`)
	timeout := time.Second
	start := time.Now()
	var errOut bytes.Buffer
	code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "--timeout", "1", "brief"}, strings.NewReader(""), io.Discard, &errOut)
	elapsed := time.Since(start)
	// The bound is a backstop for a kill that never came — the fake worker
	// would then sleep out its 30 s — and takes its margin from the timeout:
	// a loaded machine spends seconds on post-deadline bookkeeping (#874).
	if code != 124 || elapsed > 10*timeout || !strings.Contains(errOut.String(), "timeout (exit 124)") {
		t.Fatalf("exit %d after %s: %q", code, elapsed, errOut.String())
	}
	// A timed-out worker is never run again. The retry's signature is a
	// second record; zero is legitimate — the kill can land between the
	// dispatch launching the worker and its shell recording the run, which
	// is exactly how the old != 1 check failed on a loaded machine (#874).
	runs, _ := os.ReadFile(filepath.Join(dir, "runs"))
	if n := strings.Count(string(runs), "run"); n > 1 {
		t.Fatalf("a timed-out run was run again: %d records %q", n, runs)
	}
}

// A model that answered with a failure is not run again: only the account's
// refusal moves the work. The worker's own words reach the caller.
func TestDispatchRetriesOnlyAnAccountRefusalAndKeepsTheWorkersWords(t *testing.T) {
	hookEnv(t, true)
	dir := t.TempDir()
	fakeWorker(t, `echo run >> `+dir+`/runs; echo '{"is_error":true,"session_id":"s1"}'; exit 1`)
	var out bytes.Buffer
	if code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "brief"}, strings.NewReader(""), &out, io.Discard); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("a model's own failure was paid for twice: %q", runs)
	}
	if out.String() != `{"is_error":true,"session_id":"s1"}`+"\n" {
		t.Fatalf("a failed worker's stdout must reach the caller unchanged: %q", out.String())
	}
}

// A quota refusal after the worker ran a tool is not retried: the second run
// would repeat what the first already did (#759).
func TestDispatchNeverRetriesAWorkerThatRanATool(t *testing.T) {
	hookEnv(t, true)
	dir := t.TempDir()
	fakeWorker(t, `echo run >> `+dir+`/runs
echo '{"type":"assistant","message":{"content":[{"type":"tool_use"}]}}'
echo '{"type":"result","is_error":true,"result":"quota exceeded"}'; exit 1`)
	var out bytes.Buffer
	if code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "brief"}, strings.NewReader(""), &out, io.Discard); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("a worker that acted was run again: %q", runs)
	}
	if out.String() != "quota exceeded" {
		t.Fatalf("the caller gets the result text as plain -p prints it: %q", out.String())
	}
}

// A decider that never answers cannot hold a dispatch past its deadline (#759).
func TestDispatchDeadlineBoundsRouting(t *testing.T) {
	hookEnv(t, true)
	fakeWorker(t, `echo ran`)
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(func() { close(block); srv.Close() })
	if err := writeJSON(ConfigPath(), Config{Enabled: true, Decider: &Decider{URL: srv.URL}}); err != nil {
		t.Fatal(err)
	}
	timeout := 300 * time.Millisecond
	start := time.Now()
	var out, errOut bytes.Buffer
	code := Main([]string{"dispatch", "--timeout", "0.3", "fix the bug"}, strings.NewReader(""), &out, &errOut)
	elapsed := time.Since(start)
	// The promise is a routing phase, not a wall clock: every request to
	// this decider is parked in its handler until the cleanup registered
	// above, which runs after these assertions, so a dispatch that returned
	// at all was cut off by its own deadline, never answered. The bound
	// only backstops a dispatch that ignores the deadline wholesale, and
	// takes its margin from the deadline: the post-deadline bookkeeping
	// alone ran to 3.5 s on a load-170 machine, tripping the literal 3 s
	// this replaces (#874).
	if code != 124 || out.String() != "" || elapsed > 60*timeout ||
		!strings.Contains(errOut.String(), "ran past the deadline") {
		t.Fatalf("exit %d after %s, out %q, err %q", code, elapsed, out.String(), errOut.String())
	}
}

func TestDispatchRefusesASecondSeparatorAndAPrintBundle(t *testing.T) {
	for _, bad := range []string{"--", "-cp", "-pc"} {
		if Main([]string{"dispatch", "x", "--", "--tools", "Read", bad}, strings.NewReader(""), io.Discard, io.Discard) != 2 {
			t.Errorf("%q after -- must be refused", bad)
		}
	}
}
