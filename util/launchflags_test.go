package util

import (
	"reflect"
	"strings"
	"testing"
)

func TestLaunchFlags(t *testing.T) {
	arity := map[string]string{"--system-prompt": "value", "-d": "optional", "--debug": "optional"}
	for _, c := range []struct {
		routed bool
		argv   []string
		want   LaunchFlags
	}{
		{false, nil, LaunchFlags{AskMissingModel: true}},
		{false, []string{"--model", "m1"}, LaunchFlags{Model: "m1", UserModel: true, SkipModel: true, AskMissingModel: true}},
		{true, []string{"--resume", "abc"}, LaunchFlags{Resume: "abc", Resuming: true}},
		{false, []string{"--resume", "abc"}, LaunchFlags{Resume: "abc", Resuming: true, SkipModel: true}},
		// Every resume spelling marks the launch as resuming (#356): the saved
		// effort is a default for new conversations only.
		{true, []string{"--resume"}, LaunchFlags{Resuming: true}},
		{false, []string{"--resume=abc"}, LaunchFlags{Resume: "abc", Resuming: true, SkipModel: true}},
		// The = spelling carries its value inside the token, empty or not: the
		// last selector wins, so a final --resume= clears the session named
		// before it (a routed launch reads Resume to pick its model, run.go).
		{false, []string{"--resume=abc", "--resume="}, LaunchFlags{Resuming: true, SkipModel: true}},
		{true, []string{"--resume", "abc", "--resume="}, LaunchFlags{Resuming: true}},
		{false, []string{"-c"}, LaunchFlags{Resuming: true, SkipModel: true}},
		{false, []string{"--continue"}, LaunchFlags{Resuming: true, SkipModel: true}},
		{false, []string{"--from-pr", "12"}, LaunchFlags{Resuming: true, SkipModel: true}},
		{false, []string{"--from-pr=12"}, LaunchFlags{Resuming: true}},
		// #356: a typed effort is the person's own decision; a bare --effort stays
		// on the argv for Claude Code to judge, and a flag-shaped token is its
		// value (#334), never an option.
		{false, []string{"--effort", "xhigh"}, LaunchFlags{Effort: "xhigh", UserEffort: true, AskMissingModel: true}},
		{false, []string{"--effort=low"}, LaunchFlags{Effort: "low", UserEffort: true, AskMissingModel: true}},
		{false, []string{"--effort"}, LaunchFlags{UserEffort: true, AskMissingModel: true}},
		{false, []string{"--effort="}, LaunchFlags{UserEffort: true, AskMissingModel: true}},
		{false, []string{"--effort", "--continue"}, LaunchFlags{Effort: "--continue", UserEffort: true, AskMissingModel: true}},
		// #334: a value that looks like a flag is the option's value, not a resume.
		{false, []string{"--system-prompt", "--continue"}, LaunchFlags{AskMissingModel: true}},
		{false, []string{"-p", "hi", "--dangerously-skip-permissions"}, LaunchFlags{Relay: []string{"--dangerously-skip-permissions"}}},
		{true, []string{"x", "--model", "-i"}, LaunchFlags{AskMissingModel: true, Interactive: true, Drop: []int{2}}},
		{false, []string{"--", "--model", "m"}, LaunchFlags{}},
	} {
		got, err := ParseLaunchFlags(c.routed, c.argv, arity)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: err=%v\n got %+v\nwant %+v", c.argv, err, got, c.want)
		}
	}
	if _, err := ParseLaunchFlags(false, []string{"--model"}, nil); err == nil || err.Error() != "--model requires a value" {
		t.Fatalf("missing value: %v", err)
	}
}

func TestLaunchArgvClaimsOnlyRunsOwnFlags(t *testing.T) {
	for _, c := range []struct {
		args        []string
		profile     string
		interactive bool
		fwd         []string
	}{
		{[]string{"-a", "gw", "-i", "-p", ""}, "gw", true, []string{"-p", ""}},
		{[]string{"--system-prompt", "--profile", "--profile=x"}, "x", false, []string{"--system-prompt", "--profile"}},
		// The grammar's own --model owns its value with no arity table to say
		// so (#490): -i is the pair's value, forwarded, not the interactive flag.
		{[]string{"--model", "-i"}, "", false, []string{"--model", "-i"}},
		// The same rule one level deeper: the value --model owns is itself a
		// native option, so the claim after it stays a claim — the flags walk
		// has always read --model's value this way (#334, #490).
		{[]string{"--model", "--system-prompt", "-i"}, "", true, []string{"--model", "--system-prompt"}},
		{[]string{"--", "-a", "gw"}, "", false, []string{"--", "-a", "gw"}},
	} {
		got, err := ParseLaunchArgv(c.args, map[string]string{"--system-prompt": "value"})
		if err != nil || got.Profile != c.profile || got.Interactive != c.interactive || strings.Join(got.Forward, "|") != strings.Join(c.fwd, "|") || len(got.Forward) != len(c.fwd) {
			t.Errorf("%v: got %+v %v", c.args, got, err)
		}
	}
	if _, err := ParseLaunchArgv([]string{"-a", "x", "-a", "y"}, nil); err == nil || !strings.Contains(err.Error(), "only once") {
		t.Errorf("duplicate -a: %v", err)
	}
}

// A value an option owns is its value whatever it looks like (#334); arity comes
// from the installed binary's --help, so invented options are proof of discovery.
func TestOptionStep(t *testing.T) {
	arity := map[string]string{"--invented-value": "value", "--invented-flag": "flag", "-z": "optional", "--invented-optional": "optional"}
	for _, c := range []struct {
		opt, next string
		want      int
	}{
		{"--invented-value", "--continue", 2},
		// The -- separator is never an option's value: it ends the scan, so
		// the tokens after it stay positionals (#405, #490).
		{"--invented-value", "--", 1},
		{"--invented-flag", "x", 1},
		{"-z", "x", 2},
		{"--invented-optional", "-p", 1},
		{"--invented-value=v", "x", 1},
		{"--no-such-option", "x", 0},
	} {
		if got := optionStep(arity, c.opt, c.next, true); got != c.want {
			t.Errorf("optionStep(%q, %q) = %d, want %d", c.opt, c.next, got, c.want)
		}
	}
}

// The exemption the native key check grants a launch that opens no session (#255)
// reads argv with ParseLaunchFlags' own walk: an option's flag-shaped value is
// its value, never a version request (#334).
func TestOpensNoSession(t *testing.T) {
	arity := map[string]string{"--system-prompt": "value", "-z": "optional"}
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"--version"}, true},
		{[]string{"-v"}, true},
		{[]string{"prompt", "--help"}, true},
		{nil, false},
		{[]string{"-p", "hello"}, false},
		// A flag-shaped value is the value --system-prompt owns.
		{[]string{"--system-prompt", "--version", "-p", "hello"}, false},
		{[]string{"--system-prompt=--version"}, false},
		// --model owns its value even with no arity table to say so.
		{[]string{"--model", "--version"}, false},
		// So does --effort (#356); its = spelling carries the value inside.
		{[]string{"--effort", "--version"}, false},
		{[]string{"--effort=xhigh", "--version"}, true},
		// An optional value never takes a flag-shaped token; -p owns nothing.
		{[]string{"--resume", "--version"}, true},
		{[]string{"--resume", "abc", "--version"}, true},
		{[]string{"-z", "--version"}, true},
		{[]string{"-p", "--version"}, true},
		// After -- every token is a positional, even a version spelling.
		{[]string{"--", "--version"}, false},
	} {
		if got := OpensNoSession(c.argv, arity); got != c.want {
			t.Errorf("OpensNoSession(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}
