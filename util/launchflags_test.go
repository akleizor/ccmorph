package util

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestLaunchFlags(t *testing.T) {
	arity := "--system-prompt value\n-d optional\n--debug optional\n"
	for _, c := range []struct {
		routed string
		argv   []string
		want   string
	}{
		{"0", nil, "model=\nuser_model=0\nskip_model=0\nask_missing_model=1\nresume=\ninteractive=0\n"},
		{"0", []string{"--model", "m1"}, "model=m1\nuser_model=1\nskip_model=1\nask_missing_model=1\nresume=\ninteractive=0\n"},
		{"1", []string{"--resume", "abc"}, "model=\nuser_model=0\nskip_model=0\nask_missing_model=0\nresume=abc\ninteractive=0\n"},
		{"0", []string{"--resume", "abc"}, "model=\nuser_model=0\nskip_model=1\nask_missing_model=0\nresume=abc\ninteractive=0\n"},
		// #334: a value that looks like a flag is the option's value, not a resume.
		{"0", []string{"--system-prompt", "--continue"}, "model=\nuser_model=0\nskip_model=0\nask_missing_model=1\nresume=\ninteractive=0\n"},
		{"0", []string{"-p", "hi", "--dangerously-skip-permissions"}, "model=\nuser_model=0\nskip_model=0\nask_missing_model=0\nresume=\ninteractive=0\nrelay=--dangerously-skip-permissions\n"},
		{"1", []string{"x", "--model", "-i"}, "model=\nuser_model=0\nskip_model=0\nask_missing_model=1\nresume=\ninteractive=1\ndrop=2\n"},
		{"0", []string{"--", "--model", "m"}, "model=\nuser_model=0\nskip_model=0\nask_missing_model=0\nresume=\ninteractive=0\n"},
	} {
		var out, errw bytes.Buffer
		if rc := launchFlags(append([]string{c.routed}, c.argv...), strings.NewReader(arity), &out, &errw); rc != 0 || out.String() != c.want {
			t.Errorf("%v: rc=%d\n got %q\nwant %q", c.argv, rc, out.String(), c.want)
		}
	}
	var out, errw bytes.Buffer
	if rc := launchFlags([]string{"0", "--model"}, strings.NewReader(""), &out, &errw); rc != 2 || errw.String() != "--model requires a value\n" {
		t.Fatalf("missing value: rc=%d %q", rc, errw.String())
	}
}

func TestLaunchArgvClaimsOnlyRunsOwnFlags(t *testing.T) {
	for _, c := range []struct {
		args                 []string
		profile, interactive string
		fwd                  []string
	}{
		{[]string{"-a", "gw", "-i", "-p", ""}, "gw", "1", []string{"-p", ""}},
		{[]string{"--system-prompt", "--profile", "--profile=x"}, "x", "0", []string{"--system-prompt", "--profile"}},
		{[]string{"--", "-a", "gw"}, "", "0", []string{"--", "-a", "gw"}},
	} {
		var b strings.Builder
		if rc := launchArgv(c.args, strings.NewReader("--system-prompt value\n"), &b, io.Discard); rc != 0 {
			t.Fatalf("%v: rc %d", c.args, rc)
		}
		head, body, _ := strings.Cut(b.String(), "\ninteractive=")
		interactive, rest, _ := strings.Cut(body, "\n")
		rest = strings.TrimSuffix(rest, ".")
		fwd := strings.Split(rest, "\x00")
		fwd = fwd[:len(fwd)-1]
		if head != "profile="+c.profile || interactive != c.interactive || strings.Join(fwd, "|") != strings.Join(c.fwd, "|") || len(fwd) != len(c.fwd) {
			t.Errorf("%v: got %q", c.args, b.String())
		}
	}
	var e strings.Builder
	if rc := launchArgv([]string{"-a", "x", "-a", "y"}, strings.NewReader(""), io.Discard, &e); rc != 2 || !strings.Contains(e.String(), "only once") {
		t.Errorf("duplicate -a: rc %d %q", rc, e.String())
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
