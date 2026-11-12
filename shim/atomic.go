package shim

import (
	"os"
	"path/filepath"
)

// AtomicWrite replaces path's content through a temp file in path's own
// directory, renamed over it: a write that truncates the target in place and
// dies midway leaves a file its next reader cannot parse, silently discarding
// everything it held. tmp is the temp file's name pattern as os.CreateTemp
// reads it — the random part it puts there keeps concurrent writers from
// sharing a temp — and mode is the replaced file's mode. Every failure removes
// the temp and leaves path as it was.
func AtomicWrite(path string, data []byte, mode os.FileMode, tmp string) error {
	f, err := os.CreateTemp(filepath.Dir(path), tmp)
	if err != nil {
		return err
	}
	name := f.Name()
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, mode)
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name)
	}
	return err
}
