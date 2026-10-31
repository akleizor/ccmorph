package util

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// relayRead is `util relay-read <relay file>`: consume the hop request a session
// left behind. No file (or a symlink) exits 3, the normal end of a launch. A
// well-formed file prints profile=, session=, model=, effort= and is removed;
// a malformed one is removed and exits 1 with the reason on stderr. The fields
// are only parsed here: the profile's existence is the caller's check.
func relayRead(args []string, _ io.Reader, out, errw io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	path := args[0]
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		os.Remove(path)
		return 3
	}
	raw, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil {
		fmt.Fprintln(errw, "invalid relay signal")
		return 1
	}
	fields := map[string]string{"profile": "", "session": "", "model": "", "effort": ""}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		if _, ok := fields[k]; !ok || strings.Contains(v, "\r") {
			fmt.Fprintln(errw, "invalid relay signal")
			return 1
		}
		if seen[k] {
			fmt.Fprintf(errw, "duplicate relay field '%s'\n", k)
			return 1
		}
		seen[k], fields[k] = true, v
	}
	if fields["session"] == "" {
		fmt.Fprintln(errw, "invalid relay signal")
		return 1
	}
	for _, k := range []string{"profile", "session", "model", "effort"} {
		fmt.Fprintf(out, "%s=%s\n", k, fields[k])
	}
	return 0
}
