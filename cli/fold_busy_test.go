package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/util"
)

// #939: the fold of a launch whose profile already has a live session cannot
// take the profile's live lock, so it is skipped — logged, never a failed
// launch, and the layer keeps what it had.
func TestABusyFoldIsLoggedAndTheLaunchGoesOn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CCMORPH_SUBCOMMAND_LAUNCH", "0")
	old := refreshOwnedPrices
	refreshOwnedPrices = func(string) {}
	t.Cleanup(func() { refreshOwnedPrices = old })
	t.Cleanup(util.SetLockWait(0))
	t.Cleanup(settingscompose.SetLiveLockWait(0))

	state, profiles := t.TempDir(), filepath.Join(t.TempDir(), "profiles")
	name := "busy939"
	dir := filepath.Join(profiles, name)
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, name+".conf"), []byte("type=token\ntoken=t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(dir, "settings.json"):               `{"theme":"dark"}`,
		filepath.Join(dir, "cache", "settings.base.json"): `{}`,
		filepath.Join(dir, "settings.profile.json"):       `{"effortLevel":"high"}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// A live session of the same profile: shared on the live lock, so this
	// launch may start beside it but no whole-file writer may rename the layer.
	lock := util.LiveLockPath(profiles, name)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	session, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := syscall.Flock(int(session.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}

	libexec, _ := coreCalls(t)
	logPath := filepath.Join(t.TempDir(), "launch.log")
	l := &launcher{dir: state, profileDirs: profiles, libexec: libexec, logPath: logPath,
		launchEnv: profile.LaunchEnv, memo: map[string]string{}}
	if _, rc := l.launch(name, false, false, nil); rc != 0 {
		t.Fatalf("a busy fold failed the launch: rc %d", rc)
	}
	written, _ := os.ReadFile(logPath)
	if !strings.Contains(string(written), "settings fold for '"+name+"' skipped:") {
		t.Fatalf("the skipped fold is not logged: %q", written)
	}
	if !strings.Contains(string(written), "live lock") {
		t.Fatalf("the log does not name the live lock: %q", written)
	}
	if got := mustReadFile(t, filepath.Join(dir, "settings.profile.json")); strings.Contains(got, "theme") {
		t.Fatalf("the busy fold wrote the layer: %s", got)
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
