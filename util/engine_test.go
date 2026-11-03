package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The launch folds the hazard advisory into the diagnostics before it injects the
// percentage; the fallback sizes after the injection. Both must word the same
// advisory for the same percentage.
func TestHazardRecordAndTheFallbackAgree(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, "gw.conf")
	layer := filepath.Join(dir, "settings.json")
	os.WriteFile(conf, []byte("type=proxy\ncontext_window.roomy=1000000\ncontext_window.kimi-k3=40000"), 0o600)
	os.WriteFile(layer, []byte(`{"availableModels":["roomy","kimi-k3"]}`), 0o600)
	var fromRecord string
	for _, r := range LaunchDiagnostics(DiagIn{Paths: []string{layer}, ProfileConf: conf, Profile: "gw", PolicyPct: "75", Budget: "1000000", Hazard: true}) {
		if r.Kind == "hazard" {
			fromRecord = r.Text
		}
	}
	rows, err := HazardSet(HazardIn{Profile: "gw", Budget: "1000000", Sources: []string{layer}, PctRaw: "75", Conf: conf, PctInEnv: true})
	if err != nil {
		t.Fatal(err)
	}
	if fromFallback := HazardMessage(rows, false); fromRecord == "" || fromRecord != fromFallback {
		t.Fatalf("record %q, fallback %q", fromRecord, fromFallback)
	}
	if !strings.Contains(fromRecord, "lower model limits: kimi-k3 limit 40k") {
		t.Fatal(fromRecord)
	}
}

// The engine's env boolean: ECMAScript trim (U+FEFF included, NEL not), then 1/true/yes/on.
func TestEngineBoolReadsLikeTheEngine(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "on", "TRUE", "On", " on ", "yes ", "\ufeffon", "\ufeff1"} {
		if !EngineBool(v) {
			t.Errorf("%q must read as true", v)
		}
	}
	for _, v := range []string{"off", "no", "false", "0", "2", "onx", "", "\ufeffoff", "\u3080on", "\u0085on"} {
		if EngineBool(v) {
			t.Errorf("%q must read as false", v)
		}
	}
}
