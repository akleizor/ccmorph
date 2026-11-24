package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// GH-837: a descriptor names its path until the path is unlinked or replaced.
func TestNamesPathFollowsTheFileThePathNames(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "x.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !NamesPath(f, lock) {
		t.Fatal("a freshly opened file names its path")
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if NamesPath(f, lock) {
		t.Fatal("an unlinked file no longer names the path")
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if NamesPath(f, lock) {
		t.Fatal("a replacement at the path is not the file this descriptor holds")
	}
}
