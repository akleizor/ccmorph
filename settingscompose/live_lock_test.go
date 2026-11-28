package settingscompose

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/util/paths"
)

// #939: with a live session of the profile holding the live lock shared, the
// projection still writes the generated settings its policy requires — a launch
// must start with them — and leaves the layer, its guarded whole-file write,
// exactly as it was.
func TestMaterializeUnderTheLiveLockKeepsThePolicyAndTheLayer(t *testing.T) {
	f := newFixture(t)
	f.linkPolicy(t, sandbox)
	layer := filepath.Join(f.profile, layerFile)
	write(t, filepath.Join(f.profile, "settings.json"), `{"theme":"dark"}`)
	write(t, filepath.Join(f.profile, baseFile), `{}`)
	write(t, layer, `{"effortLevel":"high"}`)
	lock := liveLockFor(f.profile)
	if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(paths.SetLockWait(0))
	code, stderr := f.run("materialize")
	if code != 0 {
		t.Fatalf("materialize under a live session: %d %s", code, stderr)
	}
	if got := string(settingsBytes(t, f)); !strings.Contains(got, "denyRead") {
		t.Fatalf("the generated settings lost the policy under a live session: %s", got)
	}
	if got, err := os.ReadFile(layer); err != nil || string(got) != `{"effortLevel":"high"}` {
		t.Fatalf("the layer changed under a live holder: %s (%v)", got, err)
	}
}
