package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A lock file this process cannot open — a root-owned 0600 one left by a
// single sudo'd compaction — must not poison every reader forever, and must
// never be unlinked: a privileged compactor may hold that very inode, and a
// replacement would split the store's coordination. The reader answers
// without a lock and leaves the file where it is.
func TestUsageStoreReadLockReadsWithoutAnUnopenableLockAndKeepsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens files with mode 0")
	}
	base := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(base, ".move.lock")
	if err := os.WriteFile(lock, nil, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lock, 0o600) })
	before, err := os.Stat(lock)
	if err != nil {
		t.Fatal(err)
	}
	f, err := TryUsageStoreReadLock(base)
	if err != nil || f != nil {
		t.Fatalf("a foreign lock poisoned the reader or was replaced: %v %v", err, f)
	}
	after, err := os.Stat(lock)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("the unopenable lock was unlinked or replaced: %v", err)
	}
}

// A store whose directory is read-only changes nothing about that: the reader
// answers without a lock rather than brick forever.
func TestUsageStoreReadLockProceedsWithoutAnUnmovableLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens files with mode 0")
	}
	base := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(base, ".move.lock")
	if err := os.WriteFile(lock, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(base, 0o700) })
	f, err := TryUsageStoreReadLock(base)
	if err != nil || f != nil {
		t.Fatalf("a stuck stale lock bricked the reader: %v %v", err, f)
	}
}

// A store another process holds past the bound is a refusal, not a hang: the
// statusline, the Stop hook and the export all take this path (INTENT 8, #654).
func TestUsageStoreLockWithinRefusesABusyStore(t *testing.T) {
	base := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := UsageStoreLock(base, true)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	start := time.Now()
	f, err := UsageStoreLockWithin(base, false, UsageLockWait)
	if f != nil || !errors.Is(err, ErrUsageStoreBusy) || !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("a held store was not refused with a busy error: %v %v", f, err)
	}
	if elapsed := time.Since(start); elapsed > 2*UsageLockWait {
		t.Fatalf("the refusal took %v, past its bound", elapsed)
	}
}

// The bound is a wait, not a refusal of every held store: a lock released
// inside it is taken.
func TestUsageStoreLockWithinTakesAReleasedLock(t *testing.T) {
	base := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := UsageStoreLock(base, true)
	if err != nil {
		t.Fatal(err)
	}
	release := time.AfterFunc(50*time.Millisecond, func() { held.Close() })
	defer release.Stop()
	got := make(chan error, 1)
	go func() {
		f, err := UsageStoreLockWithin(base, true, 5*time.Second)
		if f != nil {
			f.Close()
		}
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("a released lock was not taken: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bounded lock did not take the lock once it was free")
	}
}
