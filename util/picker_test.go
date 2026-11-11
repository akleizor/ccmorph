package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// A gateway serving many models must not pay one catalogue read per claude-
// row: the picker loads the conf, the endpoint listing and the bundled
// catalogue once, before its loop (#424).
func TestProjectPickerReadsTheCatalogueOnceForEveryClaudeModel(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	share := filepath.Join(root, "share")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	models := map[string]any{"anthropic/glm-5": map[string]any{"limit": map[string]any{"context": 200000}}}
	var data []any
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("claude-model-%d", i)
		data = append(data, map[string]any{"id": id})
		models["anthropic/"+id] = map[string]any{"limit": map[string]any{"context": 1000000}}
	}
	data = append(data, map[string]any{"id": "glm-5"})
	catalogue, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "model-catalogue.json"), catalogue, 0o644); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, "gw.conf")
	confText := "type=apikey\nbase_url=http://gw.invalid\napi_key=k\ncontext_window.sized-elsewhere=1000\n\ncontext_window.sized-elsewhere=1000\n\n" +
		"context_window.sized-elsewhere=1000\n"
	if err := os.WriteFile(conf, []byte(confText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	listing, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}

	// ShareDir is read once per catalogue load, so counting it counts the reads.
	reads := 0
	oldShare := ctxwindow.ShareDir
	ctxwindow.ShareDir = func() string { reads++; return share }
	t.Cleanup(func() { ctxwindow.ShareDir = oldShare })
	t.Setenv("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "")
	t.Setenv("MODEL_WINDOW_ENDPOINT_JSON", "")

	var errw bytes.Buffer
	if !ProjectPicker(PickerIn{Home: home, Conf: conf, Tag: "Gateway", Profile: "gw",
		Layer: filepath.Join(home, "settings.profile.json"), ModelsJSON: string(listing)}, &errw) {
		t.Fatalf("picker failed: %s", errw.String())
	}
	if reads != 1 {
		t.Fatalf("the picker read the bundled catalogue %d times for 12 claude models; it must read it once", reads)
	}
	state, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), `"value": "claude-model-7[1m]"`) {
		t.Fatalf("a probed 1M claude model must keep its derived [1m] row: %s", state)
	}
	if !strings.Contains(string(state), `"value": "glm-5"`) {
		t.Fatalf("the non-claude row must stay: %s", state)
	}
}
