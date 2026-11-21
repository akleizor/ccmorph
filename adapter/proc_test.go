package adapter

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
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
	t.Setenv("CCMORPH_UPSTREAM_PROXY", "http://shell.invalid:3128")
	profile := map[string]string{} // the profile conf names no knob

	getenv := shimGetenv(profile)
	if got := getenv("CCMORPH_UPSTREAM_PROXY"); got != "" {
		t.Fatalf("the shim read CCMORPH_UPSTREAM_PROXY from the process env, not the profile conf: %q "+
			"(fix: answer CCMORPH_UPSTREAM_PROXY from the adapter's profile env map alone, adapter/proc.go)", got)
	}
	probe, shim := freshRoute(t, "", "http://shell.invalid:3128")
	if probe != "direct" || shim != "direct" {
		t.Fatalf("with the profile conf silent and the shell exporting the knob, the probe routed %s and the shim %s; "+
			"want both direct -- the shim read CCMORPH_UPSTREAM_PROXY from the process env, not the profile conf (GH-773)",
			probe, shim)
	}

	// A knob the profile conf does name still reaches the shim.
	profile["CCMORPH_UPSTREAM_PROXY"] = "http://bridge.invalid:3128"
	if got := shimGetenv(profile)("CCMORPH_UPSTREAM_PROXY"); got != "http://bridge.invalid:3128" {
		t.Fatalf("the profile conf's own knob does not reach the shim: %q", got)
	}
	probe, shim = freshRoute(t, "http://bridge.invalid:3128", "http://shell.invalid:3128")
	want := "proxy:http://bridge.invalid:3128"
	if probe != want || shim != want {
		t.Fatalf("with the profile conf naming %s, the probe routed %s and the shim %s; want both %s",
			profile["CCMORPH_UPSTREAM_PROXY"], probe, shim, want)
	}
	// Every other setting keeps the process env fallback the shim always had.
	t.Setenv("CCMORPH_STALL_TIMEOUT", "7")
	if got := shimGetenv(map[string]string{})("CCMORPH_STALL_TIMEOUT"); got != "7" {
		t.Fatalf("the shim lost the process env fallback for CCMORPH_STALL_TIMEOUT: %q", got)
	}
}

// TestShimRouteChild answers, in a fresh process with no ambient proxy, what
// route the probe and the shim take for the profile knob its environment
// carries (none when CCMORPH_TEST_PROFILE_KNOB is unset) while
// CCMORPH_UPSTREAM_PROXY is the launcher's shell export. Run on its own it
// does nothing.
func TestShimRouteChild(t *testing.T) {
	if os.Getenv("CCMORPH_TEST_ROUTE_CHILD") != "1" {
		return
	}
	profile := map[string]string{}
	if v, ok := os.LookupEnv("CCMORPH_TEST_PROFILE_KNOB"); ok {
		profile["CCMORPH_UPSTREAM_PROXY"] = v
	}
	req, err := http.NewRequest("POST", "https://gateway.lan/v1/messages", nil)
	if err != nil {
		fmt.Printf("ccmorph-adapter-route: bad request: %v\n", err)
		return
	}
	route := func(knob string) string {
		u, err := netroute.Proxy(netroute.Parse(knob), req)
		switch {
		case err != nil:
			return "error:" + err.Error()
		case u == nil:
			return "direct"
		default:
			return "proxy:" + u.String()
		}
	}
	fmt.Printf("ccmorph-adapter-route: probe=%s shim=%s\n",
		route(profile["CCMORPH_UPSTREAM_PROXY"]), route(shimGetenv(profile)("CCMORPH_UPSTREAM_PROXY")))
}

// freshRoute asks a fresh test binary -- whose http.ProxyFromEnvironment cache
// has read no proxy of its own -- what routes the probe and the shim take. The
// process-wide cache is why this cannot be asked in-process: a test that ran
// earlier may have primed it with an ambient proxy, and t.Setenv cannot undo
// that (GH-773 review).
func freshRoute(t *testing.T, profile, shellProxy string) (probe, shim string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestShimRouteChild$")
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
			"CCMORPH_UPSTREAM_PROXY", "CCMORPH_TEST_PROFILE_KNOB", "CCMORPH_TEST_ROUTE_CHILD":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "CCMORPH_TEST_ROUTE_CHILD=1", "CCMORPH_UPSTREAM_PROXY="+shellProxy)
	if profile != "" {
		env = append(env, "CCMORPH_TEST_PROFILE_KNOB="+profile)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("the route child failed: %v (output %q)", err, out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(line, "ccmorph-adapter-route: ")
		if !ok {
			continue
		}
		var p, s string
		if _, err := fmt.Sscanf(rest, "probe=%s shim=%s", &p, &s); err != nil {
			t.Fatalf("the route child answered %q: %v", rest, err)
		}
		return p, s
	}
	t.Fatalf("the route child printed no answer: %q", out)
	return "", ""
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
