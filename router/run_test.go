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
	}{
		{"quality fail", 0.1, true},
		{"pass", 0.9, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, true)
			fakeWorker(t, "printf '%s' 'a decent answer'")
			if err := writeJSON(ConfigPath(), Config{Enabled: true, Decider: fakeGrader(t, tc.score)}); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			if code := Main([]string{"dispatch", "--kind", "code", "--tier", "mid", "fix the bug"}, strings.NewReader(""), &out, &errOut); code != 0 {
				t.Fatalf("dispatch exit %d: %s", code, errOut.String())
			}
			got := out.String()
			if !strings.Contains(got, "a decent answer") {
				t.Fatalf("the answer is missing: %q", got)
			}
			if tc.escalates {
				if !strings.Contains(got, "Escalate once") {
					t.Fatalf("the dispatch caller never learns the run failed quality: %q", got)
				}
			} else if got != "a decent answer" {
				t.Fatalf("a passing answer changed: %q", got)
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
	got := out.String()
	if !strings.Contains(got, "a decent answer") || !strings.Contains(got, "retry the same brief at the same tier") {
		t.Fatalf("the caller never learns the route backed off: %q", got)
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
	start := time.Now()
	var out bytes.Buffer
	code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "--timeout", "0.3", "brief"}, strings.NewReader(""), &out, io.Discard)
	if code != 1 || time.Since(start) > 5*time.Second || !strings.Contains(out.String(), "timeout (exit 124)") {
		t.Fatalf("exit %d after %s: %q", code, time.Since(start), out.String())
	}
	if runs, _ := os.ReadFile(filepath.Join(dir, "runs")); strings.Count(string(runs), "run") != 1 {
		t.Fatalf("a timed-out run was run again: %q", runs)
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
	if !strings.Contains(out.String(), `"session_id":"s1"`) {
		t.Fatalf("the worker's own output is gone: %q", out.String())
	}
}
