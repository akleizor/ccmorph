package quota

import (
	"context"
	"encoding/json"
	"errors"
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
	if err := os.WriteFile(conf, refreshConfig(dir), 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, bin, "-refresh", "-config", conf).Run()
}

// refreshConfig is the one-shot cliproxyapi config for dir. auth-dir takes a
// JSON string, which is a valid YAML double-quoted scalar: Go's %q quotes some
// bytes YAML does not read the same way, and the file then stops being honest
// YAML (#636).
func refreshConfig(dir string) []byte {
	quoted, _ := json.Marshal(dir)
	return []byte(fmt.Sprintf("host: \"127.0.0.1\"\nport: 0\nauth-dir: %s\napi-keys:\n  - \"unused\"\n", quoted))
}

// clipAuth is the part of a cliproxyapi sign-in file the quota readers use.
type clipAuth struct {
	Type        string `json:"type"`
	AccessToken string `json:"access_token"`
	AccountID   string `json:"account_id"`
	ProjectID   string `json:"project_id"`
	Expired     string `json:"expired"`
	Disabled    bool   `json:"disabled"`
}

var errSignInExpired = errors.New("sign-in expired; start a session on it to refresh")

// clipAuthFile is the profile's cliproxyapi sign-in of the given type, refreshed
// through cliproxyapi first when it has expired.
func clipAuthFile(dir, kind string) (clipAuth, error) {
	a, err := findClipAuth(dir, kind)
	if errors.Is(err, errSignInExpired) && refreshAuthDir(dir) == nil {
		a, err = findClipAuth(dir, kind)
	}
	return a, err
}

// findClipAuth reads the sign-in from the auth dir. Symlinks are not followed: a
// credential outside the 0700 auth root is not this profile's.
func findClipAuth(dir, kind string) (clipAuth, error) {
	paths := []string{}
	for _, pattern := range []string{filepath.Join(dir, "*.json"), filepath.Join(dir, "*", "*.json")} {
		m, _ := filepath.Glob(pattern)
		paths = append(paths, m...)
	}
	for _, p := range paths {
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var a clipAuth
		if json.Unmarshal(raw, &a) != nil || a.Type != kind || a.Disabled || a.AccessToken == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, a.Expired); err == nil && now().After(t) {
			return a, errSignInExpired
		}
		return a, nil
	}
	return clipAuth{}, errors.New("not signed in")
}
