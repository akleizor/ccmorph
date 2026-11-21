package ctxwindow

import (
	"testing"

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
