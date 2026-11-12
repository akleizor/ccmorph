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

// The unsafe warning and the hazard set are one walk over the offered models, so
// they must agree where a model's wall meets its trigger exactly: the engine
// compacts at e >= trigger, so a wall AT the trigger is compacted in time and is
// safe — the set has always said so. The warning's old boundary (trigger >= wall)
// named such a model while the set stayed silent (#483). A wall one token under
// the trigger is fatal on both.
func TestTheWarningAndTheSetAgreeAModelWalledAtItsTriggerIsSafe(t *testing.T) {
	conf, set := hazardWorld(t, "claude-edge=28999 claude-under=28998", `{"availableModels":["claude-edge","claude-under"]}`)
	surfaces := []struct{ name, text string }{
		{"the unsafe warning", unsafeText(DiagIn{Profile: "gw", ProfileConf: conf, Settings: set, PctInEnv: true, EnvPct: "29", Budget: "120000"})},
		{"the hazard set", hazardNames(t, HazardIn{Profile: "gw", Budget: "120000", Sources: []string{set}, PctRaw: "29", Conf: conf, PctInEnv: true})},
	}
	for _, s := range surfaces {
		if strings.Contains(s.text, "claude-edge") {
			t.Errorf("%s names a model walled exactly at its trigger, got %q", s.name, s.text)
		}
		if !strings.Contains(s.text, "claude-under") {
			t.Errorf("%s must name the model walled one token under the trigger, got %q", s.name, s.text)
		}
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
