package paths

import (
	"errors"
	"os"
)

// NamesPath reports whether f is still the file path names, links followed as
// the open that made f followed them. A flock is a lock on what the path names
// only while the two are one file: rm unlinks a profile's lock files, and a
// taker that opened the file before that unlink and flocked it after would
// hold an inode no other process can reach, while the next taker makes and
// locks a second file at the same path (GH-837). Every taker of a lock file
// that something may unlink checks this after its flock and takes the lock
// again when it fails.
func NamesPath(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(path)
	return err == nil && os.SameFile(fi, pi)
}

// LockRetakes bounds how often a taker opens a lock file again because the one
// it locked was unlinked under it. Each retake answers one removal that landed
// inside its own open-to-flock window or while it waited, so ten is slack.
const LockRetakes = 10

// ErrLockReplaced is why a taker gave up: the lock file was removed or
// replaced on every attempt, so no lock it took covered what the path names.
var ErrLockReplaced = errors.New("the lock file was removed or replaced on every attempt to take it")
