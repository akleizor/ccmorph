package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A private view that cannot be built must say so and never print the hop's
// cache notice: the session it announces is not starting (#226).
func TestPrivateViewFailureSkipsTheNotice(t *testing.T) {
	real := filepath.Join(t.TempDir(), "demo")
	os.Mkdir(real, 0o300) // listable by no one: the view cannot link its entries
	t.Cleanup(func() { os.Chmod(real, 0o700) })
	t.Setenv("CLAUDE_CONFIG_DIR", real)
	t.Setenv("TMPDIR", t.TempDir())
	said := false
	var errw strings.Builder
	if rc := privateSession("/bin/true", nil, func() { said = true }, &errw); rc != 1 || said {
		t.Fatalf("rc %d, notice printed %v", rc, said)
	}
	if !strings.Contains(errw.String(), "private view") {
		t.Errorf("stderr %q", errw.String())
	}
}

func TestPrivateViewLinksConfigButNotSessionState(t *testing.T) {
	real := filepath.Join(t.TempDir(), "demo")
	for _, n := range []string{"settings.json", "projects", ".claude.json"} {
		os.MkdirAll(filepath.Join(real, n), 0o700)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", real)
	t.Setenv("TMPDIR", t.TempDir())
	out := filepath.Join(t.TempDir(), "seen")
	script := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(script, []byte("#!/bin/sh\nls -A \"$CLAUDE_CONFIG_DIR\" > '"+out+"'; echo \"$CLAUDE_SECURESTORAGE_CONFIG_DIR\" >> '"+out+"'; exit 7\n"), 0o755)
	if rc := privateSession(script, nil, func() {}, &strings.Builder{}); rc != 7 {
		t.Fatalf("rc %d, want claude's own 7", rc)
	}
	b, _ := os.ReadFile(out)
	if got := string(b); got != ".claude.json\nsettings.json\n"+real+"\n" {
		t.Errorf("view saw %q", got)
	}
}
