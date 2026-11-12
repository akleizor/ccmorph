package util

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func linkIs(t *testing.T, link, want, why string) {
	t.Helper()
	if got, err := os.Readlink(link); err != nil || got != want {
		t.Errorf("%s: %s -> %q (%v), want %q", why, link, got, err, want)
	}
}

func migrateRoots(t *testing.T) (from, to string) {
	root := t.TempDir()
	return filepath.Join(root, "profiles/p1/agents"), filepath.Join(root, "claude/agents")
}

func drop(string) {}

// consumed asserts the migrated tree left nothing behind: the dir itself is
// gone and no backup was created (#177: v0.5.0 left one per launch).
func consumed(t *testing.T, from string) {
	t.Helper()
	if _, err := os.Lstat(from); err == nil {
		t.Errorf("the migrated tree must be consumed: %q remains", from)
	}
	if left, _ := filepath.Glob(from + ".*"); len(left) != 0 {
		t.Errorf("no backup may remain when nothing diverged: %v", left)
	}
}

// A relative link migrates to one resolvable absolute target.
func TestMigrateProjectsRewritesARelativeLink(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/defs/agent.md", "agent")
	mustLink(t, "../defs/agent.md", from+"/links/agent.md")
	if !migrateProjects(from, to, drop) {
		t.Fatal("migrate failed")
	}
	if b, err := os.ReadFile(to + "/links/agent.md"); err != nil || string(b) != "agent" {
		t.Fatalf("the migrated agent link must resolve to its definition: %q %v", b, err)
	}
	consumed(t, from)
}

// #399: a relative target whose own name ends in a newline migrates literally.
func TestMigrateProjectsKeepsALinkTargetEndingInANewline(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/notes.md\n", "agent")
	mustLink(t, "notes.md\n", from+"/alias.md")
	if !migrateProjects(from, to, drop) {
		t.Fatal("migrate failed")
	}
	linkIs(t, to+"/alias.md", to+"/notes.md\n", "a relative target ending in a newline must migrate literally (#399)")
	if _, err := os.Stat(to + "/alias.md"); err != nil {
		t.Errorf("the migrated link must resolve to the migrated definition: %v", err)
	}
	consumed(t, from)
}

// #167, #267, #376, #404: links whose parents do not resolve keep their textual
// target and dangle where they always did, never landing on a shared look-alike.
func TestMigrateProjectsKeepsDanglingLinksDangling(t *testing.T) {
	from, to := migrateRoots(t)
	root := filepath.Dir(filepath.Dir(from))
	mustWrite(t, from+"/defs/agent.md", "agent")
	mustLink(t, "../nodir/f.md", from+"/f.md")
	mustLink(t, "../defs/agent.md", from+"/deep/agent.md")
	mustLink(t, "../nodir2/x.md", from+"/deep/x.md")
	// Two `..` more than the backup path is deep, so the chain overshoots the root.
	phys, _ := filepath.EvalSymlinks(filepath.Dir(from))
	up := strings.Repeat("../", strings.Count(phys, "/")+3)
	mustLink(t, up+"nodir3/..", from+"/root.md")
	// `out` dangles, so `out/..` resolves nowhere; `real.md` is what a textual
	// collapse would land on, and `a.md` would collapse onto itself.
	mustWrite(t, to+"/real.md", "shared real")
	mustLink(t, filepath.Join(root, "gone"), from+"/out")
	mustLink(t, "out/../real.md", from+"/x.md")
	mustLink(t, "out/../a.md", from+"/a.md")
	mustWrite(t, to+"/nodir/nl.md", "decoy")
	mustLink(t, "nodir\n/nl.md", from+"/nl.md")
	mustWrite(t, from+"/real\n/agent.md", "INTENDED")
	mustWrite(t, to+"/real/agent.md", "DECOY")
	mustLink(t, "real\n", from+"/ext")
	mustWrite(t, from+"/we\nird.md", "we-ird")
	if !migrateProjects(from, to, drop) {
		t.Fatal("migrate failed")
	}

	if got, err := os.Readlink(to + "/f.md"); err != nil || !strings.HasSuffix(got, "/profiles/p1/nodir/f.md") || strings.Contains(got, "\n") {
		t.Errorf("a link whose target parent is missing must migrate, dangling with its textual target: %q %v", got, err)
	}
	if _, err := os.Stat(to + "/f.md"); err == nil {
		t.Error("a link to a missing parent must stay dangling")
	}
	linkIs(t, to+"/deep/agent.md", to+"/defs/agent.md", "a link whose parent resolves keeps its physical resolution")
	if _, err := os.Stat(to + "/deep/agent.md"); err != nil {
		t.Error("the resolved migrated link must still resolve")
	}
	linkIs(t, to+"/deep/x.md", to+"/nodir2/x.md", "a missing parent inside the migrated tree remaps into the shared store")
	linkIs(t, to+"/x.md", to+"/out/../real.md", "a dangling mid-path component stays in the target (#376)")
	if _, err := os.Stat(to + "/x.md"); err == nil {
		t.Error("the link through the dangling component must dangle, not load the shared real.md (#376)")
	}
	if b, _ := os.ReadFile(to + "/real.md"); string(b) != "shared real" {
		t.Error("the migration must not overwrite an existing file in the shared store")
	}
	linkIs(t, to+"/a.md", to+"/out/../a.md", "the self-pointing shape keeps the dangling component too (#376)")
	linkIs(t, to+"/nl.md", to+"/nodir\n/nl.md", "a component ending in a newline survives (#267)")
	if _, err := os.Stat(to + "/nl.md"); err == nil {
		t.Error("the newline-ending component must dangle, not land on the shared decoy (#267)")
	}
	if b, _ := os.ReadFile(to + "/nodir/nl.md"); string(b) != "decoy" {
		t.Error("the migration must not overwrite an existing file in the shared store")
	}
	// An overshooting chain clamps at the root and keeps the unresolved tail.
	linkIs(t, to+"/root.md", "/nodir3/..", "an overshooting fallback keeps one root and the unresolved tail")
	if b, _ := os.ReadFile(to + "/ext/agent.md"); string(b) != "INTENDED" {
		t.Error("a link whose target ends in a newline must load its own definition, not the decoy (#404)")
	}
	linkIs(t, to+"/ext", to+"/real\n", "the newline ending a link target survives (#404)")
	if b, _ := os.ReadFile(to + "/we\nird.md"); string(b) != "we-ird" {
		t.Error("a name containing a newline must migrate with its file (#404)")
	}
	consumed(t, from)
}

// #400: a name containing a newline is one entry, and nothing outside the tree moves.
func TestMigrateProjectsTreatsANewlineNameAsOneEntry(t *testing.T) {
	from, to := migrateRoots(t)
	cwd := t.TempDir()
	mustWrite(t, from+"/sub/a\nb.md", "newline agent")
	mustWrite(t, from+"/plain.md", "ordinary agent")
	mustLink(t, "../plain.md", from+"/sub/l\nk.md")
	mustWrite(t, cwd+"/b.md", "unrelated")
	old, _ := os.Getwd()
	os.Chdir(cwd)
	defer os.Chdir(old)
	if !migrateProjects(from, to, drop) {
		t.Fatal("migrate failed")
	}
	if b, _ := os.ReadFile(to + "/sub/a\nb.md"); string(b) != "newline agent" {
		t.Error("a name containing a newline must migrate as one entry (#400)")
	}
	linkIs(t, to+"/sub/l\nk.md", to+"/plain.md", "a link whose name contains a newline migrates as one entry (#400)")
	if b, _ := os.ReadFile(to + "/plain.md"); string(b) != "ordinary agent" {
		t.Error("ordinary names must still migrate (#400)")
	}
	if b, _ := os.ReadFile(cwd + "/b.md"); string(b) != "unrelated" {
		t.Error("a same-named file in the caller's cwd must not be moved (#400)")
	}
	if _, err := os.Stat(to + "/b.md"); err == nil {
		t.Error("nothing from outside the tree may enter the shared store (#400)")
	}
	consumed(t, from)
}

// #177: a same-name file the store already holds divergently is kept side by
// side in a sibling '.premigration.' park and reported; the store's copy is
// never overwritten and neither is lost. A link the store cannot take is parked
// as a link with its literal target. A byte-identical duplicate needs no park.
func TestMigrateProjectsParksADivergentDuplicate(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/private", "profile content")
	mustWrite(t, from+"/same", "shared content")
	mustWrite(t, from+"/new", "new content")
	mustWrite(t, to+"/private", "shared content")
	mustWrite(t, to+"/same", "shared content")
	mustLink(t, "private", from+"/alias")
	mustWrite(t, to+"/alias", "shared alias")
	var notes []string
	if !migrateProjects(from, to, func(s string) { notes = append(notes, s) }) {
		t.Fatal("migrate failed")
	}
	for p, want := range map[string]string{to + "/new": "new content", to + "/private": "shared content", to + "/same": "shared content"} {
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Errorf("%s = %q, want %q", p, b, want)
		}
	}
	divs, _ := filepath.Glob(from + ".premigration.*")
	if len(divs) != 1 {
		t.Fatalf("want one park, got %v", divs)
	}
	if got, err := os.Readlink(divs[0] + "/alias"); err != nil || got != "private" {
		t.Errorf("a parked link must stay a link with its literal target: %q %v", got, err)
	}
	if b, _ := os.ReadFile(to + "/alias"); string(b) != "shared alias" {
		t.Errorf("the store's alias must not be overwritten: %q", b)
	}
	if b, _ := os.ReadFile(divs[0] + "/private"); string(b) != "profile content" {
		t.Errorf("the divergent profile copy must be kept: %q", b)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "alias") || !strings.Contains(notes[1], "private") {
		t.Errorf("each parked entry must be reported once: %v", notes)
	}
	if _, err := os.Lstat(from); err == nil {
		t.Error("the migrated tree must be consumed")
	}
}

// A session carried from profile b into profile a reached the store first as a
// link; b's real transcript must then replace that link, never stay behind (#173).
func TestMigrateCarriedLinkGivesWayToTheTranscript(t *testing.T) {
	root := t.TempDir()
	a, b, store := filepath.Join(root, "p/a/projects"), filepath.Join(root, "p/b/projects"), filepath.Join(root, "claude/projects")
	real := filepath.Join(b, "slug/s1.jsonl")
	mustWrite(t, real, "{}\n")
	mustLink(t, real, filepath.Join(a, "slug/s1.jsonl"))
	if !migrateProjects(a, store, drop) {
		t.Fatal("migrate a")
	}
	if !migrateProjects(b, store, drop) {
		t.Fatal("migrate b")
	}
	dest := filepath.Join(store, "slug/s1.jsonl")
	fi, err := os.Lstat(dest)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("store holds %v (%v), want the real transcript", fi, err)
	}
	if body, _ := os.ReadFile(dest); string(body) != "{}\n" {
		t.Fatalf("transcript body %q", body)
	}
}

// #177: a store that refuses writes fails the migration and every original
// stays where it was — the profile remains on its own store, whole, with no
// backup dir created (v0.5.0 renamed the tree aside before touching the store).
func TestMigrateProjectsLeavesTheTreeWholeWhenTheStoreRefuses(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/a", "one")
	mustWrite(t, from+"/sub/b", "two")
	mustWrite(t, to+"/kept", "store content")
	if err := os.Chmod(to, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(to, 0o755)
	if migrateProjects(from, to, drop) {
		t.Fatal("a store that refuses writes must fail the migration")
	}
	for p, want := range map[string]string{from + "/a": "one", from + "/sub/b": "two"} {
		if b, err := os.ReadFile(p); err != nil || string(b) != want {
			t.Errorf("%s must survive a failed migration: %q %v", p, b, err)
		}
	}
	if b, _ := os.ReadFile(to + "/kept"); string(b) != "store content" {
		t.Errorf("the store must be untouched: %q", b)
	}
	if left, _ := filepath.Glob(from + ".*"); len(left) != 0 {
		t.Errorf("a failed migration must leave no backup: %v", left)
	}
}

// #177: a file that lands under the tree while the merge consumes it — a
// session a contended launch starts writes into the real dir, and a live
// session appends to a transcript already copied — survives, and the profile
// is not linked at a store that lacks it. The report hook fires after the
// walk, while the tree is being consumed: exactly that window.
func TestMigrateProjectsKeepsWhatArrivesWhileItMerges(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/private", "profile content")
	mustWrite(t, from+"/walked", "walked content\n")
	mustWrite(t, to+"/private", "shared content")
	arrived := false
	ok := migrateProjects(from, to, func(string) {
		if !arrived {
			arrived = true
			mustWrite(t, from+"/late/s2.jsonl", "late\n")
			mustWrite(t, from+"/walked", "walked content\nappended\n")
		}
	})
	if ok {
		t.Fatal("a tree that changed while it was merged must not be linked away")
	}
	if b, err := os.ReadFile(from + "/late/s2.jsonl"); err != nil || string(b) != "late\n" {
		t.Errorf("a file that arrived during the merge must survive: %q %v", b, err)
	}
	if b, err := os.ReadFile(from + "/walked"); err != nil || string(b) != "walked content\nappended\n" {
		t.Errorf("a line appended during the merge must survive: %q %v", b, err)
	}
	// The walked file was copied before the append; the store keeps that copy
	// and the profile keeps the fuller original for the next launch.
	if b, _ := os.ReadFile(to + "/walked"); string(b) != "walked content\n" {
		t.Errorf("the verified copy stays in the store: %q", b)
	}
	if _, err := os.Lstat(filepath.Join(filepath.Dir(from), filepath.Base(from))); err != nil {
		t.Errorf("the profile's own dir must stay: %v", err)
	}
}

// #177: a profile link at a real file already in the store drops the link and
// leaves the file. The link was once written over it, a loop onto itself. Also
// through another profile's projects link, the shape this machine carries.
func TestMigrateCarriedLinkAtARealStoreFileKeepsIt(t *testing.T) {
	for _, through := range []bool{false, true} {
		root := t.TempDir()
		store := filepath.Join(root, "store")
		real := filepath.Join(store, "p", "s.jsonl")
		mustWrite(t, real, "transcript\n")
		target := real
		if through {
			other := filepath.Join(root, "other")
			if err := os.MkdirAll(other, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(store, filepath.Join(other, "projects")); err != nil {
				t.Fatal(err)
			}
			target = filepath.Join(other, "projects", "p", "s.jsonl")
		}
		own := filepath.Join(root, "own")
		if err := os.MkdirAll(filepath.Join(own, "p"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(own, "p", "s.jsonl")); err != nil {
			t.Fatal(err)
		}
		if !migrateProjects(own, store, nil) {
			t.Fatalf("through=%v: merge failed", through)
		}
		b, err := os.ReadFile(real)
		if err != nil || string(b) != "transcript\n" {
			t.Fatalf("through=%v: real transcript lost: %v %q", through, err, b)
		}
		if paths.IsSymlink(real) {
			t.Fatalf("through=%v: the real file became a link", through)
		}
	}
}

// #177: a copied transcript keeps its age, or every session looks just modified.
func TestMigrateKeepsTheModificationTime(t *testing.T) {
	root := t.TempDir()
	own, store := filepath.Join(root, "own"), filepath.Join(root, "store")
	f := filepath.Join(own, "p", "s.jsonl")
	mustWrite(t, f, "x\n")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(f, old, old); err != nil {
		t.Fatal(err)
	}
	if !migrateProjects(own, store, nil) {
		t.Fatal("merge failed")
	}
	fi, err := os.Stat(filepath.Join(store, "p", "s.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !fi.ModTime().Equal(old) {
		t.Errorf("mtime %v, want %v", fi.ModTime(), old)
	}
}

// #185: hop plants the transcript AND its sidecar dir as links into the source
// profile's tree. When the source later merges, the sidecar dir link must give
// way first, so the subagent transcripts are copied into the store — before
// this fix the parent resolved through the link, os.SameFile answered true, and
// the removal deleted them from both places.
func TestMigrateCarriedSidecarDirKeepsItsFiles(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "p/projects"), filepath.Join(root, "claude/projects")
	mustWrite(t, from+"/slug/S.jsonl", "main\n")
	mustWrite(t, from+"/slug/S/subagents/a.jsonl", "sub\n")
	mustLink(t, from+"/slug/S.jsonl", to+"/slug/S.jsonl")
	mustLink(t, from+"/slug/S", to+"/slug/S")
	if !migrateProjects(from, to, drop) {
		t.Fatal("the merge must complete")
	}
	if fi, err := os.Lstat(to + "/slug/S"); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the store's sidecar dir must be a real dir: %v %v", fi, err)
	}
	if b, err := os.ReadFile(to + "/slug/S/subagents/a.jsonl"); err != nil || string(b) != "sub\n" {
		t.Fatalf("the sidecar transcript must reach the store: %q %v", b, err)
	}
	if fi, err := os.Lstat(to + "/slug/S.jsonl"); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the store's transcript must be a real file: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(to + "/slug/S.jsonl"); string(b) != "main\n" {
		t.Fatalf("transcript body %q", b)
	}
	consumed(t, from)
}

// #185: a walk-time stat failure leaves no FileInfo; stillUnchanged must answer
// false (keep the file) instead of dereferencing nil and killing the core.
func TestStillUnchangedWithoutAWalkTimeStatKeepsTheFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	mustWrite(t, p, "x\n")
	if stillUnchanged(walked{f: p}) {
		t.Fatal("an entry with no walk-time stat must not be called unchanged")
	}
}

// #185: a profile's absolute link whose target is the very store path it would
// be written to (the target was pruned) was re-created as a link to itself. It
// is parked beside the profile instead, never published as an ELOOP entry.
func TestMigrateProjectsParksALinkToItsOwnStorePath(t *testing.T) {
	from, to := migrateRoots(t)
	want := to + "/slug/x.jsonl"
	mustLink(t, want, from+"/slug/x.jsonl")
	if !migrateProjects(from, to, drop) {
		t.Fatal("the merge must complete")
	}
	if _, err := os.Lstat(want); err == nil {
		t.Fatal("a link to the store path it would be written to must not be published")
	}
	parks, _ := filepath.Glob(from + ".premigration.*")
	if len(parks) != 1 {
		t.Fatalf("want one park beside the profile, got %v", parks)
	}
	got, err := os.Readlink(parks[0] + "/slug/x.jsonl")
	if err != nil || got != want {
		t.Fatalf("the park must hold the literal link (%q %v)", got, err)
	}
}

// #185: a store on exFAT, FAT or an SMB mount refuses link(2). The verified
// copy is published by a reserved rename instead, and the transcript joins the
// store rather than every launch reporting an incomplete merge.
func TestMigrateProjectsWithoutHardLinks(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/slug/s.jsonl", "x\n")
	linkFile = func(a, b string) error {
		return &os.LinkError{Op: "link", Old: a, New: b, Err: syscall.ENOTSUP}
	}
	t.Cleanup(func() { linkFile = os.Link })
	if !migrateProjects(from, to, drop) {
		t.Fatal("a store without hard links must still take the transcript")
	}
	if b, err := os.ReadFile(to + "/slug/s.jsonl"); err != nil || string(b) != "x\n" {
		t.Fatalf("the transcript must reach the store: %q %v", b, err)
	}
	if left, _ := filepath.Glob(to + "/slug/.ccm-copy-*"); len(left) != 0 {
		t.Errorf("no temp file may remain: %v", left)
	}
	consumed(t, from)
}

// #185/#177: a line appended to a divergent file while its park copy runs is
// measured before the copy, the way the store-copy guard measures it at the
// walk. The original stays with the line; the parked copy is already safe.
func TestMigrateDivergentParkKeepsALineAppendedDuringItsCopy(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/dup", "profile\n")
	mustWrite(t, to+"/dup", "shared\n")
	appended := false
	ok := migrateProjects(from, to, func(string) {
		if appended {
			return
		}
		appended = true
		f, err := os.OpenFile(from+"/dup", os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString("late\n"); err != nil {
			t.Fatal(err)
		}
	})
	if ok {
		t.Fatal("a tree that changed while it merged must not be called consumed")
	}
	if b, err := os.ReadFile(from + "/dup"); err != nil || string(b) != "profile\nlate\n" {
		t.Fatalf("the original must keep the line appended during the park copy: %q %v", b, err)
	}
	parks, _ := filepath.Glob(from + ".premigration.*")
	if len(parks) != 1 {
		t.Fatalf("want one park, got %v", parks)
	}
	if b, err := os.ReadFile(parks[0] + "/dup"); err != nil || string(b) != "profile\n" {
		t.Fatalf("the park must hold the copy that verified: %q %v", b, err)
	}
}

func TestMigrateReplacesADanglingStoreLinkAtAParentDir(t *testing.T) {
	root := t.TempDir()
	from, to := filepath.Join(root, "from"), filepath.Join(root, "to")
	transcript := filepath.Join(from, "proj", "sess", "subagents", "a.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte("real\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(to, "proj"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(to, "proj", "sess")); err != nil {
		t.Fatal(err)
	}
	if !migrateProjects(from, to, nil) {
		t.Fatal("a dangling store link stopped the merge")
	}
	got, err := os.ReadFile(filepath.Join(to, "proj", "sess", "subagents", "a.jsonl"))
	if err != nil || string(got) != "real\n" {
		t.Fatalf("transcript = %q, %v", got, err)
	}
}

// parkRoots lays out a profile dir with one park and an empty shared store.
func parkRoots(t *testing.T) (dir, park, store string) {
	t.Helper()
	root := t.TempDir()
	dir, store = filepath.Join(root, "p"), filepath.Join(root, "claude/projects")
	if err := os.MkdirAll(store, 0o777); err != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "projects.premigration.1.1"), store
}

// #204: a stalled volume answers the walk with an error and is back by the
// sweep, so the sweep can reach what the walk never saw. chmod alone cannot show
// it: RemoveAll cannot enter the dir either, so the mode comes back mid-walk,
// when the later file publishes.
func TestMergeParksKeepsWhatItsWalkCouldNotRead(t *testing.T) {
	dir, park, store := parkRoots(t)
	mustWrite(t, park+"/a/locked.jsonl", "only copy\n")
	mustWrite(t, park+"/z/free.jsonl", "free\n")
	if err := os.Chmod(park+"/a", 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(park+"/a", 0o700) })
	linkFile = func(a, b string) error {
		if err := os.Chmod(park+"/a", 0o700); err != nil {
			return err
		}
		return os.Link(a, b)
	}
	t.Cleanup(func() { linkFile = os.Link })
	var notes []string
	mergeParks(dir, store, func(s string) { notes = append(notes, s) })
	if b, err := os.ReadFile(park + "/a/locked.jsonl"); err != nil || string(b) != "only copy\n" {
		t.Fatalf("what the walk could not read must stay in its park: %q %v", b, err)
	}
	if b, err := os.ReadFile(store + "/z/free.jsonl"); err != nil || string(b) != "free\n" {
		t.Errorf("what the walk could read still merges: %q %v", b, err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], park+"/a") {
		t.Errorf("the unread subtree must be reported once: %v", notes)
	}
}

// #204: a link whose target answers EACCES or ETIMEDOUT is a mount refusing or
// stalled, not a link naming nothing: it holds the user's store, so neither the
// profile merge nor the park sweep may unlink it.
func TestMergeKeepsAStoreLinkItCannotStat(t *testing.T) {
	merges := map[string]func(dir, from, store string){
		"profile": func(_, from, store string) { migrateProjects(from, store, drop) },
		"park":    func(dir, _, store string) { mergeParks(dir, store, drop) },
	}
	shapes := map[string]struct{ held, file string }{
		"parent-issue-204": {"sess/kept.jsonl", "proj/sess/a.jsonl"},
		"leaf-issue-204":   {"sess", "proj/sess"},
	}
	for mname, merge := range merges {
		for sname, shape := range shapes {
			for _, errno := range []syscall.Errno{syscall.EACCES, syscall.ETIMEDOUT} {
				t.Run(mname+"/"+sname+"/"+errno.Error(), func(t *testing.T) {
					dir, park, store := parkRoots(t)
					mount := filepath.Join(filepath.Dir(dir), "mount")
					mustWrite(t, mount+"/"+shape.held, "the user's store\n")
					mustWrite(t, park+"/"+shape.file, "parked\n")
					link := store + "/proj/sess"
					mustLink(t, mount+"/sess", link)
					statTarget = func(p string) (os.FileInfo, error) {
						if p == link {
							return nil, &os.PathError{Op: "stat", Path: p, Err: errno}
						}
						return os.Stat(p)
					}
					t.Cleanup(func() { statTarget = os.Stat })
					merge(dir, park, store)
					linkIs(t, link, mount+"/sess", "a link it cannot stat stays")
					if b, err := os.ReadFile(mount + "/" + shape.held); err != nil || string(b) != "the user's store\n" {
						t.Errorf("the link's target must survive: %q %v", b, err)
					}
				})
			}
		}
	}
}

// #204: a store link that provably names nothing gives way to a parked file the
// way it does in the profile merge (#184), so the session returns to /resume.
func TestMergeParksClearsADeadStoreLink(t *testing.T) {
	cases := map[string]struct {
		file, link string
		target     func(root, link string) string
	}{
		"leaf-dangling-issue-204":         {"proj/s.jsonl", "proj/s.jsonl", func(root, _ string) string { return root + "/gone" }},
		"parent-dangling-issue-204":       {"proj/sess/subagents/a.jsonl", "proj/sess", func(root, _ string) string { return root + "/gone" }},
		"leaf-self-link-issue-204":        {"proj/s.jsonl", "proj/s.jsonl", func(_, link string) string { return link }},
		"parent-through-a-file-issue-204": {"proj/sess/a.jsonl", "proj/sess", func(root, _ string) string { return root + "/plain/x" }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir, park, store := parkRoots(t)
			root := filepath.Dir(dir)
			mustWrite(t, root+"/plain", "not a dir\n")
			mustWrite(t, park+"/"+c.file, "parked\n")
			link := store + "/" + c.link
			mustLink(t, c.target(root, link), link)
			var notes []string
			mergeParks(dir, store, func(s string) { notes = append(notes, s) })
			if b, err := os.ReadFile(store + "/" + c.file); err != nil || string(b) != "parked\n" {
				t.Fatalf("the parked file must reach the store: %q %v (%v)", b, err, notes)
			}
			if paths.IsSymlink(link) {
				t.Errorf("the dead link must have given way: %s", link)
			}
			if len(notes) != 0 {
				t.Errorf("a merged file reports nothing: %v", notes)
			}
			if _, err := os.Lstat(park); err == nil {
				t.Errorf("an emptied park goes: %s", park)
			}
		})
	}
}

// #204: a store path that reaches the parked file itself holds no second copy.
// A leaf link gives way to the file it names; a link on a parent keeps the file,
// and neither is deleted as a duplicate of itself.
func TestMergeParksKeepsAFileTheStoreReachesThroughALink(t *testing.T) {
	t.Run("leaf-issue-204", func(t *testing.T) {
		dir, park, store := parkRoots(t)
		mustWrite(t, park+"/proj/s.jsonl", "parked\n")
		mustLink(t, park+"/proj/s.jsonl", store+"/proj/s.jsonl")
		mergeParks(dir, store, drop)
		if b, err := os.ReadFile(store + "/proj/s.jsonl"); err != nil || string(b) != "parked\n" {
			t.Fatalf("the store's link must become the transcript: %q %v", b, err)
		}
		if paths.IsSymlink(store + "/proj/s.jsonl") {
			t.Error("the store must hold the real file")
		}
	})
	t.Run("parent-issue-204", func(t *testing.T) {
		dir, park, store := parkRoots(t)
		mustWrite(t, park+"/proj/s.jsonl", "parked\n")
		mustLink(t, park+"/proj", store+"/proj")
		var notes []string
		mergeParks(dir, store, func(s string) { notes = append(notes, s) })
		if b, err := os.ReadFile(park + "/proj/s.jsonl"); err != nil || string(b) != "parked\n" {
			t.Fatalf("the only copy must stay: %q %v", b, err)
		}
		if len(notes) != 1 || strings.Contains(notes[0], "different copy") {
			t.Errorf("the kept file must be reported truthfully: %v", notes)
		}
	})
}

// #204: a store link it cannot stat is no proof of a different copy.
func TestMergeParksNeverCallsAnUnreadableLinkADifferentCopy(t *testing.T) {
	dir, park, store := parkRoots(t)
	mount := filepath.Join(filepath.Dir(dir), "mount")
	mustWrite(t, mount+"/s.jsonl", "the user's store\n")
	mustWrite(t, park+"/proj/s.jsonl", "parked\n")
	link := store + "/proj/s.jsonl"
	mustLink(t, mount+"/s.jsonl", link)
	statTarget = func(p string) (os.FileInfo, error) {
		if p == link {
			return nil, &os.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
		}
		return os.Stat(p)
	}
	t.Cleanup(func() { statTarget = os.Stat })
	var notes []string
	mergeParks(dir, store, func(s string) { notes = append(notes, s) })
	if len(notes) != 1 || !strings.Contains(notes[0], "could not be read") {
		t.Errorf("the kept file must be reported as unreadable, not different: %v", notes)
	}
	if b, err := os.ReadFile(park + "/proj/s.jsonl"); err != nil || string(b) != "parked\n" {
		t.Fatalf("the parked file must stay: %q %v", b, err)
	}
}
