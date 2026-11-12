package adapter

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/shim"
)

// GH-513: the stock adapter serves the shim in-process, so shim.Main — where
// the pre-shard move used to start — never runs. Starting the shim the default
// way (no hooks binary) must still move a settled flat row into its mtime's
// day folder.
func TestStartShimMovesSettledFlatRowsGH513(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	dir := filepath.Join(state, "ccmorph", "usage")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().AddDate(0, 0, -1)
	flat := filepath.Join(dir, "row.json")
	if err := os.WriteFile(flat, []byte(`{"response_id":"row"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(flat, day, day); err != nil {
		t.Fatal(err)
	}

	p, err := startShim("", shim.Options{Port: 0, Target: "http://127.0.0.1:1"},
		map[string]string{"CCMORPH_MASK": "0", "CCMORPH_QUIRKS": "off"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopProcesses(p) })

	moved := filepath.Join(dir, day.Format("2006-01-02"), "row.json")
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, movedErr := os.Stat(moved)
		_, flatErr := os.Stat(flat)
		if movedErr == nil && os.IsNotExist(flatErr) {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the in-process shim did not move the settled flat row to %s", moved)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if data, err := os.ReadFile(moved); err != nil || string(data) != `{"response_id":"row"}` {
		t.Fatalf("the moved row: %v %s", err, data)
	}
}
