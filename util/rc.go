package util

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

// rcInterpreter prints the version of the interpreter the Remote Control add-on runs
// on; exit 1 (with the version when there is one) below the 3.8 floor or when absent.
func rcInterpreter(_ []string, _ io.Reader, out, _ io.Writer) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "python3", "-c",
		`import sys; print("%d.%d.%d" % sys.version_info[:3]); sys.exit(0 if sys.version_info >= (3, 8) else 1)`).Output()
	fmt.Fprint(out, string(raw))
	if err != nil {
		return 1
	}
	return 0
}
