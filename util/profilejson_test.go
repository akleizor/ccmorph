package util

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// #861: the one locked rewrite takes no lock when its edit changes nothing, and
// writes nothing it was not allowed to: a change under a held lock, a name that
// is no profile's, or a file that is not a JSON object.
func TestRewriteProfileJSONWithNothingToChangeTakesNoLock(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles")
	path := writeClaudeJSON(t, filepath.Join(profiles, "p"), `{"numStartups":3}`)
	if err := os.MkdirAll(filepath.Dir(paths.LiveLockPath(profiles, "p")), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(paths.LiveLockPath(profiles, "p"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	restore := paths.SetLockWait(0)
	defer restore()
	saved := 0
	save := func(string, *ordjson.Obj) error { saved++; return nil }
	if err := rewriteProfileJSON(profiles, "p", path, func(*ordjson.Obj) (bool, error) { return false, nil }, save); err != nil {
		t.Fatalf("an edit that changes nothing must not wait on the lock: %v", err)
	}
	err = rewriteProfileJSON(profiles, "p", path, func(doc *ordjson.Obj) (bool, error) { doc.Set("x", true); return true, nil }, save)
	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("a change under a held lock must wrap ErrProfileInUse: %v", err)
	}
	if saved != 0 || mustRead(t, path) != `{"numStartups":3}` {
		t.Fatalf("nothing may be written under a held lock (saves %d): %s", saved, mustRead(t, path))
	}
	if err := rewriteProfileJSON(profiles, "active", path, func(*ordjson.Obj) (bool, error) { return true, nil }, save); err == nil || saved != 0 {
		t.Fatal("an invalid profile name must be refused before any write")
	}
	if err := os.WriteFile(path, []byte(`[1]`), 0o600); err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := rewriteProfileJSON(profiles, "p", path, func(*ordjson.Obj) (bool, error) { return true, nil }, save); err == nil || saved != 0 {
		t.Fatal("a file that is not a JSON object must be refused, never rewritten")
	}
}
