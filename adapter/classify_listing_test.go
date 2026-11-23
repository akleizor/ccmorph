package adapter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// listingTransport is the counting fake backend for #598, at the same HTTP
// transport seam the package's other stand-in gateways use: it answers
// /v1/models with the ids it holds, each declaring chat, and counts every GET
// of it. A sweep that re-fetches the listing per fresh model shows up here as
// more than one GET, with no socket involved.
type listingTransport struct {
	ids []string

	mu    sync.Mutex
	gets  int
	calls []string
}

func (l *listingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	l.mu.Lock()
	l.calls = append(l.calls, r.Method+" "+r.URL.Path)
	listing := r.Method == "GET" && r.URL.Path == "/v1/models"
	if listing {
		l.gets++
	}
	l.mu.Unlock()
	code, body := 404, `{"error":"no route"}`
	if listing {
		rows := make([]any, len(l.ids))
		for i, id := range l.ids {
			rows[i] = map[string]any{"id": id, "supported_endpoints": []any{"/v1/chat/completions"}}
		}
		raw, _ := json.Marshal(map[string]any{"data": rows})
		code, body = 200, string(raw)
	}
	return &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)),
		Body:   io.NopCloser(strings.NewReader(body)),
		Header: http.Header{"Content-Type": []string{"application/json"}}, Request: r}, nil
}

func (l *listingTransport) listingGets() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gets, append([]string(nil), l.calls...)
}

// The auto sweep answers every fresh model from the listing inventory already
// fetched (#598): the listing is one document, so a pass over N uncached models
// must GET it once. Worse, the old pass made N+1 — inventory's GET, then
// dialectAuto's identical fetch per model — which is what shoved a large
// gateway's launch past selectTimeout.
func TestClassifyAutoFetchesTheListingOnceIssue598(t *testing.T) {
	ids := []string{"alpha-1", "beta-2", "gamma-3", "delta-4", "epsilon-5"}
	gw := &listingTransport{ids: ids}
	swap(t, &client, &http.Client{Transport: gw})
	rows, err := classify("https://gateway", "k", filepath.Join(t.TempDir(), "cache.json"), "", true)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if len(rows) != len(ids) {
		t.Fatalf("classified %d row(s), want %d", len(rows), len(ids))
	}
	for _, r := range rows {
		if r.Dialect != "chat" {
			t.Errorf("%s: dialect %q, want the declared chat", r.Model, r.Dialect)
		}
	}
	if gets, calls := gw.listingGets(); gets != 1 {
		t.Fatalf("GET /v1/models %d times for %d fresh models, want 1 (calls: %v)", gets, len(ids), calls)
	}
}

// A 200 reply that is not a listing — a proxy's error object, a wrapped reply
// whose data is an object — says nothing about what the endpoint serves. The
// auto sweep keeps the verdicts its cache holds, exactly as it does when the
// listing fetch itself fails, instead of reading the broken reply as "serves
// nothing" and leaving the launch with no usable model (#695).
func TestClassifySweepKeepsCachedVerdictsUnderANonListingReply(t *testing.T) {
	for _, body := range []string{`{"error":"upstream down"}`, `{"data":{"id":"m"}}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("content-type", "application/json")
			io.WriteString(w, body)
		}))
		cache := seededCache(t, srv.URL, true, rowJSON(row("chat")))
		rows, err := classify(srv.URL, "k", cache, "", true)
		srv.Close()
		if err != nil {
			t.Fatalf("%s: classify: %v", body, err)
		}
		kept := false
		for _, r := range rows {
			if r.Model == "m" && r.Dialect != "unusable" {
				kept = true
			}
		}
		if !kept {
			t.Errorf("reply %s: the sweep gave servable model m no usable verdict (rows %v), want its cached chat verdict kept", body, rows)
		}
		if cached := cachedRows(srv.URL, cache); len(cached) != 1 || cached[0].Dialect != "chat" {
			t.Errorf("reply %s: cache after the sweep %v, want m kept as chat", body, cached)
		}
	}
}
