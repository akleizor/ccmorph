package settingscompose

import (
	"os"
	"path/filepath"
	"testing"
)

// A profile's own settings are edited in its layer once it is composed (layer
// or base present, a dangling link included), else in the generated file.
func TestProfileLayerFollowsTheComposedState(t *testing.T) {
	for _, c := range []struct {
		name, present, want string
		link                bool
	}{
		{"first run", "", GeneratedFile, false},
		{"layer present", LayerFile, LayerFile, false},
		{"only a base", BaseFile, LayerFile, false},
		{"a dangling layer link", LayerFile, LayerFile, true},
	} {
		dir := t.TempDir()
		if c.present != "" {
			p := filepath.Join(dir, c.present)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			var err error
			if c.link {
				err = os.Symlink(filepath.Join(dir, "gone"), p)
			} else {
				err = os.WriteFile(p, []byte("{}"), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := ProfileLayer(dir); got != filepath.Join(dir, c.want) {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
