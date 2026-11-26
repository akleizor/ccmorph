package util

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/settingscompose"
)

// foldLockFixture is profile gw's home holding an edit Claude Code made to
// settings.json since the last projection, and the layer the picker also
// writes.
func foldLockFixture(t *testing.T) (profiles, dir, canonical, layer string, in PickerIn) {
	t.Helper()
	root := t.TempDir()
	profiles = filepath.Join(root, "profiles")
	dir = filepath.Join(profiles, "gw")
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	canonical = filepath.Join(root, "claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(canonical), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(canonical, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	layer = filepath.Join(dir, "settings.profile.json")
	for path, body := range map[string]string{
		layer:                               `{"effortLevel":"high"}`,
		filepath.Join(dir, "settings.json"): `{"theme":"dark"}`,
		filepath.Join(dir, "cache", "settings.base.json"): `{}`,
		filepath.Join(dir, ".claude.json"):                `{"numStartups":3}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(root, "gw.conf")
	if err := os.WriteFile(conf, []byte("type=apikey\nbase_url=http://gw.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in = PickerIn{Home: dir, Conf: conf, Tag: "Gateway", Profile: "gw", Layer: layer,
		ModelsJSON: `{"data":[{"id":"glm-5"}]}`}
	return profiles, dir, canonical, layer, in
}

// holdLiveExclusive is the picker's own state: the profile's live lock held
// exclusively by someone else while the fold runs.
func holdLiveExclusive(t *testing.T, profiles string) *os.File {
	t.Helper()
	lock := LiveLockPath(profiles, "gw")
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatal(err)
	}
	return f
}

// #939: the picker and the settings fold are two whole-file writers of
// settings.profile.json. The picker's rewrite holds the profile's live lock
// exclusively; a fold that runs under that holder must not rename the layer
// through it.
func TestAFoldUnderTheLiveLockLeavesTheLayerAlone(t *testing.T) {
	profiles, dir, canonical, layer, _ := foldLockFixture(t)
	held := holdLiveExclusive(t, profiles)
	defer held.Close()
	t.Cleanup(settingscompose.SetLiveLockWait(0))
	var errw bytes.Buffer
	rc := settingscompose.Main([]string{"fold", dir, "--canonical", canonical}, &errw)
	if rc == 0 {
		t.Fatalf("the fold wrote the layer under the live lock: %s", mustRead(t, layer))
	}
	if got := mustRead(t, layer); got != `{"effortLevel":"high"}` {
		t.Fatalf("the layer changed under a live holder: %s", got)
	}
	if !strings.Contains(errw.String(), "live lock") {
		t.Fatalf("the refusal does not name the live lock: %q", errw.String())
	}
	if !strings.Contains(errw.String(), "left as it was") {
		t.Fatalf("the refusal does not say the layer was left alone: %q", errw.String())
	}
}

// #939: the picker's rewrite and the fold are the layer's two whole-file
// writers. With the fold's live lock held across its read and its rename, a
// picker rewrite inside that window writes nothing; run again once the fold
// lets go, both edits stand.
func TestTheFoldAndThePickerKeepBothWrites(t *testing.T) {
	_, dir, canonical, layer, in := foldLockFixture(t)
	t.Cleanup(settingscompose.SetLiveLockWait(0))
	t.Cleanup(SetLockWait(0))
	var errw bytes.Buffer
	seam := false
	err := settingscompose.Fold(dir, canonical, nil, nil, func() {
		seam = true
		if got := ProjectPicker(in, &errw); got != PickerDone {
			t.Fatalf("picker %d inside the fold's window: %s", got, errw.String())
		}
		if strings.Contains(mustRead(t, layer), "glm-5") {
			t.Fatalf("the picker renamed the layer inside the fold's read-to-save window: %s", mustRead(t, layer))
		}
	})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if !seam {
		t.Fatal("the fold never reached its read-to-save seam")
	}
	if !strings.Contains(mustRead(t, layer), `"theme"`) {
		t.Fatalf("the fold's own edit is missing: %s", mustRead(t, layer))
	}
	if got := ProjectPicker(in, &errw); got != PickerDone {
		t.Fatalf("picker %d after the fold: %s", got, errw.String())
	}
	doc, err := loadObj(layer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := doc.Get("theme"); v != "dark" {
		t.Fatalf("the picker's rewrite lost the fold's edit: %s", mustRead(t, layer))
	}
	if !strings.Contains(mustRead(t, layer), "glm-5") {
		t.Fatalf("the picker's own rows are missing: %s", mustRead(t, layer))
	}
}

// #939: a fold run in a launch joins the launch's own live lock and hands it
// back shared, never dropping the coverage or leaving it exclusive.
func TestAFoldInALaunchHandsTheSharedLockBack(t *testing.T) {
	profiles, dir, canonical, layer, _ := foldLockFixture(t)
	if live, why := HoldLive(profiles, "gw", nil); live == nil {
		t.Fatal(why)
	}
	t.Cleanup(func() { ReleaseLive(profiles, "gw") })
	if err := settingscompose.Fold(dir, canonical, nil, LiveFile(profiles, "gw"), nil); err != nil {
		t.Fatalf("fold: %v", err)
	}
	if !strings.Contains(mustRead(t, layer), `"theme"`) {
		t.Fatalf("the fold did not run: %s", mustRead(t, layer))
	}
	if takeLive(t, profiles, "gw", syscall.LOCK_EX) {
		t.Fatal("the fold left the launch's live lock exclusive")
	}
	if !takeLive(t, profiles, "gw", syscall.LOCK_SH) {
		t.Fatal("the fold dropped the launch's shared live lock")
	}
}

// #939: the projection writes the same layer, so it takes the same lock.
func TestAProjectionUnderTheLiveLockLeavesTheLayerAlone(t *testing.T) {
	profiles, dir, canonical, layer, _ := foldLockFixture(t)
	held := holdLiveExclusive(t, profiles)
	defer held.Close()
	t.Cleanup(settingscompose.SetLiveLockWait(0))
	var errw bytes.Buffer
	rc := settingscompose.Main([]string{"materialize", dir, "--canonical", canonical, "--saved", "{}"}, &errw)
	if rc == 0 {
		t.Fatalf("the projection wrote the layer under the live lock: %s", mustRead(t, layer))
	}
	if got := mustRead(t, layer); got != `{"effortLevel":"high"}` {
		t.Fatalf("the layer changed under a live holder: %s", got)
	}
}
