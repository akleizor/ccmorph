package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPriceJSON(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)
	dir := filepath.Join(root, "ccmorph")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cache := `{"host":"owned:any","fetched":0,"models":{"model":{"input":0.15,"output":0.6}}}`
	if err := os.WriteFile(filepath.Join(dir, "prices-owned-any.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	out, rc := captureStdout(t, func() int {
		return (&launcher{}).verb([]string{"price", "--json", "model-FP8", "missing"})
	})
	if rc != 0 || out != "{\"model-FP8\":{\"input\":0.15,\"output\":0.6}}\n" {
		t.Fatalf("rc %d, output %q", rc, out)
	}
}
