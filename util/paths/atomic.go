package paths

import (
	"os"
	"path/filepath"
)

// WriteIn is what one atomic write needs: the file replaced, the temp it is
// written through, its content and mode, and the policies that differ between
// callers.
type WriteIn struct {
	// Path is the file the write replaces.
	Path string
	// Temp is the temp file's name pattern as os.CreateTemp reads it; the
	// random part it puts there keeps concurrent writers from sharing a temp.
	Temp string
	// Data is the content Path ends up holding.
	Data []byte
	// Mode is the replaced file's mode. It is chmod'd onto the temp as soon as
	// the temp is created, before any data is written, and always, 0 included:
	// a mode-0000 file lands 0000, and the umask does not reach it. Chmod-
	// before-write is replaceFields' order; the other chmod'ing writes chmod'd
	// after the close, but their modes carry no setuid/setgid bits, so the
	// order changes nothing for them.
	Mode os.FileMode
	// NoChmod skips the chmod and leaves Mode unused: the file keeps the
	// temp's own CreateTemp mode, 0600 filtered by the process umask, the mode
	// the writes that never chmod'd always landed (#521).
	NoChmod bool
	// Sync fsyncs the temp before the rename: a credential, or a file another
	// process must not read half-written after a crash.
	Sync bool
	// Mkdir creates Path's directory first at this mode; 0 leaves the
	// directory as it is.
	Mkdir os.FileMode
	// Guard runs after the temp is written and closed, before the rename: the
	// rename happens only when it returns nil, and its error is the write's.
	// nil guards nothing.
	Guard func() error
}

// AtomicWrite replaces in.Path's content through a temp file in its own
// directory, renamed over it: a write that truncates the target in place and
// dies midway leaves a file its next reader cannot parse, silently discarding
// everything it held. Every failure removes the temp and leaves the target as
// it was.
func AtomicWrite(in WriteIn) error {
	if in.Mkdir != 0 {
		if err := os.MkdirAll(filepath.Dir(in.Path), in.Mkdir); err != nil {
			return err
		}
	}
	f, err := os.CreateTemp(filepath.Dir(in.Path), in.Temp)
	if err != nil {
		return err
	}
	tmp := f.Name()
	bad := func(err error) error { f.Close(); os.Remove(tmp); return err }
	if !in.NoChmod {
		if err := f.Chmod(in.Mode); err != nil {
			return bad(err)
		}
	}
	if _, err := f.Write(in.Data); err != nil {
		return bad(err)
	}
	if in.Sync {
		if err := f.Sync(); err != nil {
			return bad(err)
		}
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if in.Guard != nil {
		if err := in.Guard(); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, in.Path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
