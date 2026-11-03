package util

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// GH-321: a launch with nothing old to reap must not scan the host's processes.
// Launch-settings overlays and provider bridge dirs go after 12h unless a live
// process still names them; a young one stays; a failed scan names nothing, so the
// old ones go; and an old neighbour that is neither overlay nor bridge is spared.
func TestGCOverlaysAndBridgesScanProcessesOnlyWhenSomethingIsOld(t *testing.T) {
	for _, c := range []struct {
		name        string
		state       string // empty, young, old, live
		psStatus    int
		wantPresent bool
		wantScans   int
	}{
		{"nothing to reap", "empty", 0, false, 0},
		{"young stays without a scan", "young", 0, true, 0},
		{"abandoned goes", "old", 0, false, 1},
		{"live stays", "live", 0, true, 1},
		{"a failed process scan reaps", "live", 1, false, 1},
	} {
		root := t.TempDir()
		// A tmp dir whose own path looks like a bridge must not make its neighbours one.
		tmp := filepath.Join(root, "ccmorph-bridge-"+strconv.Itoa(os.Getuid())+"-tmp")
		profiles := filepath.Join(root, "profiles")
		os.MkdirAll(tmp, 0o700)
		os.MkdirAll(filepath.Join(profiles, "demo", "cache"), 0o700)
		os.MkdirAll(filepath.Join(root, "bin"), 0o700)
		live := filepath.Join(root, "live-processes")
		scans := filepath.Join(root, "ps-scans")
		os.WriteFile(live, nil, 0o600)
		os.WriteFile(scans, nil, 0o600)
		os.WriteFile(filepath.Join(root, "bin", "ps"), []byte("#!/bin/sh\nprintf 'scan\\n' >> '"+scans+"'\ncat '"+live+"'\nexit "+strconv.Itoa(c.psStatus)+"\n"), 0o755)
		t.Setenv("PATH", filepath.Join(root, "bin")+":"+os.Getenv("PATH"))

		ancient := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		unrelated := filepath.Join(tmp, "ccmorph-rc-ready.test")
		os.Mkdir(unrelated, 0o700)
		os.Chtimes(unrelated, ancient, ancient)

		overlayA := filepath.Join(profiles, "demo", "cache", "launch-settings.test")
		overlayB := filepath.Join(tmp, "ccmorph-launch-settings.test")
		bridge := filepath.Join(tmp, "ccmorph-bridge-"+strconv.Itoa(os.Getuid())+"-test")
		candidates := []string{overlayA, overlayB, bridge}
		if c.state != "empty" {
			os.WriteFile(overlayA, []byte("{}"), 0o600)
			os.WriteFile(overlayB, []byte("{}"), 0o600)
			os.Mkdir(bridge, 0o700)
			os.WriteFile(filepath.Join(bridge, "config.yaml"), []byte("{}"), 0o600)
			if c.state != "young" {
				for _, f := range candidates {
					os.Chtimes(f, ancient, ancient)
				}
			}
			if c.state == "live" {
				os.WriteFile(live, []byte("claude --settings "+overlayA+"\nclaude --settings "+overlayB+"\ncliproxyapi --config "+bridge+"/config.yaml\n"), 0o600)
			}
		}

		GCStale(filepath.Join(root, "state"), profiles, tmp)

		b, _ := os.ReadFile(scans)
		if n := len(strings.Fields(string(b))); n != c.wantScans {
			t.Errorf("%s: want %d process scans, got %d", c.name, c.wantScans, n)
		}
		if _, err := os.Stat(unrelated); err != nil {
			t.Errorf("%s: removed %s", c.name, unrelated)
		}
		if c.state == "empty" {
			continue
		}
		for _, f := range candidates {
			if _, err := os.Stat(f); (err == nil) != c.wantPresent {
				t.Errorf("%s: %s present=%v, want %v", c.name, f, err == nil, c.wantPresent)
			}
		}
	}
}
