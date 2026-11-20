package cli

import (
	"os"
	"path/filepath"
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
