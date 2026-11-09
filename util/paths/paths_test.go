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
