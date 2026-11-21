package adapter

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/netroute"
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

// GH-773: the profile conf names no upstream proxy, but the launcher's shell
// exported CCMORPH_UPSTREAM_PROXY. The probes and the translator read the
// profile conf alone; a shim that falls back to the process env routes the
// session through a proxy they never saw -- GH-580's split, mirrored.
func TestShimTakesTheUpstreamProxyFromTheProfileConfAlone(t *testing.T) { // GH-773
	noAmbientProxy(t)
	t.Setenv("CCMORPH_UPSTREAM_PROXY", "http://shell.invalid:3128")
	profile := map[string]string{} // the profile conf names no knob

	getenv := shimGetenv(profile)
	if got := getenv("CCMORPH_UPSTREAM_PROXY"); got != "" {
		t.Fatalf("the shim read CCMORPH_UPSTREAM_PROXY from the process env, not the profile conf: %q "+
			"(fix: answer CCMORPH_UPSTREAM_PROXY from the adapter's profile env map alone, adapter/proc.go)", got)
	}
	req, err := http.NewRequest("POST", "https://gateway.lan/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	probe, err := netroute.Proxy(netroute.Parse(profile["CCMORPH_UPSTREAM_PROXY"]), req)
	if err != nil || probe != nil {
		t.Fatalf("the probe's route = (%v, %v), want a direct dial", probe, err)
	}
	viaShim, err := netroute.Proxy(netroute.Parse(getenv("CCMORPH_UPSTREAM_PROXY")), req)
	if err != nil || viaShim != nil {
		t.Fatalf("the shim's route = (%v, %v), want the probe's direct dial", viaShim, err)
	}

	// A knob the profile conf does name still reaches the shim.
	profile["CCMORPH_UPSTREAM_PROXY"] = "http://bridge.invalid:3128"
	if got := shimGetenv(profile)("CCMORPH_UPSTREAM_PROXY"); got != "http://bridge.invalid:3128" {
		t.Fatalf("the profile conf's own knob does not reach the shim: %q", got)
	}
	// Every other setting keeps the process env fallback the shim always had.
	t.Setenv("CCMORPH_STALL_TIMEOUT", "7")
	if got := shimGetenv(map[string]string{})("CCMORPH_STALL_TIMEOUT"); got != "7" {
		t.Fatalf("the shim lost the process env fallback for CCMORPH_STALL_TIMEOUT: %q", got)
	}
}

// GH-773: the spawned shim inherits the adapter's environment; a routing knob
// the profile conf never named must not ride into it, while the profile's own
// spelling still must.
func TestSpawnedShimEnvDropsAnInheritedUpstreamProxy(t *testing.T) { // GH-773
	t.Setenv("CCMORPH_UPSTREAM_PROXY", "http://shell.invalid:3128")
	for _, kv := range environWith(map[string]string{"CCMORPH_MASK": "0"}) {
		if v, ok := strings.CutPrefix(kv, "CCMORPH_UPSTREAM_PROXY="); ok {
			t.Fatalf("the spawned shim inherited CCMORPH_UPSTREAM_PROXY=%q from the launcher's shell, "+
				"but the profile conf named none and the probes and the translator ignore it (GH-773)", v)
		}
	}
	profile := "http://bridge.invalid:3128"
	got, found := "", 0
	for _, kv := range environWith(map[string]string{"CCMORPH_UPSTREAM_PROXY": profile}) {
		if v, ok := strings.CutPrefix(kv, "CCMORPH_UPSTREAM_PROXY="); ok {
			got, found = v, found+1
		}
	}
	if found != 1 || got != profile {
		t.Fatalf("the spawned shim's env carries CCMORPH_UPSTREAM_PROXY %d time(s), last %q; "+
			"want the profile conf's one spelling %q", found, got, profile)
	}
}
