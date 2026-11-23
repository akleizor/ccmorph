package adapter

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/util"
)

// #747: the three readers of a /v1/models reply — model-known's listing walk,
// the adapter inventory, and the project picker — answer from one shape
// verdict. A data list, empty or not, is the endpoint's own answer; every
// other shape is no listing, never a confident "serves nothing". The picker
// column fails on 593d73b4, where a 200 error object read as an authoritative
// empty listing and printed the profile's config message #723 exists to stop.
func TestTheThreeListingReadersAnswerFromOneShapeVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ctxwindow.ListingVerdict
	}{
		{"valid list", `{"data":[{"id":"wanted"}]}`, ctxwindow.ListingServed},
		{"empty list", `{"data":[]}`, ctxwindow.ListingConfidentlyEmpty},
		{"data missing", `{}`, ctxwindow.ListingNotAListing},
		{"data null", `{"data":null}`, ctxwindow.ListingNotAListing},
		{"data non-list", `{"data":{"id":"wanted"}}`, ctxwindow.ListingNotAListing},
		{"top-level error object", `{"error":{"message":"upstream is unwell"}}`, ctxwindow.ListingNotAListing},
		{"non-JSON", `upstream is unwell`, ctxwindow.ListingNotAListing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, got := ctxwindow.ListingShapeBody(tc.body); got != tc.want {
				t.Fatalf("the shared verdict %d, want %d", got, tc.want)
			}

			// modelInListing (util/misc.go): 0 served, 1 not, 2 cannot tell.
			wantServed := util.ServedUnknown
			switch tc.want {
			case ctxwindow.ListingServed:
				wantServed = util.ServedYes
			case ctxwindow.ListingConfidentlyEmpty:
				wantServed = util.ServedNo
			}
			if got := util.ServedByListing(tc.body, "wanted", ""); got != wantServed {
				t.Errorf("model-known %d, want %d for %s", got, wantServed, tc.body)
			}

			// inventoryListing: cannot tell is an error, never an empty list.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("content-type", "application/json")
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			if _, _, err := inventoryListing(srv.URL, "k", nil); (err != nil) != (tc.want == ctxwindow.ListingNotAListing) {
				t.Errorf("inventory error %v, want error %v for %s", err, tc.want == ctxwindow.ListingNotAListing, tc.body)
			}

			// ProjectPicker: served projects rows, a confident empty refuses
			// with the config message, and no listing is the gateway's state.
			wantPicker := map[ctxwindow.ListingVerdict]util.PickerResult{
				ctxwindow.ListingServed:           util.PickerDone,
				ctxwindow.ListingConfidentlyEmpty: util.PickerRefused,
				ctxwindow.ListingNotAListing:      util.PickerUnreached,
			}[tc.want]
			root := t.TempDir()
			home := filepath.Join(root, "home")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			conf := filepath.Join(root, "gw.conf")
			if err := os.WriteFile(conf, []byte(
				"type=apikey\nbase_url=http://gw.invalid\napi_key={{SECRET_4zoyf4hcvvke}}"+
					"\nexcluded_models=blocked\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, ".claude.json"),
				[]byte(`{"additionalModelOptionsCache":[{"value":"blocked"}]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			var errw bytes.Buffer
			got := util.ProjectPicker(util.PickerIn{Home: home, Conf: conf, Tag: "Gateway", Profile: "gw",
				Layer: filepath.Join(home, "settings.profile.json"), ModelsJSON: tc.body}, &errw)
			if got != wantPicker {
				t.Errorf("picker %d, want %d for %s: %s", got, wantPicker, tc.body, errw.String())
			}
			if wantPicker != util.PickerRefused && bytes.Contains(errw.Bytes(), []byte("revise the list")) {
				t.Errorf("a non-config outcome printed the config message for %s: %s", tc.body, errw.String())
			}
		})
	}
}
