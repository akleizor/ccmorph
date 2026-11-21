package router

import (
	"bytes"
	"io"
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

func TestMainRefusesAWordItDoesNotTakeBeforeActing(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"status", "--bogus"}, "unknown option '--bogus'"},
		{[]string{"status", "extra9"}, "unexpected argument 'extra9'"},
		{[]string{"--bogus"}, "unknown option '--bogus'"},
		{[]string{"bogus"}, "unexpected argument 'bogus'"},
		{[]string{"on", "extra9"}, "unexpected argument 'extra9'"},
		{[]string{"on", "--bogus"}, "unknown option '--bogus'"},
		{[]string{"on", "--help"}, "unknown option '--help'"},
		{[]string{"off", "extra9"}, "unexpected argument 'extra9'"},
		{[]string{"off", "-h"}, "unknown option '-h'"},
	} {
		for _, enabled := range []bool{false, true} {
			name := strings.Join(c.args, "_") + map[bool]string{false: "_while_off", true: "_while_on"}[enabled]
			t.Run(name, func(t *testing.T) {
				oneProfile(t)
				if err := SetEnabled(enabled); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(ConfigPath())
				if err != nil {
					t.Fatal(err)
				}
				var out, errOut bytes.Buffer
				if code := Main(c.args, strings.NewReader(""), &out, &errOut); code != 2 {
					t.Fatalf("exit %d, want 2; stdout=%q stderr=%q", code, out.String(), errOut.String())
				}
				if !strings.Contains(errOut.String(), c.want) || !strings.Contains(errOut.String(), "usage: "+Usage) {
					t.Fatalf("stderr %q does not name %q with the usage line", errOut.String(), c.want)
				}
				if out.Len() != 0 {
					t.Fatalf("printed %q", out.String())
				}
				if after, _ := os.ReadFile(ConfigPath()); string(after) != string(before) {
					t.Fatalf("router.json changed: %q -> %q", before, after)
				}
			})
		}
	}
}

func TestMainRefusedOnCreatesNoRouterConfig(t *testing.T) {
	oneProfile(t)
	if code := Main([]string{"on", "extra9"}, strings.NewReader(""), io.Discard, io.Discard); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if _, err := os.Stat(ConfigPath()); !os.IsNotExist(err) {
		t.Fatalf("created: %v", err)
	}
}

func TestMainOnOffAndStatusStillAct(t *testing.T) {
	oneProfile(t)
	for _, c := range []struct {
		args []string
		want string
	}{{[]string{"on"}, "Router on."}, {[]string{"status"}, "Router on ("}, {[]string{"off"}, "Router off."}, {nil, "Router off ("}} {
		var out bytes.Buffer
		if code := Main(c.args, strings.NewReader(""), &out, io.Discard); code != 0 || !strings.HasPrefix(out.String(), c.want) {
			t.Fatalf("%q: exit %d, stdout %q", c.args, code, out.String())
		}
	}
}

func TestMainHookStillNeverFails(t *testing.T) {
	oneProfile(t)
	for _, args := range [][]string{{"hook", "pre", "extra9"}, {"hook", "bogus"}, {"hook"}} {
		var errOut bytes.Buffer
		if code := Main(args, strings.NewReader(""), io.Discard, &errOut); code != 0 || errOut.Len() != 0 {
			t.Errorf("%q: exit %d, stderr %q", args, code, errOut.String())
		}
	}
}

func TestReportRefusesAWordItDoesNotTake(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "unknown option '--bogus'"},
		{[]string{"extra9"}, "unexpected argument 'extra9'"},
		{[]string{"--since", "1h", "--bogus"}, "unknown option '--bogus'"},
		{[]string{"--since"}, "--since needs a value"},
		{[]string{"--since", "--bogus"}, "--since needs a value, not the option '--bogus'"},
	} {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			oneProfile(t)
			var out, errOut bytes.Buffer
			if code := Main(append([]string{"report"}, c.args...), strings.NewReader(""), &out, &errOut); code != 2 {
				t.Fatalf("exit %d; %q %q", code, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), c.want) || !strings.Contains(errOut.String(), "usage: "+Usage) || out.Len() != 0 {
				t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
			}
		})
	}
}

func TestDispatchRefusesAnUnknownFlagBeforeRouting(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus", "fix the parser"}, "unknown option '--bogus'"},
		{[]string{"fix the parser", "--bogus"}, "unknown option '--bogus'"},
		{[]string{"--tier=top", "fix the parser"}, "unknown option '--tier=top'"},
		{[]string{"--dry-run", "-x", "fix the parser"}, "unknown option '-x'"},
		{[]string{"fix the parser", "--tier"}, "--tier needs a value"},
		{[]string{"--kind"}, "--kind needs a value"},
		{[]string{"fix the parser", "--cwd"}, "--cwd needs a value"},
		{[]string{"--profile", "--dry-run", "fix the parser"}, "--profile needs a value, not the option '--dry-run'"},
		{[]string{"--model", "--", "--tools", "Read"}, "--model needs a value"},
		{[]string{"--timeout", "--bogus", "fix the parser"}, "--timeout needs a value, not the option '--bogus'"},
		{[]string{"fix the parser", "--timeout"}, "--timeout needs a value"},
		{[]string{"--timeout", "--", "fix the parser"}, "--timeout needs a value"},
		{[]string{"--timeout", "x", "fix the parser"}, "--timeout has invalid value \"x\""},
	} {
		t.Run(strings.Join(c.args, "_"), func(t *testing.T) {
			oneProfile(t)
			var out, errOut bytes.Buffer
			if code := Main(append([]string{"dispatch"}, c.args...), strings.NewReader("brief on stdin"), &out, &errOut); code != 2 {
				t.Fatalf("exit %d; %q %q", code, out.String(), errOut.String())
			}
			if !strings.Contains(errOut.String(), c.want) || !strings.Contains(errOut.String(), "usage: "+Usage) || out.Len() != 0 {
				t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
			}
			if _, err := os.Stat(ledgerPath()); !os.IsNotExist(err) {
				t.Fatalf("ledger: %v", err)
			}
		})
	}
}
