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
	conf := filepath.Join(dir, "gw.conf")
	os.WriteFile(conf, []byte("type=apikey\napi_key=k\nbase_url=https://g.invalid\nexcluded_models=hidden-model\ncontext_window.pinned=100000\n"), 0o600)
	_, fields, _ := ctxwindow.ReadConf(conf)
	os.MkdirAll(filepath.Dir(ctxwindow.DiscoveredPath(conf)), 0o700)
	os.WriteFile(ctxwindow.DiscoveredPath(conf), []byte(`{"route":"`+ctxwindow.RouteFingerprint(fields)+`","models":{"known-model":300000,"gpt-5.6-sol":372000}}`), 0o600)
	known := `{"data":[{"id":"known-model"},{"id":"claude-fable-5-dd-los-6.5-tpg"},{"id":"hidden-model"},{"id":"pinned"},{"id":"bge-m3","capabilities":{"chat_completion":false}}]}`
	if ListingHasUnsized(conf, known) {
		t.Fatal("an all-known listing must skip the sizer")
	}
	if !ListingHasUnsized(conf, `{"data":[{"id":"known-model"},{"id":"brand-new"}]}`) {
		t.Fatal("a new model must reach the sizer")
	}
	// #767: a 200 that is not a listing cannot be read, so the sizer decides;
	// a raw data read answered "nothing to size" for the endpoint's answer.
	if !ListingHasUnsized(conf, `{"error":{"type":"upstream_error"}}`) {
		t.Fatal("a not-a-listing reply must reach the sizer")
	}
}

// #767: a 200 that is not a listing is no endpoint's empty listing. The
// filtered view read absent data as an empty list and printed it; it now
// refuses the reply rather than answering a confident nothing.
func TestModelsFilteredJSONRefusesANonListingIssue767(t *testing.T) {
	conf := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(conf, []byte("type=apikey\napi_key={{SECRET_767filter1}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"error":"upstream down"}`, `{"data":{"id":"m"}}`, `{}`} {
		if out, ok := ModelsFilteredJSON(conf, body); ok || out != "" {
			t.Errorf("%s: got %q, ok; want the reply refused as unreadable", body, out)
		}
	}
	// Positive witness: a real listing still filters, so the refusals above are
	// the shape verdict and not a reader that never reads its argument.
	out, ok := ModelsFilteredJSON(conf, `{"data":[{"id":"m"}]}`)
	if !ok || !strings.Contains(out, `"m"`) {
		t.Fatalf("a data list: %q, ok=%v; want the row kept", out, ok)
	}
}
