package util

import (
	"bytes"
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
