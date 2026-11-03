package util

import (
	"os"
	"path/filepath"
	"testing"
)

// ~/.claude is Claude Code's: provisioning links the shared layer into the
// profile and creates the shared dirs, but never changes that directory's mode.
func TestSetupProfileLeavesTheNativeClaudeDirModeAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	native := filepath.Join(home, ".claude")
	os.Mkdir(native, 0o755)
	os.Chmod(native, 0o755)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	os.MkdirAll(profiles, 0o700)
	var errs []string
	if !SetupProfile(profiles, "demo", "apikey", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("setup failed: %v", errs)
	}
	if fi, _ := os.Stat(native); fi.Mode().Perm() != 0o755 {
		t.Errorf("native dir mode = %o", fi.Mode().Perm())
	}
	if fi, err := os.Lstat(filepath.Join(profiles, "demo/skills")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("skills must link to the shared layer: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(profiles, "demo")); fi.Mode().Perm() != 0o700 {
		t.Errorf("profile dir mode = %o", fi.Mode().Perm())
	}
	if !ProfileOK(profiles, "demo") {
		t.Error("a provisioned profile must pass the fast path")
	}
}
