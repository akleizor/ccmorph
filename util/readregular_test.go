package util

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The session-end channel sits at predictable names in a shared TMPDIR, so a
// reader there trusts only a regular file this account owns and only a limit's
// worth of it: another account's plant, a file others can rewrite, a FIFO and a
// link are all refused before their bytes are read (#410).
func TestReadPrivateRegularTrustsOnlyAnOwnedPrivateFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("sid-410"), 0o600); err != nil {
			t.Fatal(err)
		}
		// WriteFile keeps an existing file's mode and umask may clear bits; the
		// case's mode is the one under test.
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	u := os.Geteuid()
	for _, c := range []struct {
		name  string
		path  string
		owner int
		limit int64
		want  bool
	}{
		{"this account's own private record", write("own", 0o600), u, 64 << 10, true},
		{"a record another account wrote", write("foreign", 0o600), u + 1, 64 << 10, false},
		{"a record group can write", write("group", 0o660), u, 64 << 10, false},
		{"a record anyone can write", write("world", 0o666), u, 64 << 10, false},
		{"a record others may read but not write", write("readable", 0o644), u, 64 << 10, true},
		{"a record past the limit", write("big", 0o600), u, 3, false},
	} {
		got, ok := ReadPrivateRegular(c.path, c.limit, c.owner)
		if ok != c.want {
			t.Errorf("%s: read %q, ok=%v; want ok=%v", c.name, got, ok, c.want)
		}
	}
	// Shape is refused before an owner is consulted, so a planted FIFO cannot
	// wait for a writer and a planted link cannot send the read elsewhere.
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(write("target", 0o600), link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fifo, link, filepath.Join(dir, "gone")} {
		if _, ok := ReadPrivateRegular(p, 64<<10, u); ok {
			t.Errorf("%s read as a record", p)
		}
	}
}
