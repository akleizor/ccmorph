package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// secondFS returns a fresh directory on a filesystem other than base's, so a
// test can put the shared store behind a volume boundary the way #177's
// machine does (~/.claude/projects is a link at another disk). It tries
// $CCMORPH_TEST_SECOND_FS, then /dev/shm and the mounted volumes. A machine
// with no second filesystem the test may write returns "" and the store stays
// on base's filesystem: the copy path is the same either way, only a rename
// would notice the boundary.
func secondFS(t *testing.T, base string) string {
	t.Helper()
	dev := func(p string) uint64 {
		fi, err := os.Stat(p)
		if err != nil {
			return 0
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			return uint64(st.Dev)
		}
		return 0
	}
	try := func(dir string) string {
		if dir == "" || dev(dir) == 0 || dev(dir) == dev(base) {
			return ""
		}
		d, err := os.MkdirTemp(dir, "ccmorph-second-fs.")
		if err != nil {
			return ""
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	if v := os.Getenv("CCMORPH_TEST_SECOND_FS"); v != "" {
		if d := try(v); d != "" {
			return d
		}
	}
	cands := []string{"/dev/shm"}
	if vols, err := os.ReadDir("/Volumes"); err == nil {
		for _, v := range vols {
			cands = append(cands, filepath.Join("/Volumes", v.Name(), "cache"), filepath.Join("/Volumes", v.Name()))
		}
	}
	for _, c := range cands {
		if d := try(c); d != "" {
			return d
		}
	}
	if os.Getenv("CI") != "" {
		t.Fatal("CI must give this test a second filesystem, or the cross-volume path goes untested")
	}
	return ""
}

func inodeOf(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("no stat for " + p)
	}
	return st.Ino
}

// hrec is one prompt-history record: a display text and its millisecond
// timestamp, the field Claude Code orders history by.
func hrec(ms int64, display string) string {
	return fmt.Sprintf("{\"display\":%q,\"pastedContents\":{},\"project\":\"/w\",\"timestamp\":%d}\n", display, ms)
}

// #177: a profile's own projects merges into the shared store by copying, so a
// store on another filesystem — reached through a link, the shape of the
// incident machine — takes every file; the profile then links at the store and
// its paste cache shares the same way.
func TestSetupMergesProfileHistoryIntoTheSharedStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	store := filepath.Join(home, ".claude", "projects")
	if second := secondFS(t, home); second != "" {
		t.Logf("store on a second filesystem: %s", second)
		real := filepath.Join(second, "store")
		if err := os.MkdirAll(real, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(store), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, store); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(profiles, "p/projects/slug/s1.jsonl"), "session one\n")
	mustWrite(t, filepath.Join(profiles, "p/paste-cache/abc"), "pasted\n")
	mustWrite(t, filepath.Join(home, ".claude/projects/slug/native.jsonl"), "native\n")
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("setup failed: %v", errs)
	}
	dir := filepath.Join(profiles, "p")
	link := filepath.Join(dir, "projects")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("profile history must link at the shared store: %v", err)
	}
	for p, want := range map[string]string{
		filepath.Join(link, "slug/s1.jsonl"):           "session one\n",
		filepath.Join(link, "slug/native.jsonl"):       "native\n",
		filepath.Join(store, "slug/s1.jsonl"):          "session one\n",
		filepath.Join(home, ".claude/paste-cache/abc"): "pasted\n",
	} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q", p, b, err, want)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dir, "paste-cache")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the paste cache must link at the shared store: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "projects.*")); len(left) != 0 {
		t.Errorf("no backup may remain: %v", left)
	}
	if !ProfileOK(profiles, "p") {
		t.Error("a shared profile must pass the fast path")
	}
}

// #177: prompt history is shared without losing a record — the shared file
// keeps its own records and gains the profile's missing ones, appended in
// their timestamp order (an identical record collapses, a line no reader can
// order keeps its place at the end), and the profile links at the shared file.
func TestSetupSharesPromptHistoryWithoutLosingARecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	mustWrite(t, filepath.Join(home, ".claude/history.jsonl"), hrec(200, "b")+hrec(400, "d"))
	mustWrite(t, filepath.Join(profiles, "p/history.jsonl"), hrec(100, "a")+hrec(300, "c")+hrec(200, "b")+"not json\n")
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("setup failed: %v", errs)
	}
	merged, err := os.ReadFile(filepath.Join(home, ".claude/history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := hrec(200, "b") + hrec(400, "d") + hrec(100, "a") + hrec(300, "c") + "not json\n"
	if string(merged) != want {
		t.Errorf("shared history =\n%q\nwant\n%q", merged, want)
	}
	if fi, err := os.Lstat(filepath.Join(profiles, "p/history.jsonl")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("profile history must link at the shared file: %v", err)
	}
	if !ProfileOK(profiles, "p") {
		t.Error("a shared profile must pass the fast path")
	}
}

// #177: a profile with a live session is left unchanged at launch — no swap,
// no failure — and merges once the session has ended. Claude Code records each
// session as sessions/<pid>.json under the config dir it runs with.
func TestSetupLeavesABusyProfileOnItsOwnHistory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	projects := filepath.Join(profiles, "p/projects")
	mustWrite(t, filepath.Join(projects, "slug/s1.jsonl"), "session one\n")
	sess := exec.Command("/bin/sleep", "30")
	if err := sess.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sess.Process.Kill(); sess.Wait() }()
	sessions := filepath.Join(profiles, "p/sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, strconv.Itoa(sess.Process.Pid)+".json"),
		[]byte(fmt.Sprintf("{\"pid\":%d,\"sessionId\":\"s\"}\n", sess.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	var notes, errs []string
	log, errf := func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }
	before := inodeOf(t, projects)
	if !SetupProfile(profiles, "p", "proxy", log, errf) {
		t.Fatalf("a busy profile must still launch: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("a busy profile's history must stay a real dir: %v", err)
	}
	if after := inodeOf(t, projects); after != before {
		t.Errorf("a busy profile's history dir must keep its inode: %d -> %d", before, after)
	}
	if b, _ := os.ReadFile(filepath.Join(projects, "slug/s1.jsonl")); string(b) != "session one\n" {
		t.Error("a busy profile's transcripts must be untouched")
	}
	if left, _ := filepath.Glob(filepath.Join(profiles, "p", "projects.*")); len(left) != 0 {
		t.Errorf("a busy profile must not grow backups: %v", left)
	}
	deferred := false
	for _, n := range notes {
		if strings.Contains(n, "live session") {
			deferred = true
		}
	}
	if !deferred {
		t.Errorf("the deferral must be reported: %v", notes)
	}
	// The session ends; the next launch merges.
	if err := sess.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = sess.Wait() // killed by the test; its exit status is not under test
	notes, errs = nil, nil
	if !SetupProfile(profiles, "p", "proxy", log, errf) {
		t.Fatalf("setup after the session ended failed: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("an idle profile's history must link at the store: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(projects, "slug/s1.jsonl")); err != nil || string(b) != "session one\n" {
		t.Errorf("the merged transcript must be readable through the link: %q %v", b, err)
	}
}

// #177: a profile whose projects links at the user's own store on another disk
// merges too; the disk keeps every file and only the profile's link is
// re-pointed at the shared store.
func TestSetupMigratesAProfileWhoseHistoryLinksToAnotherDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	elsewhere := filepath.Join(home, "disk/p/projects")
	mustWrite(t, filepath.Join(elsewhere, "slug/s1.jsonl"), "own store\n")
	mustLink(t, elsewhere, filepath.Join(profiles, "p/projects"))
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("setup failed: %v", errs)
	}
	if b, err := os.ReadFile(filepath.Join(elsewhere, "slug/s1.jsonl")); err != nil || string(b) != "own store\n" {
		t.Errorf("the other disk must be left intact: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude/projects/slug/s1.jsonl")); err != nil || string(b) != "own store\n" {
		t.Errorf("the shared store must hold the transcript: %q %v", b, err)
	}
	link := filepath.Join(profiles, "p/projects")
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the profile must link at the shared store: %v", err)
	}
	if resolved(link) != resolved(filepath.Join(home, ".claude/projects")) {
		t.Error("the profile's link must point at the shared store")
	}
	if !ProfileOK(profiles, "p") {
		t.Error("a migrated profile must pass the fast path")
	}
}

// #177: a store that refuses writes must not fail the launch — the profile
// keeps its own history, whole, and the deferral is reported.
func TestSetupKeepsTheProfileLaunchableWhenTheStoreRefuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	slug := filepath.Join(home, ".claude/projects/slug")
	mustWrite(t, filepath.Join(slug, "native.jsonl"), "native\n")
	mustWrite(t, filepath.Join(profiles, "p/projects/slug/s1.jsonl"), "session one\n")
	if err := os.Chmod(slug, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(slug, 0o755)
	var notes, errs []string
	if !SetupProfile(profiles, "p", "proxy", func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("a refusing store must not fail the launch: %v", errs)
	}
	projects := filepath.Join(profiles, "p/projects")
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the profile must keep its own history: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(projects, "slug/s1.jsonl")); err != nil || string(b) != "session one\n" {
		t.Errorf("every transcript must survive: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(slug, "native.jsonl")); err != nil || string(b) != "native\n" {
		t.Errorf("the store must be untouched: %q %v", b, err)
	}
	deferred := false
	for _, n := range notes {
		if strings.Contains(n, "the merge into the shared store did not complete") {
			deferred = true
		}
	}
	if !deferred {
		t.Errorf("the failed merge must be reported: %v", notes)
	}
}

// #185: with ~/.claude/projects a link to an unmounted volume, MkdirAll on it
// failed and every launch exited `cannot create profile`. The launch now runs
// on the profile's own history and says why the store was not linked.
func TestSetupKeepsTheProfileOnItsOwnHistoryWhenTheStoreIsUnmounted(t *testing.T) {
	for _, throughStore := range []bool{false, true} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		profiles := filepath.Join(home, ".config/ccmorph/profiles")
		mustLink(t, filepath.Join(home, "gone/projects"), filepath.Join(home, ".claude/projects"))
		projects := filepath.Join(profiles, "p/projects")
		if throughStore {
			mustLink(t, filepath.Join(home, ".claude/projects"), projects)
		} else {
			mustWrite(t, filepath.Join(projects, "slug/s1.jsonl"), "session one\n")
		}
		var notes, errs []string
		if !SetupProfile(profiles, "p", "proxy",
			func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }) {
			t.Fatalf("throughStore=%v: a launch must not fail on an unmounted store: %v", throughStore, errs)
		}
		if len(errs) != 0 {
			t.Errorf("throughStore=%v: no refusal expected: %v", throughStore, errs)
		}
		if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("throughStore=%v: the profile must run on its own real history dir: %v %v", throughStore, fi, err)
		}
		if !throughStore {
			if b, err := os.ReadFile(filepath.Join(projects, "slug/s1.jsonl")); err != nil || string(b) != "session one\n" {
				t.Errorf("the profile's own transcript must survive: %q %v", b, err)
			}
		}
		said := false
		for _, n := range notes {
			if strings.Contains(n, "cannot be reached") {
				said = true
			}
		}
		if !said {
			t.Errorf("throughStore=%v: the unreachable store must be reported: %v", throughStore, notes)
		}
	}
}

// #185: a profile-local paste-cache that links at the user's own dir elsewhere
// is content to merge, like projects — not a refusal. The dir it points at is
// left intact and the profile ends up on the shared store.
func TestSetupMergesAPasteCacheThatLinksElsewhere(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	disk := filepath.Join(home, "disk/paste-cache")
	mustWrite(t, disk+"/a.txt", "pasted\n")
	mustLink(t, disk, filepath.Join(profiles, "p/paste-cache"))
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("a paste-cache link elsewhere must not fail provisioning: %v", errs)
	}
	if fi, err := os.Lstat(filepath.Join(profiles, "p/paste-cache")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the profile must link at the shared paste cache: %v %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude/paste-cache/a.txt")); err != nil || string(b) != "pasted\n" {
		t.Errorf("the pasted text must reach the shared store: %q %v", b, err)
	}
	if b, err := os.ReadFile(disk + "/a.txt"); err != nil || string(b) != "pasted\n" {
		t.Errorf("the user's own dir must keep every file: %q %v", b, err)
	}
}

// #185: a launch that holds the profile's live lock (its claude is running, or
// about to) must never have its shared dirs swapped under it, even though it
// has not written sessions/<pid>.json yet. The merge waits for the next idle
// launch. When that live launch ends, the next launch merges.
func TestSetupNeverSwapsUnderALaunchThatLostTheLock(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	projects := filepath.Join(profiles, "p/projects")
	mustWrite(t, projects+"/slug/s1.jsonl", "session one\n")
	before := inodeOf(t, projects)
	lockDir := filepath.Join(filepath.Dir(profiles), ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(filepath.Join(lockDir, "p.live"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	var notes, errs []string
	log, errf := func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }
	t.Cleanup(func() { ReleaseLive(profiles, "p") })
	if !SetupProfile(profiles, "p", "proxy", log, errf) {
		t.Fatalf("a launch sharing a live profile must still succeed: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the live launch's history must not be swapped: %v %v", fi, err)
	}
	if after := inodeOf(t, projects); after != before {
		t.Errorf("the live launch's history dir must keep its inode: %d -> %d", before, after)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "another launch") {
		t.Errorf("the wait must be reported: %v", notes)
	}
	// That launch ends; the next one merges.
	other.Close()
	// #205: Linux flock(2) removes the existing shared lock before it tries a
	// conversion, so the failed upgrade above must leave this launch holding its
	// shared lock, not none. A probe that can take the exclusive lock here means
	// the session would run through the unprotected window the lock covers.
	probe, err := os.OpenFile(filepath.Join(lockDir, "p.live"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		probe.Close()
		t.Fatal("a failed upgrade must leave the launch holding its live lock")
	}
	probe.Close()
	notes, errs = nil, nil
	if !SetupProfile(profiles, "p", "proxy", log, errf) {
		t.Fatalf("setup after the live launch ended failed: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("an idle profile's history must link at the store: %v %v", fi, err)
	}
}

// #185: a launch that loses the setup lock (another launch holds it) still
// marks itself live, so the winner — which sampled the pid list before this
// session registered — never merges under it.
func TestSetupMarksTheLaunchLiveEvenWhenTheSetupLockIsHeld(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	projects := filepath.Join(profiles, "p/projects")
	mustWrite(t, projects+"/slug/s1.jsonl", "session one\n")
	lockDir := filepath.Join(filepath.Dir(profiles), ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(filepath.Join(lockDir, "p.lock"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("a launch that lost the setup lock must still succeed: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the loser must not swap its own history either: %v %v", fi, err)
	}
	probe, err := os.OpenFile(filepath.Join(lockDir, "p.live"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Fatal("a launch that lost the setup lock must still hold the profile's live lock")
	}
}

// #185: on a home where claude only ever runs through ccmorph, the shared
// history file is never created by claude, ProfileOK skipped its check and
// said provisioned, so the second launch never shared the profile's prompt
// history. Setup creates the shared file it links at, and ProfileOK insists
// on the link.
func TestSetupSharesPromptHistoryOnAHomeClaudeNeverRanOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	shared := filepath.Join(home, ".claude/history.jsonl")
	if _, err := os.Lstat(shared); !os.IsNotExist(err) {
		t.Fatalf("the fixture must start with no shared history: %v", err)
	}
	var errs []string
	errf := func(m string) { errs = append(errs, m) }
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, errf) {
		t.Fatalf("setup failed: %v", errs)
	}
	// claude appends its prompt history through the profile's link.
	f, err := os.OpenFile(filepath.Join(profiles, "p/history.jsonl"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(hrec(100, "first")); err != nil {
		t.Fatal(err)
	}
	f.Close()
	// The second launch: whatever the fast path says, the prompt history is shared.
	if !ProfileOK(profiles, "p") {
		if !SetupProfile(profiles, "p", "proxy", func(string) {}, errf) {
			t.Fatalf("second setup failed: %v", errs)
		}
	}
	if b, err := os.ReadFile(shared); err != nil || !strings.Contains(string(b), "first") {
		t.Fatalf("the second launch must share the prompt history: %q %v", b, err)
	}
	if !ProfileOK(profiles, "p") {
		t.Error("a profile sharing its prompt history must pass the fast path")
	}
}

// #185: a holder stuck on a stalled volume must not hang the launch. HoldLive
// gives up after its deadline and the launch proceeds on its own history.
func TestHoldLiveGivesUpOnAStuckHolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	projects := filepath.Join(profiles, "p/projects")
	mustWrite(t, projects+"/slug/s1.jsonl", "session one\n")
	lockDir := filepath.Join(filepath.Dir(profiles), ".locks")
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		t.Fatal(err)
	}
	holder, err := os.OpenFile(filepath.Join(lockDir, "p.live"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	lockWait = 0
	t.Cleanup(func() { lockWait = 2 * time.Second })
	if f := HoldLive(profiles, "p", nil); f != nil {
		t.Fatal("a stuck holder must not be waited on past the deadline")
	}
	var errs []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("the launch must proceed while the lock is stuck: %v", errs)
	}
	if fi, err := os.Lstat(projects); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("a stuck holder must not have its history swapped either: %v %v", fi, err)
	}
}

// #185: the transcripts v0.5.0 and earlier merges parked beside a profile
// exist nowhere else and are merged into the shared store at the next idle
// launch, so their sessions return to /resume; a file the store already holds
// differently stays in its park and is reported.
func TestSetupMergesParkedTranscriptsIntoTheStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	park := filepath.Join(profiles, "p/projects.premigration.1.1")
	mustWrite(t, park+"/p/S2.jsonl", "park-free\n")
	// A colliding park: the store already holds a different file of this name.
	mustWrite(t, park+"/p/S.jsonl", "park-collide\n")
	mustWrite(t, filepath.Join(home, ".claude/projects/p/S.jsonl"), "shared\n")
	var notes, errs []string
	if !SetupProfile(profiles, "p", "proxy", func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("setup failed: %v", errs)
	}
	if b, err := os.ReadFile(filepath.Join(home, ".claude/projects/p/S2.jsonl")); err != nil || string(b) != "park-free\n" {
		t.Fatalf("a parked transcript must return to the shared store: %q %v", b, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude/projects/p/S.jsonl")); string(b) != "shared\n" {
		t.Fatalf("the store's own file must never be overwritten: %q", b)
	}
	if b, err := os.ReadFile(park + "/p/S.jsonl"); err != nil || string(b) != "park-collide\n" {
		t.Fatalf("a colliding park file must stay in its park: %q %v", b, err)
	}
	if _, err := os.Lstat(park + "/p/S2.jsonl"); !os.IsNotExist(err) {
		t.Errorf("a merged park file must leave the park: %v", err)
	}
	sawReport := false
	for _, n := range notes {
		if strings.Contains(n, "already holds a different copy") {
			sawReport = true
		}
	}
	if !sawReport {
		t.Errorf("the kept park file must be reported: %v", notes)
	}
}

// #185: the shared history is a link at the profile's own real history file —
// the reverse of the shape setup makes. Merging then deleted the link's target
// and left the store pointing at itself, so every later append failed ELOOP.
// The store path becomes the real file and the profile links at it.
// #205: the repair must never unlink the shared file before its replacement is
// there — a failed copy leaves both where they were, logged, never a failed
// launch — or every profile linking at the store is left with no history.
func TestSetupKeepsHistoryWhoseStoreLinksAtIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	profiles := filepath.Join(home, ".config/ccmorph/profiles")
	own := filepath.Join(profiles, "p/history.jsonl")
	mustWrite(t, own, hrec(100, "kept"))
	shared := filepath.Join(home, ".claude/history.jsonl")
	mustLink(t, own, shared)
	// The copy into the store fails before anything is published.
	linkFile = func(a, b string) error { return &os.LinkError{Op: "link", Old: a, New: b, Err: syscall.EEXIST} }
	t.Cleanup(func() { linkFile = os.Link })
	var notes, errs []string
	if !SetupProfile(profiles, "p", "proxy", func(s string) { notes = append(notes, s) }, func(m string) { errs = append(errs, m) }) {
		t.Fatalf("a repair whose copy failed must not fail the launch: %v", errs)
	}
	if fi, err := os.Lstat(shared); err != nil {
		t.Fatalf("a failed repair must leave the shared history in place: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("a failed repair must not touch the shared path: %v", fi)
	}
	if fi, err := os.Lstat(own); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("a failed repair must leave the profile on its own copy: %v %v", fi, err)
	}
	if b, err := os.ReadFile(own); err != nil || !strings.Contains(string(b), "kept") {
		t.Fatalf("the records must survive a failed repair: %q %v", b, err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "kept profile-local 'history.jsonl'") {
		t.Errorf("a failed repair must be logged: %v", notes)
	}
	for _, pat := range []string{home + "/.claude/history.jsonl.ccm-repair*", home + "/.claude/.ccm-copy-*"} {
		if left, _ := filepath.Glob(pat); len(left) != 0 {
			t.Errorf("a failed repair must leave no temp behind: %v", left)
		}
	}
	// The next launch, with the copy working, completes the repair.
	linkFile = os.Link
	var errs2 []string
	if !SetupProfile(profiles, "p", "proxy", func(string) {}, func(m string) { errs2 = append(errs2, m) }) {
		t.Fatalf("setup failed: %v", errs2)
	}
	if fi, err := os.Lstat(shared); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the shared history must become a real file: %v %v", fi, err)
	}
	if b, err := os.ReadFile(shared); err != nil || !strings.Contains(string(b), "kept") {
		t.Fatalf("the records must survive: %q %v", b, err)
	}
	if fi, err := os.Lstat(own); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the profile must link at the shared file: %v %v", fi, err)
	}
	f, err := os.OpenFile(own, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("appending through the profile link must work: %v", err)
	}
	if _, err := f.WriteString(hrec(200, "later")); err != nil {
		t.Fatalf("appending through the profile link must work: %v", err)
	}
	f.Close()
	if b, _ := os.ReadFile(shared); !strings.Contains(string(b), "later") {
		t.Fatalf("an append through the profile link must reach the shared file: %q", b)
	}
	if !ProfileOK(profiles, "p") {
		t.Error("a profile sharing its prompt history must pass the fast path")
	}
}
