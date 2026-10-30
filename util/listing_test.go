package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/ctxwindow"
)

func TestListingHasUnsizedSkipsKnownExcludedAndNonChat(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", dir)
	t.Setenv("MODEL_WINDOW_PROFILE_CONF", "")
	conf := filepath.Join(dir, "gw.conf")
	os.WriteFile(conf, []byte("type=apikey\napi_key=k\nbase_url=https://g.invalid\nexcluded_models=hidden-model\ncontext_window.pinned=100000\n"), 0o600)
	_, fields := ctxwindow.ReadConf(conf)
	os.MkdirAll(filepath.Dir(ctxwindow.DiscoveredPath(conf)), 0o700)
	os.WriteFile(ctxwindow.DiscoveredPath(conf), []byte(`{"route":"`+ctxwindow.RouteFingerprint(fields)+`","models":{"known-model":300000,"gpt-5.6-sol":372000}}`), 0o600)
	known := `{"data":[{"id":"known-model"},{"id":"claude-fable-5-dd-los-6.5-tpg"},{"id":"hidden-model"},{"id":"pinned"},{"id":"bge-m3","capabilities":{"chat_completion":false}}]}`
	if listingHasUnsized([]string{"gw"}, strings.NewReader(known), nil, nil) != 1 {
		t.Fatal("an all-known listing must skip the sizer")
	}
	if listingHasUnsized([]string{"gw"}, strings.NewReader(`{"data":[{"id":"known-model"},{"id":"brand-new"}]}`), nil, nil) != 0 {
		t.Fatal("a new model must reach the sizer")
	}
}
