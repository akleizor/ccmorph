package router

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// oneProfile: an isolated state home with one subscription profile and no
// quota reading, so only the ledger can fail the report.
func oneProfile(t *testing.T) {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.WriteFile(filepath.Join(root, "here.conf"), []byte("type=sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Every outcome complete() writes except completed is a failed route; a window
// of timeouts or quota refusals must not pass as healthy.
func TestReportCountsEveryOutcomeButCompletedAsFailed(t *testing.T) {
	for _, outcome := range []string{"timeout", "quota", "auth", "empty", "quality_fail", "error", "completed"} {
		t.Run(outcome, func(t *testing.T) {
			oneProfile(t)
			ledgerAppend(Row{"id": "route-1", "ts": nowSec(), "kind": "code", "tier": "mid",
				"profile": "here", "family": "claude", "outcome": outcome})
			var out, errOut bytes.Buffer
			code := report([]string{"--since", "24h"}, &out, &errOut)
			if outcome == "completed" {
				if code != 0 {
					t.Fatalf("a completed route fails the report: %s", out.String())
				}
				return
			}
			if code == 0 {
				t.Fatalf("a ledger of %s rows passes the report: %s", outcome, out.String())
			}
			if !strings.Contains(out.String(), "1/1 finished routes failed") {
				t.Fatalf("the report does not count %s: %s", outcome, out.String())
			}
		})
	}
}
