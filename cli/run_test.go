package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/router"
)

// A router worker is launched by the parent session with os.Environ(), so it
// inherits the parent's CLAUDE_CODE_PLUGIN_DIRS, which names the router plugin
// there. pluginDirs must drop that inherited entry: the worker must never load
// the router's hooks and MCP server the launch guard refuses it, and a nested
// ccmorph run must not accumulate a second copy.
func TestPluginDirsNeverInheritsTheRouterPlugin(t *testing.T) {
	t.Setenv("CCMORPH_STATE_DIR", t.TempDir())
	tmp := t.TempDir()
	lib := filepath.Join(tmp, "lib")
	own := filepath.Join(tmp, "share", "claude-plugin")
	routerDir := filepath.Join(tmp, "share", "claude-plugin-router")
	if err := router.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := router.SetEnabled(false); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", "/opt/other/plugin:"+routerDir)
	t.Setenv(router.WorkerEnv, "1")
	if got, want := pluginDirs(lib), "/opt/other/plugin:"+own; got != want {
		t.Fatalf("a worker must not inherit the router plugin: got %q, want %q", got, want)
	}
	t.Setenv(router.WorkerEnv, "")
	got := pluginDirs(lib)
	if want := "/opt/other/plugin:" + own + ":" + routerDir; got != want {
		t.Fatalf("a non-worker must name the router plugin exactly once: got %q, want %q", got, want)
	}
	t.Setenv("CLAUDE_CODE_PLUGIN_DIRS", got)
	if again := pluginDirs(lib); strings.Count(again, routerDir) != 1 {
		t.Fatalf("a second ccmorph run layer must not duplicate the router plugin: got %q", again)
	}
}

// TestWithoutOwnDropsAnyInstallsRouterPlugin pins #636: an inherited
// claude-plugin-router from another install re-added the router hooks to a
// worker, so the base name is what the guard drops.
func TestWithoutOwnDropsAnyInstallsRouterPlugin(t *testing.T) {
	share := "/opt/ccmorph/share"
	got := withoutOwn("/opt/homebrew/Cellar/ccmorph/0.5.23/share/claude-plugin-router:/opt/other/plugin:"+share+"/claude-plugin", share)
	if want := "/opt/other/plugin"; got != want {
		t.Fatalf("withoutOwn = %q, want %q", got, want)
	}
}

// #636: Claude's own mcp text sends a person to `claude mcp add`, which writes
// the host's ~/.claude.json; a profile's run names the profile's own way.
func TestMcpHintNamesTheProfilesOwnWay(t *testing.T) {
	if got := mcpHint([]string{"mcp", "list"}, "work"); !strings.Contains(got, "ccmorph run work mcp add") {
		t.Fatalf("hint %q", got)
	}
	for _, c := range []struct {
		args []string
		name string
	}{{[]string{"explain mcp"}, "work"}, {[]string{"doctor"}, "work"}, {[]string{"mcp"}, ""}, {[]string{"mcp", "add", "x", "y"}, "work"}} {
		if got := mcpHint(c.args, c.name); got != "" {
			t.Errorf("%+v: unexpected hint %q", c, got)
		}
	}
}
