package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The write every state file goes through, with the policies that differ
// between callers: a missing parent is created at the mode asked, the target
// ends up holding exactly the data at the mode asked, fsynced before the
// rename, and the temp it was written through is gone — a reader never sees a
// half-written file.
func TestAtomicWriteCreatesTheParentAndLeavesNoTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota", "reading.json")
	if err := AtomicWrite(WriteIn{
		Path:  path,
		Temp:  ".quota-*",
		Data:  []byte(`{"read_at":1}`),
		Mode:  0o600,
		Sync:  true,
		Mkdir: 0o700,
	}); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != `{"read_at":1}` {
		t.Fatalf("target holds %q, %v", b, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatal(err, st)
	}
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatal(err, st)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "reading.json" {
		t.Fatalf("the temp survived the write: %v", entries)
	}
}

// The mode policies, told apart by a umask that would otherwise hide the
// difference: a write that opts out of the chmod keeps the temp's own mode —
// CreateTemp's 0600 filtered by the process umask, the mode the writes that
// never chmod'd always landed — a pinned mode is chmod'd, which the umask does
// not reach, and a pinned zero is still pinned: 0000 lands 0000 (#521).
func TestAtomicWriteModePoliciesUnderAUmask(t *testing.T) {
	dir := t.TempDir()
	old := syscall.Umask(0o200)
	defer syscall.Umask(old)
	cases := []struct {
		name    string
		mode    os.FileMode
		noChmod bool
		want    os.FileMode
	}{
		{"NoChmod keeps the temp's own, umask and all", 0, true, 0o400},
		{"a pinned mode is chmod'd past the umask", 0o600, false, 0o600},
		{"a pinned zero mode lands zero", 0, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if err := AtomicWrite(WriteIn{Path: path, Temp: ".tmp-*", Data: []byte(`{}`), Mode: c.mode, NoChmod: c.noChmod}); err != nil {
				t.Fatal(err)
			}
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != c.want {
				t.Fatalf("the file landed %o, want %o", st.Mode().Perm(), c.want)
			}
		})
	}
}

// A guard that refuses is the conf rewrite's last look at the file it is
// about to replace: the rename must not happen, the target stays as it was,
// no temp is left behind, and the refusal is the write's result.
func TestAtomicWriteGuardRefusalLeavesTheTargetAsItWas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profile.conf")
	if err := os.WriteFile(path, []byte("model=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("the conf changed since it was read")
	err := AtomicWrite(WriteIn{
		Path:  path,
		Temp:  ".context-",
		Data:  []byte("model=new\n"),
		Mode:  0o600,
		Guard: func() error { return refused },
	})
	if !errors.Is(err, refused) {
		t.Fatalf("the write returned %v, want the guard's refusal", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "model=old\n" {
		t.Fatalf("a refused write replaced the target: %q, %v", b, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "profile.conf" {
		t.Fatalf("a refused write left its temp behind: %v", entries)
	}
}
