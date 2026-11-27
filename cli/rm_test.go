package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
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

// GH-915: dispatch discounted a `--` wherever it stood, while rm and mv only
// consume a leading one, so `ccmorph rm work --` deleted work. Only the leading
// separator is the command's; a trailing one is an extra word.
func TestRmAndMvTakeOnlyALeadingSeparator(t *testing.T) {
	stubKeychain(t)
	for _, argv := range [][]string{
		{"rm", "work", "--"},
		{"rm", "--", "work", "--"},
		{"rm", "work", "--", "x"},
		{"mv", "work", "b", "--"},
		{"mv", "--", "work", "b", "--"},
	} {
		l := storeTestLauncher(t, "work", "type=apikey\napi_key=k\n", `{"env":{}}`)
		answerRmPrompt(t, "y\n")
		out, rc := readerOutput(t, func() int { return l.dispatch(argv) })
		if rc != 2 || !strings.Contains(out, "unexpected argument") {
			t.Fatalf("%q: want exit 2 and an unexpected-argument line, got %d %q", argv, rc, out)
		}
		if _, err := os.Stat(filepath.Join(l.dir, "work.conf")); err != nil {
			t.Fatalf("%q: the refused command touched work.conf: %v", argv, err)
		}
		if _, err := os.Stat(filepath.Join(l.dir, "b.conf")); err == nil {
			t.Fatalf("%q: the refused mv wrote b.conf", argv)
		}
	}
}

// GH-915: a stem starting with '.' names no profile's private state — the
// validator never allowed one — and profiles/.rc-bridge is shared Remote
// Control state. rm and mv must refuse it before the destructive path.
func TestRmLeavesSharedDotStateAlone(t *testing.T) {
	stubKeychain(t)
	for _, stem := range []string{".rc-bridge", ".adapter-cache"} {
		l := storeTestLauncher(t, "work", "type=apikey\napi_key=k\n", `{"env":{}}`)
		t.Setenv("CCMORPH_CONTEXT_LOCKED", lockKey(stem))
		shared := filepath.Join(l.profileDirs, stem, "hosted.json")
		if err := os.MkdirAll(filepath.Dir(shared), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shared, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		conf := filepath.Join(l.dir, stem+".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		answerRmPrompt(t, "y\n")
		msg, rc := rmStderr(t, l, stem)
		if rc == 0 {
			t.Fatalf("stem %q: rm must refuse (printed %q)", stem, msg)
		}
		if _, err := os.Stat(shared); err != nil {
			t.Fatalf("stem %q: rm removed shared state: %v", stem, err)
		}
		if rc := l.mv([]string{stem, "fixed"}); rc == 0 {
			t.Fatalf("stem %q: mv must refuse", stem)
		}
		if _, err := os.Stat(shared); err != nil {
			t.Fatalf("stem %q: mv moved shared state: %v", stem, err)
		}
	}
}

// GH-915: the file-only repair for a stem without state of its own must run
// as printed under any state root: a relative root that starts with '-' read
// as an option to rm, and a root holding a space or quote must stay one word.
func TestFileOnlyRepairRunsUnderAnyStateRoot(t *testing.T) {
	for _, root := range []string{"-state", "my state", "it's"} {
		base := t.TempDir()
		t.Chdir(base)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		conf := filepath.Join(root, ".conf")
		if err := os.WriteFile(conf, []byte("type=apikey\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		l := &launcher{dir: root, profileDirs: filepath.Join(root, "profiles")}
		hint := l.repair("", errors.New("x"))
		_, cmd, ok := strings.Cut(hint, "or delete that file: ")
		if !ok || !strings.HasPrefix(cmd, "rm -- ") {
			t.Fatalf("root %q: the hint must end option parsing before the path: %q", root, hint)
		}
		if out, err := exec.Command("sh", "-c", cmd).CombinedOutput(); err != nil {
			t.Fatalf("root %q: the printed command failed: %q: %v %s", root, cmd, err, out)
		}
		if _, err := os.Lstat(conf); !os.IsNotExist(err) {
			t.Fatalf("root %q: the printed command left the conf: %v", root, err)
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

// rmOutput runs rm for one name with a piped answer and returns its stdout,
// stderr and status.
func rmOutput(t *testing.T, l *launcher, name, answer string) (string, string, int) {
	t.Helper()
	answerRmPrompt(t, answer)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	out, rc := captureStdout(t, func() int { return l.rm([]string{name}) })
	os.Stderr = old
	w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return out, string(b), rc
}

// GH-971: removing the last profile printed "set one: ccmorph use <profile>"
// with no profile left to set, directly above the listing's own "No profiles
// yet — run: ccmorph add <name>". A home with no conf is repairable one way,
// and the removal summary now names that way alone.
func TestRmNamesTheWayOnWhenTheLastProfileGoes(t *testing.T) {
	// remaining names the confs still on disk after the removal, and active
	// the default before it ("" for none).
	for _, c := range []struct {
		name            string
		confs, active   []string
		removed, answer string
		add, use        bool
	}{
		{"the last profile was the default", []string{"work"}, []string{"work"}, "work", "y\n", true, false},
		{"the last profile was not the default", []string{"work"}, nil, "work", "y\n", true, false},
		{"another profile remains and it was the default", []string{"work", "home"}, []string{"work"}, "work", "y\n", false, true},
		{"another profile remains and it was not the default", []string{"work", "home"}, []string{"home"}, "work", "y\n", false, false},
		{"only a skipped conf remains after the default", []string{"work"}, []string{"work"}, "work", "y\n", false, true},
		{"the removal is declined", []string{"work"}, []string{"work"}, "work", "n\n", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			state, home := t.TempDir(), t.TempDir()
			t.Setenv("HOME", home)
			stubKeychain(t)
			l := &launcher{dir: state, profileDirs: filepath.Join(home, ".claude")}
			for _, n := range c.confs {
				if err := os.WriteFile(filepath.Join(state, n+".conf"), []byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A conf no listing can load: its name is past the limit, so it is
			// skipped as a profile while its .conf still marks a non-empty home.
			if c.name == "only a skipped conf remains after the default" {
				if err := os.WriteFile(filepath.Join(state, strings.Repeat("n", profile.MaxNameLen+1)+".conf"),
					[]byte("type=apikey\napi_key=k\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if len(c.active) > 0 {
				if err := os.WriteFile(filepath.Join(state, "active"), []byte(c.active[0]+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CCMORPH_CONTEXT_LOCKED", c.removed)
			out, stderr, rc := rmOutput(t, l, c.removed, c.answer)
			wantRC := 0
			if c.answer == "n\n" {
				wantRC = 1 // a decline is not a removal
			}
			if rc != wantRC {
				t.Fatalf("rc %d, want %d: %s", rc, wantRC, stderr)
			}
			gotAdd := strings.Contains(out, "No profiles yet — run: ccmorph add <name>")
			gotUse := strings.Contains(stderr, "default profile removed — set one: ccmorph use <profile>")
			if gotAdd != c.add || gotUse != c.use {
				t.Fatalf("add hint %v (want %v), use hint %v (want %v)\nstdout: %q\nstderr: %q",
					gotAdd, c.add, gotUse, c.use, out, stderr)
			}
			if c.add {
				// The removal's own line comes first; the repair is last.
				lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
				if lines[0] != "Removed "+c.removed || lines[len(lines)-1] != noProfilesMsg {
					t.Fatalf("removal summary = %q", lines)
				}
			}
			if c.answer == "n\n" {
				if _, err := os.Stat(filepath.Join(state, c.removed+".conf")); err != nil {
					t.Fatalf("a declined removal removed the profile: %v", err)
				}
			}
		})
	}
}
