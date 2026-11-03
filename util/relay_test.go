package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRelayConsumesOnlyAWellFormedRequest(t *testing.T) {
	dir := t.TempDir()
	run := func(body string) (Relay, error) {
		p := filepath.Join(dir, "relay")
		if body != "\x00none" {
			os.WriteFile(p, []byte(body), 0o600)
		}
		r, err := ReadRelay(p)
		if _, e := os.Lstat(p); e == nil {
			t.Errorf("%q: relay file left behind", body)
		}
		return r, err
	}
	if _, err := run("\x00none"); err != ErrNoRelay {
		t.Errorf("no file: %v", err)
	}
	if r, err := run("profile=gw\nsession=abc\nmodel=m"); err != nil || r != (Relay{"gw", "abc", "m", ""}) {
		t.Errorf("good: %v %+v", err, r)
	}
	for body, want := range map[string]string{
		"profile=gw\nprofile=x\nsession=s\n": "duplicate relay field 'profile'",
		"profile=gw\nbogus=1\nsession=s\n":   "invalid relay signal",
		"profile=gw\n":                       "invalid relay signal",
		"profile=gw\nsession=s\r\n":          "invalid relay signal",
	} {
		if _, err := run(body); err == nil || err == ErrNoRelay || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", body, err)
		}
	}
}
