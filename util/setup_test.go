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
