package shim

import (
	"os"
	"path/filepath"
	"testing"
)

// The write every ccmorph state file goes through: the target ends up holding
// exactly the data at the mode asked, and the temp it was written through is
// gone — a reader never sees a half-written file.
func TestAtomicWriteReplacesTheTargetAndRemovesItsTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")
	if err := AtomicWrite(path, []byte("body"), 0o600, ".prefix."); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "body" {
		t.Fatalf("target holds %q, %v", b, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(err, st)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "target.json" {
		t.Fatalf("the temp survived the write: %v", entries)
	}
}

// A rename that cannot land leaves the target as it was and no temp behind:
// the half-written file beside the target is what a truncate-in-place write
// leaves when it dies midway, the corruption every caller guards against.
func TestAtomicWriteRemovesItsTempWhenTheRenameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target.json")
	// A directory where the file belongs: renaming a file over it always fails.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(path, []byte("body"), 0o600, ".prefix."); err == nil {
		t.Fatal("renaming a file over a directory must fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("a failed write left its temp behind: %v", entries)
	}
}
