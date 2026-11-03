package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func leftovers2(t *testing.T, from string) {
	t.Helper()
	baks, _ := filepath.Glob(from + ".premigration.*")
	for _, b := range baks {
		filepath.Walk(b, func(p string, fi os.FileInfo, err error) error {
			if err == nil && !fi.IsDir() {
				t.Errorf("every entry must leave the backup: %q", p)
			}
			return nil
		})
	}
}

// A relative link migrates to one resolvable absolute target.
func TestMigrateProjectsRewritesARelativeLink(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/defs/agent.md", "agent")
	mustLink(t, "../defs/agent.md", from+"/links/agent.md")
	if !migrateProjects(from, to) {
		t.Fatal("migrate failed")
	}
	if b, err := os.ReadFile(to + "/links/agent.md"); err != nil || string(b) != "agent" {
		t.Fatalf("the migrated agent link must resolve to its definition: %q %v", b, err)
	}
}

// #399: a relative target whose own name ends in a newline migrates literally.
func TestMigrateProjectsKeepsALinkTargetEndingInANewline(t *testing.T) {
	from, to := migrateRoots(t)
	mustWrite(t, from+"/notes.md\n", "agent")
	mustLink(t, "notes.md\n", from+"/alias.md")
	if !migrateProjects(from, to) {
		t.Fatal("migrate failed")
	}
	linkIs(t, to+"/alias.md", to+"/notes.md\n", "a relative target ending in a newline must migrate literally (#399)")
	if _, err := os.Stat(to + "/alias.md"); err != nil {
		t.Errorf("the migrated link must resolve to the migrated definition: %v", err)
	}
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
	if !migrateProjects(from, to) {
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
	leftovers2(t, from)
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
	if !migrateProjects(from, to) {
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
}
