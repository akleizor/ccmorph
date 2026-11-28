package util

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/paths"
)

// pickerLockFixture is profile gw's home under a fresh profiles dir, with a
// .claude.json and a settings layer the picker has a change to make in.
func pickerLockFixture(t *testing.T) (in PickerIn, profiles, state, layer string) {
	t.Helper()
	root := t.TempDir()
	profiles = filepath.Join(root, "profiles")
	home := filepath.Join(profiles, "gw")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.LiveLockPath(profiles, "gw")), 0o700); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, "gw.conf")
	if err := os.WriteFile(conf, []byte("type=apikey\nbase_url=http://gw.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(home, ".claude.json")
	if err := os.WriteFile(state, []byte(`{"numStartups":3}`), 0o600); err != nil {
		t.Fatal(err)
	}
	layer = filepath.Join(home, "settings.profile.json")
	if err := os.WriteFile(layer, []byte(`{"effortLevel":"high"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	in = PickerIn{Home: home, Conf: conf, Tag: "Gateway", Profile: "gw", Layer: layer,
		ModelsJSON: `{"data":[{"id":"glm-5"}]}`}
	return in, profiles, state, layer
}

func takeLive(t *testing.T, profiles, name string, how int) bool {
	t.Helper()
	f, err := os.OpenFile(paths.LiveLockPath(profiles, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB) == nil
}

func TestProjectPickerKeepsAWriteMadeBetweenItsReadAndItsRename(t *testing.T) {
	in, profiles, state, layer := pickerLockFixture(t)
	otherWriter := func(path string) bool {
		f, err := os.OpenFile(paths.LiveLockPath(profiles, "gw"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return false
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		doc, err := loadObj(path, ordjson.New())
		if err != nil {
			t.Fatal(err)
		}
		doc.Set("otherWriter", true)
		if err := atomicJSON(path, doc); err != nil {
			t.Fatal(err)
		}
		return true
	}
	old := profileJSONRead
	t.Cleanup(func() { profileJSONRead = old })
	var seen, pending []string
	profileJSONRead = func(path string) {
		seen = append(seen, path)
		if !otherWriter(path) {
			pending = append(pending, path)
		}
	}
	var errw bytes.Buffer
	if got := ProjectPicker(in, &errw); got != PickerDone {
		t.Fatalf("picker %d: %s", got, errw.String())
	}
	if len(seen) != 2 || seen[0] != state || seen[1] != layer {
		t.Fatalf("the picker must rewrite .claude.json, then the layer, each through the locked rewrite: %v", seen)
	}
	for _, p := range pending {
		if !otherWriter(p) {
			t.Fatal("the live lock outlived the picker")
		}
	}
	for path, kept := range map[string]string{state: "numStartups", layer: "effortLevel"} {
		doc, err := loadObj(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := doc.Get(kept); !ok {
			t.Fatalf("%s lost %s", path, kept)
		}
		if v, _ := doc.Get("otherWriter"); v != true {
			t.Fatalf("%s: the write made between the picker's read and its rename was lost: %s", path, mustRead(t, path))
		}
		if !strings.Contains(mustRead(t, path), "glm-5") {
			t.Fatalf("%s: the picker's own rows were lost: %s", path, mustRead(t, path))
		}
	}
}

func TestProjectPickerHoldsTheLiveLockAloneWhileItRewrites(t *testing.T) {
	in, profiles, _, _ := pickerLockFixture(t)
	old := profileJSONRead
	t.Cleanup(func() { profileJSONRead = old })
	calls := 0
	profileJSONRead = func(path string) {
		calls++
		if takeLive(t, profiles, "gw", syscall.LOCK_SH) {
			t.Errorf("%s: the live lock could be taken shared under the rewrite", path)
		}
		if takeLive(t, profiles, "gw", syscall.LOCK_EX) {
			t.Errorf("%s: the live lock could be taken exclusively under the rewrite", path)
		}
	}
	var errw bytes.Buffer
	if got := ProjectPicker(in, &errw); got != PickerDone {
		t.Fatalf("picker %d: %s", got, errw.String())
	}
	if calls != 2 {
		t.Fatalf("the locked rewrite ran %d times, want 2", calls)
	}
	if !takeLive(t, profiles, "gw", syscall.LOCK_EX) {
		t.Fatal("the live lock outlived the picker")
	}
}

func TestProjectPickerUnderALiveSessionLeavesBothFilesAlone(t *testing.T) {
	in, profiles, state, layer := pickerLockFixture(t)
	f, err := os.OpenFile(paths.LiveLockPath(profiles, "gw"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	restore := paths.SetLockWait(0)
	defer restore()
	var errw bytes.Buffer
	if got := ProjectPicker(in, &errw); got != PickerDone || errw.Len() != 0 {
		t.Fatalf("picker %d (%s): an open session must not stop the launch", got, errw.String())
	}
	if got := mustRead(t, state); got != `{"numStartups":3}` {
		t.Fatalf(".claude.json was rewritten under the session: %s", got)
	}
	if got := mustRead(t, layer); got != `{"effortLevel":"high"}` {
		t.Fatalf("the layer was rewritten under the session: %s", got)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if got := ProjectPicker(in, &errw); got != PickerDone {
		t.Fatalf("picker %d: %s", got, errw.String())
	}
	for _, p := range []string{state, layer} {
		if !strings.Contains(mustRead(t, p), "glm-5") {
			t.Fatalf("%s: the rows did not land once the session ended: %s", p, mustRead(t, p))
		}
	}
}

func TestProjectPickerInALaunchKeepsTheLaunchsSharedLock(t *testing.T) {
	in, profiles, _, _ := pickerLockFixture(t)
	if f, err := HoldLive(profiles, "gw", nil); f == nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ReleaseLive(profiles, "gw") })
	var errw bytes.Buffer
	if got := ProjectPicker(in, &errw); got != PickerDone {
		t.Fatalf("picker %d: %s", got, errw.String())
	}
	if takeLive(t, profiles, "gw", syscall.LOCK_EX) {
		t.Fatal("the launch lost its shared live lock to the picker's rewrite")
	}
	if !takeLive(t, profiles, "gw", syscall.LOCK_SH) {
		t.Fatal("the picker's rewrite left the live lock exclusive")
	}
}
