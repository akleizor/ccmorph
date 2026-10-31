package util

import (
	"strings"
	"testing"
)

func TestLaunchEnvRoutesThroughTheLocalHop(t *testing.T) {
	run := func(stdin string, args ...string) []string {
		var b strings.Builder
		if rc := launchEnv(args, strings.NewReader(stdin), &b, nil); rc != 0 {
			t.Fatalf("rc %d", rc)
		}
		got := strings.Split(strings.TrimSuffix(b.String(), "."), "\x00")
		return got[:len(got)-1]
	}
	// Bridge: base and key replaced, or added when the profile had none.
	if got := strings.Join(run("ANTHROPIC_BASE_URL=https://x\nFOO=1\n", "9", "k", "0", "", "t", ""), "|"); got != "ANTHROPIC_BASE_URL=http://127.0.0.1:9|FOO=1|ANTHROPIC_API_KEY=k" {
		t.Errorf("bridge: %s", got)
	}
	// Adapter: base redirected, never added; valid tags become one header value.
	got := run("ANTHROPIC_BASE_URL=https://x\n", "", "", "1", "7", "fix-1", "2")
	if got[0] != "ANTHROPIC_BASE_URL=http://127.0.0.1:7" || got[1] != "ANTHROPIC_CUSTOM_HEADERS=x-ccmorph-task: fix-1\nx-ccmorph-attempt: 2" {
		t.Errorf("adapter: %q", got)
	}
	// Bad shapes are dropped, and a native adapter answer (no port) keeps the base.
	if got := run("ANTHROPIC_BASE_URL=https://x\n", "", "", "1", "", "bad tag", "1234"); len(got) != 1 || got[0] != "ANTHROPIC_BASE_URL=https://x" {
		t.Errorf("drop: %q", got)
	}
}
