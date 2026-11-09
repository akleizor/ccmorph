package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// completionRow runs doctorCompletion for an install rooted at root and
// returns what it printed.
func completionRow(t *testing.T, l *launcher) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	l.doctorCompletion()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func completionInstall(t *testing.T, root string) *launcher {
	t.Helper()
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &launcher{libDir: filepath.Join(root, "lib")}
}

func TestDoctorCompletionPrintsTheLineForAnInstallZshDoesNotSearch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("FPATH", "")
	got := completionRow(t, l)
	want := "fpath=(" + filepath.Join(root, "share", "zsh", "site-functions") + " $fpath)"
	if !strings.Contains(got, want) {
		t.Fatalf("doctor row = %q, want it to carry %q", got, want)
	}
}

func TestDoctorCompletionQuotesADirectoryWithSpaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "my libexec")
	l := completionInstall(t, root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("FPATH", "")
	got := completionRow(t, l)
	want := "fpath=('" + filepath.Join(root, "share", "zsh", "site-functions") + "' $fpath)"
	if !strings.Contains(got, want) {
		t.Fatalf("doctor row = %q, want one quoted element %q", got, want)
	}
}

func TestDoctorCompletionStaysSilentWhenZshAlreadyFindsIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")

	t.Setenv("FPATH", "/usr/share/zsh:"+dir)
	if got := completionRow(t, l); got != "" {
		t.Errorf("dir on FPATH: want no row, got %q", got)
	}

	// Homebrew's own site-functions, where brew links the file.
	brew := t.TempDir()
	os.WriteFile(filepath.Join(brew, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644)
	t.Setenv("FPATH", brew)
	if got := completionRow(t, l); got != "" {
		t.Errorf("_ccmorph elsewhere on FPATH: want no row, got %q", got)
	}

	t.Setenv("FPATH", "")
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("fpath=("+dir+" $fpath)\n"), 0o644)
	if got := completionRow(t, l); got != "" {
		t.Errorf(".zshrc names the dir: want no row, got %q", got)
	}
	os.Remove(filepath.Join(home, ".zshrc"))

	t.Setenv("SHELL", "/usr/bin/fish")
	if got := completionRow(t, l); got != "" {
		t.Errorf("fish login shell: want no row, got %q", got)
	}
}
