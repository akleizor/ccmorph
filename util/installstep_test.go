package util

import (
	"os"
	"path/filepath"
	"testing"
)

// ClipInstallStep answers from the machine's PATH, the same branch install.sh
// takes (#369): the brew command where Homebrew is there, the project page
// where it is not — so the installer and the core name one step per machine.
func TestClipInstallStep(t *testing.T) {
	brew := t.TempDir()
	if err := os.WriteFile(filepath.Join(brew, "brew"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", brew)
	if got := ClipInstallStep(); got != "brew install cliproxyapi" {
		t.Fatalf("with brew on PATH: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := ClipInstallStep(); got != clipProjectPage {
		t.Fatalf("without brew: %q", got)
	}
}
