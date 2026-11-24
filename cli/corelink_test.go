package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// keg lays out <brew>/Cellar/ccmorph/<v>/libexec/libexec/ccmorph-core and,
// with opt, brew's <brew>/opt/ccmorph link to it. It returns brew and the
// keg's libexec dir the way a Homebrew launch names it (through opt).
func keg(t *testing.T, opt bool) (string, string) {
	t.Helper()
	brew, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(brew, "Cellar", "ccmorph", "0.0.1", "libexec", "libexec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ccmorph-core"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !opt {
		return brew, dir
	}
	if err := os.MkdirAll(filepath.Join(brew, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/ccmorph/0.0.1", filepath.Join(brew, "opt", "ccmorph")); err != nil {
		t.Fatal(err)
	}
	return brew, filepath.Join(brew, "opt", "ccmorph", "libexec", "libexec")
}

func TestBrewOptCoreNamesTheOptPathOnlyUnderHomebrew(t *testing.T) {
	brew, libexec := keg(t, true)
	want := filepath.Join(brew, "opt", "ccmorph", "libexec", "libexec", "ccmorph-core")
	if got := brewOptCore(libexec); got != want {
		t.Fatalf("through opt: got %q, want %q", got, want)
	}
	cellar := filepath.Join(brew, "Cellar", "ccmorph", "0.0.1", "libexec", "libexec")
	if got := brewOptCore(cellar); got != want {
		t.Fatalf("through the Cellar dir: got %q, want %q", got, want)
	}
	if _, noOpt := keg(t, false); brewOptCore(noOpt) != "" {
		t.Fatal("a Cellar-shaped path without brew's opt link is not a Homebrew install")
	}
	if got := brewOptCore(t.TempDir()); got != "" {
		t.Fatalf("a clone or install.sh layout has no opt core: got %q", got)
	}
	if got := brewOptCore(""); got != "" {
		t.Fatalf("an unset libexec: got %q", got)
	}
}

func TestPublishCoreLinksTheConfigPathAndKeepsWhatIsThere(t *testing.T) {
	brew, libexec := keg(t, true)
	want := filepath.Join(brew, "opt", "ccmorph", "libexec", "libexec", "ccmorph-core")
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	dest := filepath.Join(cfg, "ccmorph", "libexec", "ccmorph-core")
	l := &launcher{libexec: libexec}

	// No config dir yet: nothing is created for the link.
	l.publishCore()
	if _, err := os.Lstat(filepath.Join(cfg, "ccmorph")); err == nil {
		t.Fatal("publishCore must not create the config dir")
	}
	if err := os.Mkdir(filepath.Join(cfg, "ccmorph"), 0o700); err != nil {
		t.Fatal(err)
	}
	l.publishCore()
	if got, err := os.Readlink(dest); err != nil || got != want {
		t.Fatalf("link: got %q, %v; want %q", got, err, want)
	}
	if fi, err := os.Stat(filepath.Dir(dest)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("libexec dir must be 0700: %v %v", fi, err)
	}
	l.publishCore() // idempotent
	if got, _ := os.Readlink(dest); got != want {
		t.Fatalf("second run changed the link to %q", got)
	}

	// A regular file (install.sh's core) is never replaced.
	os.Remove(dest)
	if err := os.WriteFile(dest, []byte("install.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.publishCore()
	if b, _ := os.ReadFile(dest); string(b) != "install.sh" {
		t.Fatalf("install.sh's core was replaced: %q", b)
	}
	// A working link elsewhere is kept; a dangling one is repaired.
	os.Remove(dest)
	other := filepath.Join(t.TempDir(), "core")
	os.WriteFile(other, []byte("x"), 0o755)
	os.Symlink(other, dest)
	l.publishCore()
	if got, _ := os.Readlink(dest); got != other {
		t.Fatalf("a working link was replaced with %q", got)
	}
	os.Remove(dest)
	os.Symlink(filepath.Join(t.TempDir(), "gone"), dest)
	l.publishCore()
	if got, _ := os.Readlink(dest); got != want {
		t.Fatalf("a dangling link was not repaired: %q", got)
	}
	if left, _ := filepath.Glob(dest + ".tmp-*"); len(left) != 0 {
		t.Fatalf("temporary links left behind: %v", left)
	}

	// Not Homebrew: nothing is written.
	os.Remove(dest)
	(&launcher{libexec: t.TempDir()}).publishCore()
	if _, err := os.Lstat(dest); err == nil {
		t.Fatal("a non-Homebrew core must not be published")
	}
}
