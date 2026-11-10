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

// FzfInstallStep answers from the machine's PATH, the same table install.sh's
// own fzf hint takes (#387): the first package manager found names its
// command, and where none is, the project page — so the picker's hint and the
// installer name one step per machine, never a command it cannot run (#369).
func TestFzfInstallStep(t *testing.T) {
	stub := func(dir, name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	brew := t.TempDir()
	stub(brew, "brew")
	t.Setenv("PATH", brew)
	if got := FzfInstallStep(); got != "brew install fzf" {
		t.Fatalf("with brew on PATH: %q", got)
	}
	apt := t.TempDir()
	stub(apt, "apt-get")
	t.Setenv("PATH", apt)
	if got := FzfInstallStep(); got != "sudo apt-get install -y fzf" {
		t.Fatalf("with apt-get on PATH: %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if got := FzfInstallStep(); got != fzfProjectPage {
		t.Fatalf("without a package manager: %q", got)
	}
}
