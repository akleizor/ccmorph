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
	// #562: pinned before the picker's chat-denial check read DeniedForChat —
	// a row the endpoint denies for chat is never offered.
	data = append(data, map[string]any{"id": "embed-only", "capabilities": map[string]any{"chat_completion": false}})
	catalogue, err := json.Marshal(map[string]any{"models": models})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "model-catalogue.json"), catalogue, 0o644); err != nil {
		t.Fatal(err)
	}
	conf := filepath.Join(root, "gw.conf")
	confText := "type=apikey\nbase_url=http://gw.invalid\napi_key=sk-test-636\n" +
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
	if ProjectPicker(PickerIn{Home: home, Conf: conf, Tag: "Gateway", Profile: "gw",
		Layer: filepath.Join(home, "settings.profile.json"), ModelsJSON: string(listing)}, &errw) != PickerDone {
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
	if strings.Contains(string(state), "embed-only") {
		t.Fatalf("a row the endpoint denies for chat must not be offered: %s", state)
	}
}

// A 200 whose body is not a listing — a proxy's error object, a null or
// non-list data — means no listing arrived, exactly as silence does: the
// cached rows are judged, and only when they judge nothing does the pass end
// in the gateway class. 593d73b4 read a missing data key as an authoritative
// empty listing, skipped the cache and printed the profile's config message
// #723 exists to stop printing (#747).
func TestProjectPickerTreatsA200ThatIsNotAListingAsNoListing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		models    string
		cached    []string
		want      PickerResult
		wantCache []string
	}{
		{
			name:   "silence with an empty cache",
			models: "",
			want:   PickerUnreached,
		},
		{
			name:   "an error object with an empty cache",
			models: `{"error":{"message":"upstream is unwell"}}`,
			want:   PickerUnreached,
		},
		{
			name:      "an error object falls back to the cached rows",
			models:    `{"error":{"message":"upstream is unwell"}}`,
			cached:    []string{"blocked", "allowed"},
			want:      PickerDone,
			wantCache: []string{"allowed"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(root, "gw.conf")
			if err := os.WriteFile(conf, []byte(
				"type=apikey\nbase_url=http://gw.invalid\napi_key=sk-test-636\n"+
					"\nexcluded_models=blocked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cached := make([]map[string]string, len(tc.cached))
			for i, v := range tc.cached {
				cached[i] = map[string]string{"value": v}
			}
			state, err := json.Marshal(map[string]any{"additionalModelOptionsCache": cached})
			if err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(home, ".claude.json")
			if err := os.WriteFile(statePath, state, 0o600); err != nil {
				t.Fatal(err)
			}

			var errw bytes.Buffer
			got := ProjectPicker(PickerIn{Home: home, Conf: conf, Tag: "Gateway", Profile: "gw",
				Layer: filepath.Join(home, "settings.profile.json"), ModelsJSON: tc.models}, &errw)
			if got != tc.want {
				t.Fatalf("picker %d, want %d: %s", got, tc.want, errw.String())
			}
			if strings.Contains(errw.String(), "no cached or served picker models remain") {
				t.Fatalf("a route that answered no listing was reported as the profile's config: %s", errw.String())
			}
			if tc.want != PickerDone {
				if errw.Len() != 0 {
					t.Fatalf("an unreached listing must leave the wording to the caller: %s", errw.String())
				}
				return
			}
			after, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Cache []struct {
					Value string `json:"value"`
				} `json:"additionalModelOptionsCache"`
			}
			if err := json.Unmarshal(after, &doc); err != nil {
				t.Fatal(err)
			}
			var values []string
			for _, row := range doc.Cache {
				values = append(values, row.Value)
			}
			if strings.Join(values, ",") != strings.Join(tc.wantCache, ",") {
				t.Fatalf("cache rows %v, want %v: a fallback must judge the cached rows", values, tc.wantCache)
			}
		})
	}
}
