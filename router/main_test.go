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

// TestShowANamedPickHasNoEmptyClass pins #636: a caller-named profile+model
// fills no Class or Slack, and the old line printed `·  · slack 0.0`, which
// reads like a real reading.
func TestShowANamedPickHasNoEmptyClass(t *testing.T) {
	var b bytes.Buffer
	show(&b, Decision{Task: Task{Kind: "code", Tier: "mid"}, Pick: &Option{Profile: "demo", Model: "m", Effort: "medium"}}, false)
	got := b.String()
	if !strings.Contains(got, "named by the caller") {
		t.Fatalf("a named pick does not say who chose it: %q", got)
	}
	if strings.Contains(got, "· ·") || strings.Contains(got, "slack") {
		t.Fatalf("an empty class still prints a slack reading: %q", got)
	}
	// A routed pick keeps its reading.
	b.Reset()
	show(&b, Decision{Task: Task{Kind: "code", Tier: "mid"}, Pick: &Option{Profile: "demo", Model: "m", Effort: "medium", Class: "api", Slack: 12.5}}, false)
	if !strings.Contains(b.String(), "api · slack 12.5 points") {
		t.Fatalf("a routed pick lost its class or slack: %q", b.String())
	}
}

// TestReportNamesTheEmptyState pins #636: a fresh install's `router report`
// printed only a header, which reads as a broken command.
func TestReportNamesTheEmptyState(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	var out, errOut bytes.Buffer
	if code := Main([]string{"report"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "No profiles and no routed calls") || !strings.Contains(out.String(), "ccmorph add <name>") {
		t.Fatalf("the empty report names no state or next step: %q", out.String())
	}
	if strings.Contains(out.String(), "profile      calls") {
		t.Fatalf("the empty report printed a table header: %q", out.String())
	}
	oneProfile(t)
	out.Reset()
	if code := Main([]string{"report"}, strings.NewReader(""), &out, io.Discard); code == 0 || !strings.Contains(out.String(), "profile      calls") {
		t.Fatalf("a profile present must keep the table (and its failures): %q, exit %d", out.String(), code)
	}
}

// TestStatusNamesTheLegendAndTheNextStep pins #636: status named pace, class,
// kind and tier nowhere, and ended with no next step.
func TestStatusNamesTheLegendAndTheNextStep(t *testing.T) {
	oneProfile(t)
	var out bytes.Buffer
	if code := Main([]string{"status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"slack = plan time elapsed minus usage", "Next: ccmorph router on"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status misses %q: %q", want, out.String())
		}
	}
	if code := Main([]string{"on"}, strings.NewReader(""), io.Discard, io.Discard); code != 0 {
		t.Fatal("router on failed")
	}
	out.Reset()
	if code := Main([]string{"status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "Next: ccmorph router dispatch --dry-run") {
		t.Fatalf("status on names no next step: %q", out.String())
	}
}

// TestOnNamesThePoolAndThePaidKeys pins #636: `router on` warned about neither
// eligible profiles nor per-token billing.
func TestOnNamesThePoolAndThePaidKeys(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	for name, conf := range map[string]string{
		"sub": "type=sub\n",
		"gw":  "type=apikey\nbase_url=https://gw.invalid/v1\napi_key=k\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := Main([]string{"on"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	if !strings.Contains(got, "Eligible now:") || !strings.Contains(got, "gw") {
		t.Fatalf("router on names no pool: %q", got)
	}
	if !strings.Contains(got, "API-key profiles bill per token: gw") {
		t.Fatalf("router on hides the paid key: %q", got)
	}
	empty := t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", empty)
	out.Reset()
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := Main([]string{"on"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "Eligible now: none") || strings.Contains(out.String(), "bill per token") {
		t.Fatalf("an empty pool is not named, or a billing line appears with no paid key: %q", out.String())
	}
	// #636: the confirmation names its undo, with or without profiles.
	if !strings.Contains(out.String(), "Undo: ccmorph router off") || !strings.Contains(got, "Undo: ccmorph router off") {
		t.Fatalf("router on names no undo: %q", out.String())
	}
}

// #636: status rows read "no reading" with no header and no reason, so a
// signed-out account and a dead gateway looked alike.
func TestStatusHasAHeaderAndSaysWhyARowHasNoReading(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	for name, conf := range map[string]string{
		"acct": "type=sub\n",
		"gw":   "type=apikey\nbase_url=https://gw.invalid/v1\napi_key=k\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if code := Main([]string{"status"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	got := out.String()
	for _, want := range []string{"profile", "class", "pace", "bills per token, no plan to read"} {
		if !strings.Contains(got, want) {
			t.Errorf("status misses %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "ccmorph quota acct says why") && !strings.Contains(got, "see note") {
		t.Errorf("the account's empty reading names no reason:\n%s", got)
	}
}

// #636: a dispatch with no pick named no next step.
func TestNoPickNamesANextStep(t *testing.T) {
	var out bytes.Buffer
	show(&out, Decision{Task: Task{Kind: "code", Tier: "mid"}}, false)
	if !strings.Contains(out.String(), "Next: ccmorph router status") || !strings.Contains(out.String(), "ccmorph set <profile> model") {
		t.Fatalf("no next step: %q", out.String())
	}
}

// TestNoProfilesNamesANextStep pins #636: dispatch with no profile at all said
// only "no ccmorph profiles", naming nothing to do about it.
func TestNoProfilesNamesANextStep(t *testing.T) {
	t.Setenv("CCMORPH_STATE_DIR", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv(WorkerEnv, "")
	for _, dry := range []bool{true, false} {
		args := []string{"dispatch", "--no-decider", "--kind", "code", "--tier", "mid", "fix it"}
		if dry {
			args = append([]string{"dispatch", "--dry-run"}, args[1:]...)
		}
		var out, errb bytes.Buffer
		if rc := Main(args, strings.NewReader(""), &out, &errb); rc == 0 || !strings.Contains(errb.String(), "Next: ccmorph add <name>") {
			t.Fatalf("dry-run=%v: rc=%d stderr=%q, want non-zero and a next step", dry, rc, errb.String())
		}
	}
}

// TestProfileNamesKeepAGlobCharacterInTheRoot pins #636: the routing pool read
// its confs with a raw glob, so a `[` in the profile root made every profile
// silently vanish from it.
func TestProfileNamesKeepAGlobCharacterInTheRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a[b")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCMORPH_STATE_DIR", root)
	if err := os.WriteFile(filepath.Join(root, "work.conf"), []byte("type=sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := profileNames()
	if len(got) != 1 || got[0] != "work" {
		t.Fatalf("profileNames = %v, want [work]", got)
	}
}
