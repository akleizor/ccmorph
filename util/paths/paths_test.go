package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLibDirDefaults(t *testing.T) { // moved from adapter: the CCMORPH path half of test_profile_env_sources_the_packaged_ccmorph_script
	t.Setenv("CCMORPH_LIB_DIR", "/somewhere/lib")
	if got := LibDir(); got != "/somewhere/lib" {
		t.Errorf("override ignored: %s", got)
	}
	if got := Script(); got != "/somewhere/lib/ccmorph.sh" {
		t.Errorf("script %s, want /somewhere/lib/ccmorph.sh", got)
	}
	t.Setenv("CCMORPH_LIB_DIR", "")
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if want := filepath.Join(filepath.Dir(filepath.Dir(exe)), "lib"); LibDir() != want {
		t.Errorf("default %s, want %s", LibDir(), want)
	}
}

func TestStateDir(t *testing.T) { // where usage rows live: the XDG state root, $XDG_STATE_HOME or ~/.local/state
	t.Setenv("XDG_STATE_HOME", "  /var/state  ")
	if got := StateDir(); got != "/var/state/ccmorph" {
		t.Errorf("override ignored: %s", got)
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/home/u")
	if got := StateDir(); got != "/home/u/.local/state/ccmorph" {
		t.Errorf("default %s, want /home/u/.local/state/ccmorph", got)
	}
}

func TestLexistsCountsABrokenSymlink(t *testing.T) { // #520: the Lstat half of the predicate pair
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken")
	if err := os.Symlink(filepath.Join(dir, "gone"), broken); err != nil {
		t.Fatal(err)
	}
	if !Lexists(broken) || Exists(broken) {
		t.Errorf("a broken symlink: Lexists %v, Exists %v; want true, false",
			Lexists(broken), Exists(broken))
	}
	if !Lexists(real) || !Exists(real) {
		t.Errorf("a real file: Lexists %v, Exists %v; want true, true",
			Lexists(real), Exists(real))
	}
	if Lexists(filepath.Join(dir, "gone")) {
		t.Error("a missing path does not lexist")
	}
}

func TestResolve(t *testing.T) { // #520: symlinks followed, a missing tail kept, an unset path unset
	// The fixture sits on the resolved temp root: a runner may spell TMPDIR
	// through a symlink (/var → /private/var on macOS) that EvalSymlinks —
	// Resolve's own rule — folds away, and these assertions compare spellings.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("d", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if got, want := Resolve(filepath.Join(dir, "link")), filepath.Join(dir, "d"); got != want {
		t.Errorf("a symlink: %s, want %s", got, want)
	}
	missing := filepath.Join(dir, "gone", "tail")
	if got, want := Resolve(missing), missing; got != want {
		t.Errorf("a missing tail: %s, want the absolute path %s", got, want)
	}
	if got := Resolve(""); got != "" {
		t.Errorf("an unset path: %q, want empty", got)
	}
}
