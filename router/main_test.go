package router

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/quota"
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

// GH-971: `dispatch` with no task read stdin silently — a terminal waited for
// keys nobody was asked to type, and an EOF routed an empty prompt and printed
// a real pick as if it had been asked. A missing task now names itself with the
// usage line before anything is read, reserved or routed.
func TestDispatchWithoutATaskIsRefusedBeforeRouting(t *testing.T) {
	for _, c := range []struct {
		name  string
		args  []string
		stdin string
	}{
		{"no words, empty stdin", nil, ""},
		{"whitespace-only stdin", nil, " \n\t\n"},
		{"an empty word", []string{""}, ""},
		{"a whitespace-only word", []string{"   "}, ""},
		{"--dry-run", []string{"--dry-run"}, ""},
		{"--dry-run --json", []string{"--dry-run", "--json"}, ""},
		{"workers flags after the delimiter", []string{"--", "--tools", "Read"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			dispatchEnv(t)
			var out, errOut bytes.Buffer
			code := Main(append([]string{"dispatch"}, c.args...), strings.NewReader(c.stdin), &out, &errOut)
			if code != 2 {
				t.Fatalf("exit %d, want 2; stdout=%q stderr=%q", code, out.String(), errOut.String())
			}
			if out.Len() != 0 {
				t.Fatalf("printed a pick for a task nobody gave: %q", out.String())
			}
			if !strings.Contains(errOut.String(), "ccmorph: router: dispatch needs a task: give it as words or on stdin") ||
				!strings.Contains(errOut.String(), "usage: "+Usage) {
				t.Fatalf("stderr %q does not name the missing task with the usage line", errOut.String())
			}
			if rows := readRows(ledgerPath()); len(rows) != 0 {
				t.Fatalf("an empty task reserved a route: %+v", rows)
			}
		})
	}

	// The terminal is never read: a reader that would block forever is left
	// alone, and the refusal is immediate.
	t.Run("a terminal is never read", func(t *testing.T) {
		dispatchEnv(t)
		saved := stdinIsTerminal
		stdinIsTerminal = func(io.Reader) bool { return true }
		t.Cleanup(func() { stdinIsTerminal = saved })
		pr, pw, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer pr.Close()
		defer pw.Close()
		var out, errOut bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- Main([]string{"dispatch"}, pr, &out, &errOut) }()
		select {
		case code := <-done:
			if code != 2 {
				t.Fatalf("exit %d, want 2; stderr=%q", code, errOut.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch blocked reading a terminal")
		}
	})

	// A brief that is on stdin still routes: the refusal is the empty task's.
	t.Run("a piped brief still routes", func(t *testing.T) {
		dispatchEnv(t)
		var out, errOut bytes.Buffer
		code := Main([]string{"dispatch", "--dry-run", "--kind", "code", "--tier", "mid"},
			strings.NewReader("fix the parser"), &out, &errOut)
		if code != 0 || !strings.HasPrefix(out.String(), "→ ") {
			t.Fatalf("a piped brief did not route: exit %d out %q err %q", code, out.String(), errOut.String())
		}
	})
}

// GH-971: status printed "no reading" for every profile without a pace, with
// no gloss. A person new to the router could not tell that "no quota reading
// yet" — nothing has been fetched — was meant, and a profile the router never
// paces at all (an API key, a free or disabled class) got the same words. The
// line now says which of the two it is.
func TestStatusNamesWhyAProfileHasNoPace(t *testing.T) {
	statusOf := func(t *testing.T) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if code := status(&out, &errOut); code != 0 {
			t.Fatalf("status exit %d: %s", code, errOut.String())
		}
		return out.String()
	}
	writeConfs := func(t *testing.T, confs map[string]string) {
		t.Helper()
		root := t.TempDir()
		t.Setenv("CCMORPH_STATE_DIR", root)
		t.Setenv("XDG_STATE_HOME", t.TempDir())
		for name, conf := range confs {
			if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte(conf), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("a subscription with no reading yet", func(t *testing.T) {
		writeConfs(t, map[string]string{"sub": "type=sub\n"})
		out := statusOf(t)
		if !strings.Contains(out, "no quota reading yet") || strings.Contains(out, "no reading") {
			t.Fatalf("status %q does not gloss the missing reading", out)
		}
	})

	t.Run("a budget with no priced spend yet", func(t *testing.T) {
		writeConfs(t, map[string]string{"b": "type=apikey\nbase_url=https://example.test\nmonthly_budget_usd=20\n"})
		out := statusOf(t)
		if !strings.Contains(out, "no quota reading yet") {
			t.Fatalf("status %q does not gloss the budget's missing reading", out)
		}
	})

	t.Run("an API key is not paced", func(t *testing.T) {
		writeConfs(t, map[string]string{"gw": "type=apikey\nbase_url=https://example.test\nmodel=m\n"})
		out := statusOf(t)
		if !strings.Contains(out, "not quota-paced") {
			t.Fatalf("status %q does not say the API key is not paced", out)
		}
	})

	t.Run("a free class is not paced", func(t *testing.T) {
		writeConfs(t, map[string]string{"sub": "type=sub\n"})
		if err := writeJSON(ConfigPath(), Config{Profiles: map[string]ProfileConfig{"sub": {Class: "free"}}}); err != nil {
			t.Fatal(err)
		}
		out := statusOf(t)
		if !strings.Contains(out, "not quota-paced") || strings.Contains(out, "no quota reading yet") {
			t.Fatalf("status %q does not say a free profile is not paced", out)
		}
	})

	t.Run("a subscription with a reading keeps its slack", func(t *testing.T) {
		writeConfs(t, map[string]string{"sub": "type=sub\n"})
		now := int64(nowSec())
		used, reset := 0.5, now+3600
		if err := writeJSON(filepath.Join(routerDir(), "quota.json"), snapshot{ReadAt: nowSec(),
			Entries: []quota.Entry{{Profile: "sub", Windows: []quota.Window{{ID: "5h", Used: &used, ResetsAt: &reset}}}}}); err != nil {
			t.Fatal(err)
		}
		out := statusOf(t)
		if !regexp.MustCompile(`slack [+-][0-9.]+ points`).MatchString(out) {
			t.Fatalf("status %q lost the slack reading", out)
		}
		if strings.Contains(out, "no quota reading yet") {
			t.Fatalf("status %q glosses a profile that has a reading", out)
		}
	})

	t.Run("a mark follows the pace words", func(t *testing.T) {
		writeConfs(t, map[string]string{"sub": "type=sub\n"})
		if err := writeJSON(ConfigPath(), Config{Profiles: map[string]ProfileConfig{"sub": {Reserved: true}}}); err != nil {
			t.Fatal(err)
		}
		out := statusOf(t)
		if !strings.Contains(out, "no quota reading yet reserved") {
			t.Fatalf("status %q does not keep the reserved mark after the pace", out)
		}
	})

	t.Run("no line ends on the bare words", func(t *testing.T) {
		writeConfs(t, map[string]string{"sub": "type=sub\n", "gw": "type=apikey\nbase_url=https://example.test\nmodel=m\n"})
		for _, line := range strings.Split(statusOf(t), "\n") {
			if strings.HasSuffix(line, " no reading") {
				t.Fatalf("status still prints the bare 'no reading': %q", line)
			}
		}
	})
}
