package shim

import (
	"os"

	"github.com/akleizor/ccmorph/util/paths"
)

// AtomicWrite replaces path's content through a temp file in path's own
// directory, renamed over it: a write that truncates the target in place and
// dies midway leaves a file its next reader cannot parse, silently discarding
// everything it held. tmp is the temp file's name pattern as os.CreateTemp
// reads it — the random part it puts there keeps concurrent writers from
// sharing a temp — and mode is the replaced file's mode. Every failure removes
// the temp and leaves path as it was. The dance itself is paths.AtomicWrite's;
// this is the shape shim's callers keep.
func AtomicWrite(path string, data []byte, mode os.FileMode, tmp string) error {
	return paths.AtomicWrite(paths.WriteIn{Path: path, Temp: tmp, Data: data, Mode: mode})
}
