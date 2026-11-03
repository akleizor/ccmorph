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
		{true, []string{"--resume", "abc"}, LaunchFlags{Resume: "abc"}},
		{false, []string{"--resume", "abc"}, LaunchFlags{Resume: "abc", SkipModel: true}},
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
