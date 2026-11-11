package util

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pkgHintRow matches one row of install.sh's _ccm_pkg_hint table: the manager
// the row probes for and the install command it prints, with $1 unexpanded.
var pkgHintRow = regexp.MustCompile(`command -v (\S+).*then echo "([^"]+)"`)

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

// fzfSteps is install.sh's _ccm_pkg_hint table copied into Go, and nothing
// else pins the two (#452): a manager added or reordered in the installer
// would leave this table naming the wrong command, or a command the machine
// cannot run (#369). So read the shell table itself, in its own probe order,
// and hold fzfSteps to it.
func TestFzfStepsMatchInstallerTable(t *testing.T) {
	installer, err := os.ReadFile("../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	body := string(installer)
	start := strings.Index(body, "_ccm_pkg_hint() {")
	if start < 0 {
		t.Fatal("install.sh has no _ccm_pkg_hint table")
	}
	body = body[start:]
	end := strings.Index(body, "\n}\n")
	if end < 0 {
		t.Fatal("install.sh: _ccm_pkg_hint is not closed")
	}
	body = body[:end]

	type row struct{ probe, step string }
	var want []row
	for _, m := range pkgHintRow.FindAllStringSubmatch(body, -1) {
		want = append(want, row{m[1], strings.ReplaceAll(m[2], "$1", "fzf")})
	}
	if len(want) == 0 {
		t.Fatal("install.sh: _ccm_pkg_hint table parsed empty")
	}
	if len(want) != len(fzfSteps) {
		t.Fatalf("install.sh's _ccm_pkg_hint names %d managers, fzfSteps names %d", len(want), len(fzfSteps))
	}
	for i, w := range want {
		if w.probe != fzfSteps[i].probe || w.step != fzfSteps[i].step {
			t.Errorf("manager %d: install.sh says %q -> %q, fzfSteps says %q -> %q",
				i, w.probe, w.step, fzfSteps[i].probe, fzfSteps[i].step)
		}
	}
}
