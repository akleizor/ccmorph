package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/router"
)

func TestTheRouterPluginLoadsOnlyWhileTheRouterIsOn(t *testing.T) {
	t.Setenv("CCMORPH_STATE_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", "")
	t.Setenv(router.WorkerEnv, "")
	lib := "/opt/ccmorph/lib"
	own := filepath.Join("/opt/ccmorph/share", "claude-plugin")
	if got := pluginDirs(lib); got != own {
		t.Fatalf("off by default: got %q", got)
	}
	if err := router.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if got, want := pluginDirs(lib), own+":"+filepath.Join("/opt/ccmorph/share", "claude-plugin-router"); got != want {
		t.Fatalf("on: got %q, want %q", got, want)
	}
	t.Setenv(router.WorkerEnv, "1")
	if got := pluginDirs(lib); got != own {
		t.Fatalf("a delegated worker must not load the router: got %q", got)
	}
	if err := router.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(router.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(router.WorkerEnv, "")
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", "/x:"+own)
	if got := pluginDirs(lib); got != "/x:"+own {
		t.Fatalf("an inherited own entry must not repeat: got %q", got)
	}
}

func TestRouterHelpIsAnsweredBeforeTheRouterActs(t *testing.T) {
	for _, c := range []struct {
		args []string
		page string
	}{
		{[]string{"router", "--help"}, "Usage: ccmorph router "},
		{[]string{"router", "-h"}, "Usage: ccmorph router "},
		{[]string{"router", "on", "--help"}, "Usage: ccmorph router "},
		{[]string{"router", "off", "-h"}, "Usage: ccmorph router "},
		{[]string{"router", "status", "--help"}, "Usage: ccmorph router "},
		{[]string{"router", "on", "extra9", "--help"}, "Usage: ccmorph router "},
		{[]string{"router", "report", "--since", "1h", "--help"}, "Usage: ccmorph router "},
		{[]string{"router", "dispatch", "--help"}, "usage: ccmorph router dispatch "},
		{[]string{"router", "dispatch", "fix the parser", "--tier", "-h"}, "usage: ccmorph router dispatch "},
		{[]string{"router", "dispatch", "--profile", "--help", "--", "--tools", "Read"}, "usage: ccmorph router dispatch "},
	} {
		for _, enabled := range []bool{false, true} {
			name := strings.Join(c.args, "_") + map[bool]string{false: "_while_off", true: "_while_on"}[enabled]
			t.Run(name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("CCMORPH_STATE_DIR", t.TempDir())
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				if enabled {
					if err := router.SetEnabled(true); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := os.ReadFile(router.ConfigPath())
				out, code := captureStdout(t, func() int { return fromEnv().dispatch(c.args) })
				if code != 0 || !strings.HasPrefix(out, c.page) {
					t.Fatalf("exit %d, stdout %q", code, out)
				}
				after, err := os.ReadFile(router.ConfigPath())
				if !enabled && !os.IsNotExist(err) {
					t.Fatalf("help created router.json: %q, %v", after, err)
				}
				if string(after) != string(before) || router.Enabled() != enabled {
					t.Fatalf("changed: %q -> %q", before, after)
				}
			})
		}
	}
}
