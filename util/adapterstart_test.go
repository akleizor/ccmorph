package util

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCore writes a stub `core adapter` script answering with body.
func fakeCore(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "core")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAdapterStartHandsBackPortAndNeverTheTTY(t *testing.T) {
	stdin := filepath.Join(t.TempDir(), "stdin")
	// The 610137d wedge: the adapter holding the terminal on fd0 froze the TUI.
	core := fakeCore(t, `if [ -t 0 ]; then echo tty; else echo not-a-tty; fi > '`+stdin+`'; echo PORT=4242; sleep 5`)
	var out, errw bytes.Buffer
	if rc := adapterStart([]string{core, "p", "m", "1", os.DevNull}, nil, &out, &errw); rc != 0 {
		t.Fatalf("rc %d: %s", rc, errw.String())
	}
	if !strings.Contains(out.String(), "PORT=4242") || !strings.Contains(out.String(), "PID=") {
		t.Fatalf("out %q", out.String())
	}
	if b, _ := os.ReadFile(stdin); strings.TrimSpace(string(b)) != "not-a-tty" {
		t.Fatalf("adapter fd0: %q", b)
	}
}

func TestAdapterStartSaysWhyItFailed(t *testing.T) {
	for body, want := range map[string]string{
		"echo 'EXIT=no route for p'": "no route for p",
		"exit 3":                     "exited before readiness",
	} {
		var errw bytes.Buffer
		if rc := adapterStart([]string{fakeCore(t, body), "p", "m", "1", os.DevNull}, nil, &bytes.Buffer{}, &errw); rc != 1 || !strings.Contains(errw.String(), want) {
			t.Errorf("%q: rc %d, stderr %q", body, rc, errw.String())
		}
	}
}
