package paths

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// ErrNotRegular is a store name that is not a regular file: a FIFO, socket or
// device someone left under a receipt's name. It is damage to name, never a
// read to wait on.
var ErrNotRegular = errors.New("not a regular file")

// OpenRegular opens a store file for reading without ever waiting on the open:
// O_NONBLOCK keeps a FIFO or device under a receipt's name from blocking a hot
// path that holds the store lock (INTENT 8, #654), and anything but a regular
// file is refused with ErrNotRegular before a byte is read. On a regular file
// O_NONBLOCK changes nothing, so the read is the ordinary one.
func OpenRegular(path string) (*os.File, os.FileInfo, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: ErrNotRegular}
	}
	return f, info, nil
}

// ReadRegularFile is os.ReadFile through OpenRegular: a store file's bytes, or
// a named refusal for a name that is not a regular file.
func ReadRegularFile(path string) ([]byte, error) {
	f, _, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
