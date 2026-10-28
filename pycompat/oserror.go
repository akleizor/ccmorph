package pycompat

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"unicode"
)

// OSErrorText words an OS error like Python's str(OSError): "[Errno 2] No such
// file or directory: '/x'". Errors with no errno keep Go's own text.
func OSErrorText(err error) string {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err.Error()
	}
	msg := []rune(errno.Error())
	if len(msg) > 0 {
		msg[0] = unicode.ToUpper(msg[0])
	}
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return fmt.Sprintf("[Errno %d] %s: '%s'", int(errno), string(msg), pe.Path)
	case errors.As(err, &le):
		return fmt.Sprintf("[Errno %d] %s: '%s' -> '%s'", int(errno), string(msg), le.Old, le.New)
	}
	return fmt.Sprintf("[Errno %d] %s", int(errno), string(msg))
}
