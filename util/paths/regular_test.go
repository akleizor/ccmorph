package paths

import (
	"os"
	"path/filepath"
	"testing"
)

// #636: migrate asked IsFile (Stat) and IsSymlink (Lstat) of every cache;
// IsRegularFile is the one Lstat that answers both.
func TestIsRegularFileIsOneLstat(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]bool{file: true, link: false, dir: false, filepath.Join(dir, "missing"): false} {
		if got := IsRegularFile(path); got != want {
			t.Errorf("IsRegularFile(%s) = %v, want %v", path, got, want)
		}
		if got := IsFile(path) && !IsSymlink(path); got != want {
			t.Errorf("the pair it replaces disagrees on %s", path)
		}
	}
}
