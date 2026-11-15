package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// rmStderr runs rm for one name and returns what it printed on stderr with its
// exit status, so a refusal's line is read the way a person reads it.
func rmStderr(t *testing.T, l *launcher, name string) (string, int) {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	rc := l.rm([]string{name})
	w.Close()
	os.Stderr = old
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), rc
}

// stubKeychain points PATH's `security` at a no-op, so rm's keychain deletes on
// macOS never reach the real one.
func stubKeychain(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// GH-449, GH-551: names over MaxNameLen became invalid in #373, and rm used the
// same gate, so a profile made before the limit could only be deleted by hand.
// rm accepts every name whose own files still fit NAME_MAX: the conf and the
// lock are name+5, so 250 is the last storable name — 241, the bridge file's
// name+14, bounds only a file a bridge makes, and a name past it simply has no
// bridge file to remove. A name past the storable bound is still refused,
// naming rm's own bound rather than ValidName's.
func TestRmRemovesAProfileMadeBeforeTheNameLimit(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	stubKeychain(t)
	l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
	// 242 is the first name the bridge-file bound refused, 245 the issue's.
	for _, length := range []int{profile.MaxNameLen + 1, 242, 245, profile.LegacyMaxNameLen} {
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
	// One past the storable bound no such profile can exist: its conf would not
	// fit NAME_MAX either. The refusal names the bound rm applies (#551).
	name := strings.Repeat("n", profile.LegacyMaxNameLen+1)
	t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
	msg, rc := rmStderr(t, l, name)
	if rc == 0 {
		t.Fatal("rm must still refuse a name whose conf would exceed NAME_MAX")
	}
	if !strings.Contains(msg, "too long; at most 250 of A-Z, a-z, 0-9 and _") {
		t.Fatalf("the refusal must name the bound rm applies, got: %s", msg)
	}
}

// GH-551: rm compared l.active(), whose reader refuses the legacy length, so
// removing the default profile left the active file naming a profile that no
// longer exists — every later default launch answered "invalid active profile"
// until the file was deleted by hand. The clear reads the active file's own
// content, whatever the name's length.
func TestRmClearsTheDefaultItRemovedWhateverTheNameLength(t *testing.T) {
	// 4 is a valid name, so the clear must keep working; 201 is the first
	// legacy name; 245 is one the bridge-file bound used to refuse outright.
	for _, length := range []int{4, profile.MaxNameLen + 1, 245} {
		state, home := t.TempDir(), t.TempDir()
		t.Setenv("HOME", home)
		stubKeychain(t)
		name := strings.Repeat("w", length)
		l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
		conf := filepath.Join(state, name+".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		active := filepath.Join(state, "active")
		if err := os.WriteFile(active, []byte(name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The lock re-exec would start ccmorph-core; being already the locked
		// child is what a test of the removal itself needs.
		t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
		msg, rc := rmStderr(t, l, name)
		if rc != 0 {
			t.Fatalf("rm refused a %d-byte default: rc %d: %s", length, rc, msg)
		}
		if _, err := os.Stat(conf); !os.IsNotExist(err) {
			t.Fatalf("rm left the %d-byte profile's conf behind: %v", length, err)
		}
		if _, err := os.Stat(active); !os.IsNotExist(err) {
			t.Fatalf("rm left the active file naming the %d-byte default it removed: %v", length, err)
		}
		if strings.Contains(msg, "invalid active profile") {
			t.Fatalf("removing the default must not refuse the name it just removed: %s", msg)
		}
	}
}

// GH-551: rm's case-twin guard asked a reader that filtered candidates through
// ValidName, so a legacy-length twin was invisible and the refusal fell through
// to the leftover-state branch — on a volume that ignores letter case, the one
// that offers to remove the real profile's private state. The twin is one rm
// can address, whatever its length.
func TestRmRefusesACaseTwinOfTheLegacyLength(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
	twin := strings.Repeat("a", 210)
	conf := filepath.Join(state, twin+".conf")
	if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The twin's private state, so a case-insensitive volume takes the path the
	// guard exists to close.
	if err := os.MkdirAll(filepath.Join(l.profileDirs, twin), 0o755); err != nil {
		t.Fatal(err)
	}
	asked := strings.Repeat("A", 210)
	// The lock re-exec would start ccmorph-core; being already the locked
	// child is what a test of the refusal itself needs.
	t.Setenv("CCMORPH_CONTEXT_LOCKED", asked)
	msg, rc := rmStderr(t, l, asked)
	if rc != 1 {
		t.Fatalf("the case twin must be refused, rc %d: %s", rc, msg)
	}
	if !strings.Contains(msg, "Did you mean '"+twin+"'") {
		t.Fatalf("the refusal must name the legacy-length twin: %s", msg)
	}
	if _, err := os.Stat(conf); err != nil {
		t.Fatalf("the twin's conf must stay: %v", err)
	}
}
