package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// GH-449: names over MaxNameLen became invalid in #373, and rm used the same
// gate, so a profile made before the limit could only be deleted by hand. rm
// now accepts the legacy range; a name past it is still refused untouched.
func TestRmRemovesAProfileMadeBeforeTheNameLimit(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	// rm asks the keychain for the profile's items on macOS; a no-op security
	// keeps the test off the real one.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
	for _, length := range []int{profile.MaxNameLen + 1, 240, profile.LegacyMaxNameLen} {
		name := strings.Repeat("n", length)
		conf := filepath.Join(state, name+".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The lock re-exec would start ccmorph-core; being already the locked
		// child is what a test of the removal itself needs.
		t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
		if rc := l.rm([]string{name}); rc != 0 {
			t.Fatalf("rm refused a %d-byte legacy name: rc %d", length, rc)
		}
		if _, err := os.Stat(conf); !os.IsNotExist(err) {
			t.Fatalf("rm left the %d-byte profile's conf behind: %v", length, err)
		}
	}
	name := strings.Repeat("n", profile.LegacyMaxNameLen+1)
	conf := filepath.Join(state, name+".conf")
	if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
	if rc := l.rm([]string{name}); rc == 0 {
		t.Fatal("rm must still refuse a name whose bridge file would exceed NAME_MAX")
	}
	if _, err := os.Stat(conf); err != nil {
		t.Fatalf("the refused profile's conf must stay: %v", err)
	}
}
