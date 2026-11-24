package ctxwindow

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
)

// #747: one shape verdict serves every reader of a /v1/models reply. Only a
// real data list — empty or not — is the endpoint's own answer; a body that is
// not an object, or whose data is absent, null or not a list, is no listing
// and must never read as a confident empty one.
func TestListingShapeReadsEveryReplyShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want ListingVerdict
	}{
		{"valid list", `{"data":[{"id":"m"}]}`, ListingServed},
		{"empty list", `{"data":[]}`, ListingConfidentlyEmpty},
		{"data missing", `{}`, ListingNotAListing},
		{"data null", `{"data":null}`, ListingNotAListing},
		{"data non-list", `{"data":{"id":"m"}}`, ListingNotAListing},
		{"top-level error object", `{"error":{"message":"upstream is unwell"}}`, ListingNotAListing},
		{"top-level array", `[{"id":"m"}]`, ListingNotAListing},
		{"non-JSON", `upstream is unwell`, ListingNotAListing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, got := ListingShapeBody(tc.body)
			if got != tc.want {
				t.Fatalf("verdict %d, want %d for %s", got, tc.want, tc.body)
			}
			if tc.want == ListingNotAListing && rows != nil {
				t.Fatalf("a non-listing must carry no rows, got %v", rows)
			}
			if tc.want != ListingNotAListing && len(rows) != map[ListingVerdict]int{
				ListingServed: 1, ListingConfidentlyEmpty: 0,
			}[tc.want] {
				t.Fatalf("rows %v for %s, want the reply's own data list", rows, tc.body)
			}
		})
	}
}

// #767: a 200 whose body is a proxy's error envelope is not the endpoint's
// empty listing. ListModels read its data raw, so it answered with success and
// no models, and discovery blamed the route for the endpoint's non-answer.
func TestListModelsRefusesA200ThatIsNotAListing(t *testing.T) {
	f := &fake{reply: func(Request) ([]byte, error) {
		return []byte(`{"error":{"type":"upstream_error"}}`), nil
	}}
	ids, sizes, rows, err := ListModels(context.Background(), "http://gw.invalid", "k", time.Second, f)
	if !errors.Is(err, ErrNotAListing) {
		t.Fatalf("error %v, want the reply refused as not a listing", err)
	}
	if ids != nil || sizes != nil || rows != nil {
		t.Fatalf("a refused reply carried models: %v %v %v", ids, sizes, rows)
	}
}

// The note a person reads must name the endpoint's answer. Before the fix the
// requested model was reported "is not on this route" for a listing the
// endpoint never sent.
func TestDiscoveryNamesTheEndpointForA200ThatIsNotAListing(t *testing.T) {
	dir := state(t)
	put(t, filepath.Join(dir, "gw.conf"), "type=apikey\nbase_url=http://gw.invalid\napi_key={{SECRET_767only1111}}\n")
	stubs(t, func(string) (map[string]string, error) {
		return map[string]string{"ANTHROPIC_API_KEY": "k", "ANTHROPIC_BASE_URL": "http://gw.invalid"}, nil
	}, func(string) ([]string, map[string]int64, []obj, error) {
		t.Fatal("the stub listing must be replaced by the real ListModels")
		return nil, nil, nil, nil
	})
	listModelsFn = func(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, map[string]int64, []obj, error) {
		return ListModels(ctx, base, key, timeout, &fake{reply: func(Request) ([]byte, error) {
			return []byte(`{"error":{"type":"upstream_error"}}`), nil
		}})
	}
	priorWalk = func([]string) (map[string]int64, map[string]obj) { return nil, nil }
	o := DefaultOptions()
	o.OnlySet, o.Only = true, map[string]bool{"any-model": true}
	captureStderr(t)
	r := ProbeAll(context.Background(), []string{"gw"}, o)
	if hasNote(r, "is not on this route") {
		t.Fatalf("discovery blamed the route for the endpoint's reply: %v", r.Notes)
	}
	if !hasNote(r, "the /v1/models reply is not a listing") {
		t.Fatalf("discovery did not name the endpoint's cause: %v", r.Notes)
	}
}

// The encoding/json map a caller already decoded (the adapter inventory) and
// the ordjson object the picker parses reach the same verdict.
func TestListingShapeReadsBothDecoders(t *testing.T) {
	doc := map[string]any{"data": []any{map[string]any{"id": "m"}}}
	if _, verdict := ListingShape(doc); verdict != ListingServed {
		t.Fatalf("map reply: verdict %d, want served", verdict)
	}
	obj, err := ordjson.Parse([]byte(`{"data":[{"id":"m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, verdict := ListingShape(obj); verdict != ListingServed {
		t.Fatalf("ordjson reply: verdict %d, want served", verdict)
	}
}
