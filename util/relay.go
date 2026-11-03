package util

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrNoRelay is the normal end of a launch: no hop request was left behind.
var ErrNoRelay = errors.New("no relay signal")

// Relay is a session's hop request.
type Relay struct{ Profile, Session, Model, Effort string }

// ReadRelay consumes the hop request a session left behind, removing the file.
// No file (or a symlink) is ErrNoRelay; a malformed one is an error with the
// reason. The fields are only parsed here: the profile's existence is the
// caller's check.
func ReadRelay(path string) (Relay, error) {
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		os.Remove(path)
		return Relay{}, ErrNoRelay
	}
	raw, err := os.ReadFile(path)
	os.Remove(path)
	if err != nil {
		return Relay{}, errors.New("invalid relay signal")
	}
	fields := map[string]string{"profile": "", "session": "", "model": "", "effort": ""}
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		k, v, _ := strings.Cut(line, "=")
		if _, ok := fields[k]; !ok || strings.Contains(v, "\r") {
			return Relay{}, errors.New("invalid relay signal")
		}
		if seen[k] {
			return Relay{}, fmt.Errorf("duplicate relay field '%s'", k)
		}
		seen[k], fields[k] = true, v
	}
	if fields["session"] == "" {
		return Relay{}, errors.New("invalid relay signal")
	}
	return Relay{fields["profile"], fields["session"], fields["model"], fields["effort"]}, nil
}
