package quota

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// refreshAuthDir runs cliproxyapi's one-shot refresh on a profile's auth dir:
// every OAuth sign-in there that expires soon is refreshed and saved. cliproxyapi
// owns each provider's OAuth client and the file; ccmorph only reads the token.
// A var so tests never start a process.
var refreshAuthDir = func(dir string) error {
	bin, err := exec.LookPath("cliproxyapi")
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "ccmorph-refresh-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	conf := filepath.Join(tmp, "config.yaml")
	yaml := fmt.Sprintf("host: \"127.0.0.1\"\nport: 0\nauth-dir: %q\napi-keys:\n  - \"unused\"\n", dir)
	if err := os.WriteFile(conf, []byte(yaml), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "-refresh", "-config", conf).Run()
}
