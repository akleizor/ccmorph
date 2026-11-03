package util

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// RCInterpreter is the version of the interpreter the Remote Control add-on runs
// on; ok is false (with the version when there is one) below the 3.8 floor or when
// absent.
func RCInterpreter() (version string, ok bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "python3", "-c",
		`import sys; print("%d.%d.%d" % sys.version_info[:3]); sys.exit(0 if sys.version_info >= (3, 8) else 1)`).Output()
	return strings.TrimRight(string(raw), "\n"), err == nil
}
