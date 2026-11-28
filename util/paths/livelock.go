package paths

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// The profile live lock protocol: util's launch path (HoldLive, the
// whole-file writers) and settingscompose's fold and projection take the same
// file through its one path, bounded wait and open-lock-check loop (TakeLive);
// util's RemoveLive, which never creates and never waits, shares the path,
// the inode recheck and the retry bound.

// LiveLockPath names a profile's live lock: a launch holds it shared while
// its session runs, and a setup, fold or projection takes it exclusive to
// write (#185, #205). profiles is the profiles directory; the locks live in
// .locks beside it.
func LiveLockPath(profiles, name string) string {
	return filepath.Join(filepath.Dir(profiles), ".locks", name+".live")
}

// LockWait bounds every lock the launch path takes: a holder stuck on a
// stalled volume defers the work to the next launch instead of hanging the
// session, which INTENT promise 8 forbids (#185). Tests shorten it through
// SetLockWait.
var LockWait = 2 * time.Second

// SetLockWait replaces LockWait, for tests; the returned func restores it.
func SetLockWait(d time.Duration) (restore func()) {
	old := LockWait
	LockWait = d
	return func() { LockWait = old }
}

// FlockWithin takes how on f, retrying without blocking until LockWait
// expires; false means someone else holds the lock past the wait and the
// caller must go on without it.
func FlockWithin(f *os.File, how int) bool { return flockWaiting(f, how, nil) }

// flockWaiting is FlockWithin calling waiting, when set, once after the first
// try finds the lock held.
func flockWaiting(f *os.File, how int, waiting func()) bool {
	deadline := time.Now().Add(LockWait)
	for first := true; ; first = false {
		if syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB) == nil {
			return true
		}
		if first && waiting != nil {
			waiting()
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// LockNamesPath reports whether f still names what path names. A lock is
// only a lock on the profile while the path and the descriptor are one file:
// a removal that unlinks between an open and its flock would otherwise leave
// the taker holding an inode no other process can see, and the next taker
// would make and lock a second file at the same path (#837). The path is not
// followed: a link put there since the open is not the file the lock is on.
// SameInode compares two paths; this compares the descriptor, whose inode a
// path lookup can no longer reach.
func LockNamesPath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return os.SameFile(fi, pi)
}

// LiveLockTries bounds the open-lock-check loop that keeps a take and a
// removal off an inode the path no longer names (#837). Each round only meets
// a removal that ran inside its own open-and-lock window, so five is slack.
const LiveLockTries = 5

var (
	// ErrLiveHeld: another holder kept the live lock past LockWait (#205).
	// Any other error means the lock file itself could not be made or opened
	// (#217), so a caller's message can tell a stuck holder from that.
	ErrLiveHeld = errors.New("another launch is holding the live lock")
	// ErrLiveReplaced: the file at the live lock path was removed or replaced
	// on every attempt; a lock on an inode the path no longer names covers
	// nothing (#837).
	ErrLiveReplaced = errors.New("the live lock file was replaced while it was being taken")
	// ErrLiveIsLink: a symlink sits at the live lock path. A lock taken
	// through a link is one a removal can unlink from under the taker,
	// leaving the path free for a second lock (GH-837).
	ErrLiveIsLink = errors.New("a symlink is there, and a live lock is never taken through one")
)

// IsLinkErr reports whether an O_NOFOLLOW open failed because the path is a
// symlink: ELOOP on Linux and macOS.
func IsLinkErr(err error) bool {
	return errors.Is(err, syscall.ELOOP)
}

// TakeLive opens the live lock at path (made 0600, never through a link) and
// takes how on it within LockWait, checking after the take that the
// descriptor still names the path and starting again when a removal ran in
// between. waiting, when set, is called once when the first non-blocking try
// finds the lock held; afterOpen is the test seam between the open and the
// flock. The errors are ErrLiveIsLink, ErrLiveHeld, ErrLiveReplaced, or the
// failure to make or open the file.
func TakeLive(path string, how int, waiting, afterOpen func()) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < LiveLockTries; attempt++ {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
		if IsLinkErr(err) {
			return nil, ErrLiveIsLink
		}
		if err != nil {
			return nil, err
		}
		if afterOpen != nil {
			afterOpen()
		}
		if !flockWaiting(f, how, waiting) {
			f.Close()
			return nil, ErrLiveHeld
		}
		if !LockNamesPath(f, path) {
			f.Close()
			continue
		}
		return f, nil
	}
	return nil, ErrLiveReplaced
}
