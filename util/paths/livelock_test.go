package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func liveFixture(t *testing.T) string {
	t.Helper()
	path := LiveLockPath(filepath.Join(t.TempDir(), "profiles"), "p")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// One unlink between the open and the flock (a concurrent rm, #837): the take
// starts again and returns a lock on the file the path names.
func TestTakeLiveRetakesAfterAnUnlinkMidTake(t *testing.T) {
	path := liveFixture(t)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	opens := 0
	f, err := TakeLive(path, syscall.LOCK_EX, nil, func() {
		if opens++; opens == 1 {
			os.Remove(path)
		}
	})
	if err != nil {
		t.Fatalf("take after one unlink: %v", err)
	}
	defer f.Close()
	if opens != 2 || !LockNamesPath(f, path) {
		t.Fatalf("opens=%d, names path=%v", opens, LockNamesPath(f, path))
	}
}

// A path that never settles is refused, never locked on a stale inode.
func TestTakeLiveRefusesAPathThatNeverSettles(t *testing.T) {
	path := liveFixture(t)
	opens := 0
	_, err := TakeLive(path, syscall.LOCK_EX, nil, func() { opens++; os.Remove(path) })
	if !errors.Is(err, ErrLiveReplaced) || opens != LiveLockTries {
		t.Fatalf("err=%v opens=%d", err, opens)
	}
}

// A symlink at the path is never a live lock (GH-837).
func TestTakeLiveNeverLocksThroughALink(t *testing.T) {
	path := liveFixture(t)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := TakeLive(path, syscall.LOCK_SH, nil, nil); !errors.Is(err, ErrLiveIsLink) {
		t.Fatalf("err=%v", err)
	}
}

// A holder past the wait is ErrLiveHeld, and the waiting callback ran once.
func TestTakeLiveReportsAHolderPastTheWait(t *testing.T) {
	t.Cleanup(SetLockWait(0))
	path := liveFixture(t)
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	waited := 0
	if _, err := TakeLive(path, syscall.LOCK_SH, func() { waited++ }, nil); !errors.Is(err, ErrLiveHeld) || waited != 1 {
		t.Fatalf("err=%v waited=%d", err, waited)
	}
}
