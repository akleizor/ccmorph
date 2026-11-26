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

// fakeGrader answers every decider question with the same verdict.
func fakeGrader(t *testing.T, verdict string) *Decider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"answers":{"verdict":{"choice":%q}}}`, verdict)
	}))
	t.Cleanup(srv.Close)
	return &Decider{URL: srv.URL}
}

// Delegate has no PostToolUse caller to carry complete()'s hint, so the
// dispatch answer itself must say the run failed quality; a passing answer
// stays byte-identical. A format problem is recorded apart from a wrong answer
// and is not escalated: the model did the work. The caller's --id names the
// run, and its later acceptance is recorded on that row.
func TestDispatchRecordsTheVerdictEscalatesOnlyAWrongAnswerAndTakesAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name, verdict string
		escalates     bool
		quality       any
		flags         []string
	}{
		{"wrong", "wrong", true, false, nil},
		{"pass", "pass", false, true, nil},
		{"format", "format", false, nil, nil},
		{"no decider", "wrong", false, nil, []string{"--no-decider"}}, // the caller judges the outcome itself (harness-doctor)
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, true)
			fakeWorker(t, "printf '%s' 'a decent answer'")
			if err := writeJSON(ConfigPath(), Config{Enabled: true, Decider: fakeGrader(t, tc.verdict)}); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			args := append(append([]string{"dispatch", "--id", "job-7"}, tc.flags...), "--kind", "code", "--tier", "mid", "fix the bug")
			if code := Main(args, strings.NewReader(""), &out, &errOut); code != 0 {
				t.Fatalf("dispatch exit %d: %s", code, errOut.String())
			}
			if got := out.String(); got != "a decent answer" {
				t.Fatalf("the worker's answer changed: %q", got)
			}
			if strings.Contains(errOut.String(), "Escalate once") != tc.escalates {
				t.Fatalf("escalation note on stderr is %q, want it only on a wrong answer", errOut.String())
			}
			rows := readRows(ledgerPath())
			if len(rows) != 1 {
				t.Fatalf("ledger rows: %v", rows)
			}
			row := rows[0]
			wantVerdict := any(tc.verdict)
			if tc.flags != nil {
				wantVerdict = nil
			}
			if row["verdict"] != wantVerdict || row["quality"] != tc.quality || row["finished"] == nil || row["exit_code"] != 0.0 {
				t.Fatalf("settled row %v, want verdict %v quality %v with finish time and exit", row, wantVerdict, tc.quality)
			}
			out.Reset()
			if code := Main([]string{"accept", "job-7", "pass", "--by", "merged"}, strings.NewReader(""), &out, &errOut); code != 0 {
				t.Fatalf("accept exit %d: %s", code, errOut.String())
			}
			row = readRows(ledgerPath())[0]
			if row["accepted"] != true || row["accepted_by"] != "merged" {
				t.Fatalf("acceptance not recorded: %v", row)
			}
		})
	}
}

// accept settles the newest attempt the caller's id names and leaves the
// older one alone; a second accept replaces the first whole, source included;
// a hook's tool id that equals a caller id never reaches the dispatch row; an
// id the ledger never saw, or a write that fails, is refused, not confirmed.
func TestAcceptSettlesTheNewestAttemptAndNeverClaimsAWriteItLost(t *testing.T) {
	hookEnv(t, true)
	ledgerAppend(Row{"id": "delegate-1", "caller_id": "job-7", "outcome": "error"})
	ledgerAppend(Row{"id": "delegate-2", "caller_id": "job-7", "outcome": "completed"})
	run := func(args ...string) (int, string) {
		var out, errOut bytes.Buffer
		code := Main(append([]string{"accept"}, args...), strings.NewReader(""), &out, &errOut)
		return code, errOut.String()
	}
	if code, err := run("job-7", "pass", "--by", "tests"); code != 0 {
		t.Fatalf("accept exit %d: %s", code, err)
	}
	if code, err := run("job-7", "fail"); code != 0 {
		t.Fatalf("second accept exit %d: %s", code, err)
	}
	rows := readRows(ledgerPath())
	if _, touched := rows[0]["accepted"]; touched {
		t.Fatalf("the older attempt took the acceptance: %v", rows[0])
	}
	if rows[1]["accepted"] != false || rows[1]["accepted_by"] != nil {
		t.Fatalf("the newest attempt keeps the old verdict or its source: %v", rows[1])
	}
	if ledgerUpdate("job-7", Row{"verdict": "wrong"}) {
		t.Fatal("a hook id equal to a caller id settled the dispatch row")
	}
	if code, _ := run("nope", "pass"); code != 1 {
		t.Fatalf("an unknown id was accepted: %d", code)
	}
	if code, _ := run("job-7", "maybe"); code != 2 {
		t.Fatalf("a verdict other than pass or fail was taken: %d", code)
	}
	if err := os.Chmod(routerDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(routerDir(), 0o700) })
	if code, err := run("job-7", "pass"); code != 1 || !strings.Contains(err, "could not record") {
		t.Fatalf("a failed write was confirmed: exit %d, %q", code, err)
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
	start := time.Now()
	var errOut bytes.Buffer
	code := Main([]string{"dispatch", "--kind", "code", "--tier", "small", "--timeout", "1", "brief"}, strings.NewReader(""), io.Discard, &errOut)
	if code != 124 || time.Since(start) > 5*time.Second || !strings.Contains(errOut.String(), "timeout (exit 124)") {
		t.Fatalf("exit %d after %s: %q", code, time.Since(start), errOut.String())
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
	start := time.Now()
	var out bytes.Buffer
	code := Main([]string{"dispatch", "--timeout", "0.3", "fix the bug"}, strings.NewReader(""), &out, io.Discard)
	if code != 124 || time.Since(start) > 3*time.Second || out.String() != "" {
		t.Fatalf("exit %d after %s, out %q", code, time.Since(start), out.String())
	}
}

func TestDispatchRefusesASecondSeparatorAndAPrintBundle(t *testing.T) {
	for _, bad := range []string{"--", "-cp", "-pc"} {
		if Main([]string{"dispatch", "x", "--", "--tools", "Read", bad}, strings.NewReader(""), io.Discard, io.Discard) != 2 {
			t.Errorf("%q after -- must be refused", bad)
		}
	}
}
