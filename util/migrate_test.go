package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if isLink(real) {
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
