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

// #562, pinned before cli's sameFile and the shim's merged under util/paths:
// keepHistory's "a native link to the same file (or a dangling one) gives way"
// is the one caller that needs the follow — a symlink at the destination is
// the file it points at. A link to another file, or a real file already there,
// is not the same file: the history stays and the profile is kept.
func TestKeepHistoryGivesWayOnlyToALinkToTheSameFile(t *testing.T) {
	for _, c := range []struct {
		name string
		at   string // what sits at the destination: a link to the same file, to another, to nothing, or a real file
		want bool
	}{
		{"a native link to the same file gives way", "same", true},
		{"a dangling native link gives way", "dangling", true},
		{"a native link to another file does not", "other", false},
		{"a real file at the destination does not", "file", false},
	} {
		home := t.TempDir()
		from := filepath.Join(home, "from")
		to := filepath.Join(home, "to")
		for _, d := range []string{filepath.Join(from, "projects", "p"), filepath.Join(to, "projects", "p")} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		f := filepath.Join(from, "projects", "p", "s.jsonl")
		if err := os.WriteFile(f, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		at := filepath.Join(to, "projects", "p", "s.jsonl")
		switch c.at {
		case "same":
			if err := os.Symlink(f, at); err != nil {
				t.Fatal(err)
			}
		case "dangling":
			if err := os.Symlink(filepath.Join(to, "nowhere"), at); err != nil {
				t.Fatal(err)
			}
		case "other":
			other := filepath.Join(to, "projects", "p", "other.jsonl")
			if err := os.WriteFile(other, []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, at); err != nil {
				t.Fatal(err)
			}
		case "file":
			if err := os.WriteFile(at, []byte("keep\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := keepHistory(from, to); got != c.want {
			t.Errorf("%s: keepHistory = %t, want %t", c.name, got, c.want)
			continue
		}
		if c.want {
			if fi, err := os.Lstat(at); err != nil || fi.Mode()&os.ModeSymlink != 0 {
				t.Errorf("%s: the history file did not land on the link's name: %v", c.name, err)
			}
			continue
		}
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s: a history file that cannot land must stay: %v", c.name, err)
		}
	}
}
