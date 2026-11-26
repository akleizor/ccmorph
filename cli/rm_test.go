package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// answerRmPrompt holds os.Stdin at a pipe carrying `answer` for a whole rm:
// removal asks whoever runs it, not only a terminal (#724), so an in-process
// rm must come with its answer. One line is all the [y/N] prompt reads; a
// second call overwrites the pipe with its own answer.
func answerRmPrompt(t *testing.T, answer string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(answer); err != nil {
		t.Fatal(err)
	}
	w.Close()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old; r.Close() })
}

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
	// 245 is the issue's repro, 242 the first name the bridge-file bound
	// refused; 201 is the first legacy name, 250 the last storable one, and
	// 240/241 are the lengths the old test already removed.
	for _, length := range []int{245, profile.MaxNameLen + 1, 240, 241, 242, profile.LegacyMaxNameLen} {
		name := strings.Repeat("n", length)
		conf := filepath.Join(state, name+".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// The lock re-exec would start ccmorph-core; being already the locked
		// child is what a test of the removal itself needs.
		t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
		answerRmPrompt(t, "y\n")
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
	// The answer is held for the name gate too: with no stdin the prompt's
	// own EOF refusal could stand in for the validation refusal, and a
	// validator widened by one would pass for the wrong reason (#724 gate).
	// With the Yes held, only the name gate itself can refuse this rm.
	answerRmPrompt(t, "y\n")
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
		answerRmPrompt(t, "y\n")
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
		// The next default launch must read no default at all, not a refusal.
		if next, ok := l.readActive(); !ok || next != "" {
			t.Fatalf("after removing the %d-byte default, readActive = %q (ok %v)", length, next, ok)
		}
		if strings.Contains(msg, "invalid active profile") {
			t.Fatalf("removing the default must not refuse the name it just removed: %s", msg)
		}
	}
}

// GH-915: `.conf` and `..conf` are listed by the scanner under the stems ""
// and ".", and a removal that admitted either resolved <profiles>/"" and
// <profiles>/. to the profiles directory itself — one attempt deleted every
// profile's private state. A missing or empty operand is a usage fault before
// the destructive path, and a stem that has no private state of its own is
// never one rm removes.
func TestRmNeverReachesTheProfilesDirForAStemWithoutState(t *testing.T) {
	for _, stem := range []string{"", "."} {
		l := storeTestLauncher(t, "work", "type=apikey\napi_key=k\n", `{"env":{}}`)
		settings := filepath.Join(l.profileDirs, "work", "settings.profile.json")
		conf := filepath.Join(l.dir, stem+".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		answerRmPrompt(t, "y\n")
		msg, rc := rmStderr(t, l, stem)
		if rc == 0 {
			t.Fatalf("stem %q: rm must refuse, not remove (printed %q)", stem, msg)
		}
		if _, err := os.Stat(settings); err != nil {
			t.Fatalf("stem %q: rm removed a profile's private state: %v", stem, err)
		}
		if _, err := os.Stat(conf); err != nil {
			t.Fatalf("stem %q: rm removed the conf it refused: %v", stem, err)
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
	// The answer is held for the name gate too: with no stdin the prompt's
	// own EOF refusal could stand in for the validation refusal (#724 gate).
	// With the Yes held, only the case-twin guard can refuse this rm.
	answerRmPrompt(t, "y\n")
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

// GH-551: the clear compares the active file's own content, so removing a
// profile that is not the default leaves the default naming another profile
// exactly as it was.
func TestRmLeavesAnotherProfilesDefaultAlone(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	stubKeychain(t)
	l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
	name := strings.Repeat("n", profile.MaxNameLen+1)
	for _, n := range []string{name, "work"} {
		if err := os.WriteFile(filepath.Join(state, n+".conf"), []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	active := filepath.Join(state, "active")
	if err := os.WriteFile(active, []byte("work\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The lock re-exec would start ccmorph-core; being already the locked
	// child is what a test of the removal itself needs.
	t.Setenv("CCMORPH_CONTEXT_LOCKED", name)
	answerRmPrompt(t, "y\n")
	msg, rc := rmStderr(t, l, name)
	if rc != 0 {
		t.Fatalf("rm refused the %d-byte profile: rc %d: %s", len(name), rc, msg)
	}
	if b, err := os.ReadFile(active); err != nil || string(b) != "work\n" {
		t.Fatalf("removing a profile that is not the default changed the active file: %q, %v", b, err)
	}
	if strings.Contains(msg, "default profile removed") {
		t.Fatalf("rm must not report clearing a default it left alone: %s", msg)
	}
}
