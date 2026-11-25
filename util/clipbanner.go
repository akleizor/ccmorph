package util

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// bannerVersion is the line `cliproxyapi -h` prints (exit 0 on 7.2.x and 8.0.x
// alike). It gates the two config shapes that only some bridge versions read:
// the adapter's `cloak: never` block and the bridge's Antigravity rewrite.
var bannerVersion = regexp.MustCompile(`CLIProxyAPI Version: v?(\d+)\.(\d+)\.(\d+)`)

// ClipBanner is a cliproxyapi binary's `-h` output, or "" when it cannot run.
// The read is bounded so a hung binary cannot hold the caller.
func ClipBanner(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "-h")
	// WaitDelay makes the bound real: a grandchild that inherited the pipe
	// would otherwise hold Wait past the deadline (#636).
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// BannerBelow reports whether out names a cliproxyapi older than
// major.minor.patch. A missing or unparseable banner counts as new (false):
// the adapter then omits its cloak block and the bridge writes its Antigravity
// rewrite, the shapes a current cliproxyapi expects.
func BannerBelow(out string, major, minor, patch int) bool {
	m := bannerVersion.FindStringSubmatch(out)
	if m == nil {
		return false
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return maj < major || (maj == major && (min < minor || (min == minor && pat < patch)))
}
