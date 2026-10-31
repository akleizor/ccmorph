package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRelayReadConsumesOnlyAWellFormedRequest(t *testing.T) {
	dir := t.TempDir()
	run := func(body string) (int, string, string) {
		p := filepath.Join(dir, "relay")
		if body != "\x00none" {
			os.WriteFile(p, []byte(body), 0o600)
		}
		var o, e strings.Builder
		rc := relayRead([]string{p}, nil, &o, &e)
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%q: relay file left behind", body)
		}
		return rc, o.String(), e.String()
	}
	if rc, _, _ := run("\x00none"); rc != 3 {
		t.Errorf("no file: rc %d", rc)
	}
	if rc, out, _ := run("profile=gw\nsession=abc\nmodel=m"); rc != 0 || out != "profile=gw\nsession=abc\nmodel=m\neffort=\n" {
		t.Errorf("good: rc %d %q", rc, out)
	}
	for body, want := range map[string]string{
		"profile=gw\nprofile=x\nsession=s\n": "duplicate relay field 'profile'",
		"profile=gw\nbogus=1\nsession=s\n":   "invalid relay signal",
		"profile=gw\n":                       "invalid relay signal",
		"profile=gw\nsession=s\r\n":          "invalid relay signal",
	} {
		if rc, _, e := run(body); rc != 1 || !strings.Contains(e, want) {
			t.Errorf("%q: rc %d %q", body, rc, e)
		}
	}
}
