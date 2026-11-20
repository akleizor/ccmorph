package router

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
