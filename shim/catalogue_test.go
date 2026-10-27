package shim

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// gateway is a fake /v1/models. gate, when set, holds every read until closed.
type gateway struct {
	mu     sync.Mutex
	body   string
	status int
	hits   int
	gate   chan struct{}
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.hits++
	body, status, gate := g.body, g.status, g.gate
	g.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if status != 0 {
		w.WriteHeader(status)
	}
	w.Write([]byte(body))
}

func (g *gateway) reads() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

func newGateway(t *testing.T, body string) (*gateway, string) {
	g := &gateway{body: body}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	return g, srv.URL
}

func catalogueAt(url string) *Catalogue { return newCatalogue(url, "", NewServer("").remote) }

// expire makes the cache older than age.
func (c *Catalogue) expire(age time.Duration) { c.readAt = time.Now().Add(-age) }

// shortDeadline shrinks the catalogue's 20 s wait so a trickle costs a fraction of a second.
func shortDeadline(t *testing.T) {
	old := catalogueDeadline
	catalogueDeadline = 300 * time.Millisecond
	t.Cleanup(func() { catalogueDeadline = old })
}

// --- redirects -------------------------------------------------------------------------

func TestCatalogueCrossOriginRedirectDoesNotLeakTheKey(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("x-api-key"), r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{"data":[{"id":"target-model"}]}`))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/models", 302)
	}))
	defer origin.Close()
	// A refused redirect is a transport error the fetch swallows: no ids, so the
	// caller sends the model exactly as asked.
	if got := catalogueAt(origin.URL).served("sk-canary", "some-model"); len(got) != 0 {
		t.Fatal(got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the redirect target was reached with %q", leaked)
	}
}

func TestCatalogueSameOriginRedirectReturnsModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			http.Redirect(w, r, "/models", 302)
			return
		}
		w.Write([]byte(`{"data":[{"id":"same-origin-model"}]}`))
	}))
	defer srv.Close()
	if got := catalogueAt(srv.URL).served("sk-canary", "same-origin-model"); !got["same-origin-model"] {
		t.Fatal(got)
	}
}

// --- deadline --------------------------------------------------------------------------

// A gateway that trickles its headers or its body is unreadable, never a
// catalogue the caller waits on (issue 296).
func TestTricklingCatalogueIsUnreadable(t *testing.T) {
	shortDeadline(t)
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			done := make(chan struct{})
			defer close(done)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.Header().Set("Content-Length", "100000")
					w.Write([]byte(`{"data":[`))
					w.(http.Flusher).Flush()
				}
				select {
				case <-done:
				case <-r.Context().Done():
				}
			}))
			defer srv.Close()
			c := catalogueAt(srv.URL)
			start := time.Now()
			ids := c.served("k", "claude-opus-5")
			if len(ids) != 0 || time.Since(start) > catalogueDeadline+2*time.Second {
				t.Fatal(ids, time.Since(start))
			}
			if got := resolveModel("claude-opus-5", ids); got != "claude-opus-5" {
				t.Fatal(got)
			}
		})
	}
}

// After a trickled read expires, the next refresh reads the healthy catalogue:
// the expired read must not keep owning the refresh (issue 310).
func TestCatalogueRecoversAfterATrickle(t *testing.T) {
	shortDeadline(t)
	var mu sync.Mutex
	reads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reads++
		first := reads == 1
		mu.Unlock()
		if first {
			w.Header().Set("Transfer-Encoding", "chunked")
			w.Write([]byte(`{"data":[ `))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write([]byte(`{"data":[{"id":"claude-opus-5-6"}]}`))
	}))
	defer srv.Close()
	c := catalogueAt(srv.URL)
	if ids := c.served("k", "claude-opus-5"); len(ids) != 0 {
		t.Fatal(ids)
	}
	c.expire(catalogueTTL + time.Second)
	ids := c.served("k", "claude-opus-5")
	if !ids["claude-opus-5-6"] || resolveModel("claude-opus-5", ids) != "claude-opus-5-6" {
		t.Fatal(ids)
	}
}

func TestCatalogueBodySizeLimit(t *testing.T) {
	payload := `{"data":[{"id":"served"}]}`
	for _, tc := range []struct {
		name           string
		size, declared int
		accepted       bool
	}{
		{"exactly the limit", catalogueMaxBytes, catalogueMaxBytes, true},
		{"one byte over", catalogueMaxBytes + 1, catalogueMaxBytes + 1, false},
		{"short of its declared length", len(payload), 500, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := payload + strings.Repeat(" ", tc.size-len(payload))
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(tc.declared))
				w.Write([]byte(body))
			}))
			defer srv.Close()
			if got := catalogueAt(srv.URL).served("k", "served"); got["served"] != tc.accepted {
				t.Fatal(len(got))
			}
		})
	}
}

// Whatever the failure, the caller sends as asked: no ids, no declarations, even
// where an earlier read left a cache (issues 296, 309).
func TestCatalogueReadFailuresDropTheCache(t *testing.T) {
	shortDeadline(t)
	for _, failure := range []string{"http 503", "connection refused", "late", "not json"} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s cached=%v", failure, cached), func(t *testing.T) {
				done := make(chan struct{})
				defer close(done)
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch failure {
					case "http 503":
						w.WriteHeader(503)
					case "late":
						select {
						case <-done:
						case <-r.Context().Done():
						}
					default:
						w.Write([]byte("not JSON"))
					}
				}))
				c := catalogueAt(srv.URL)
				if failure == "connection refused" {
					srv.Close()
				} else {
					defer srv.Close()
				}
				if cached {
					c.ids = map[string]bool{"claude-opus-5-6": true}
					c.endpoints = map[string][]string{"claude-opus-5-6": {pathMessages}}
				}
				c.expire(catalogueTTL + time.Second)
				ids := c.served("k", "claude-opus-5-5")
				if len(ids) != 0 || len(c.endpoints) != 0 || resolveModel("claude-opus-5-5", ids) != "claude-opus-5-5" {
					t.Fatal(ids, c.endpoints)
				}
			})
		}
	}
}

func TestHealthyCatalogueTTL(t *testing.T) {
	for _, tc := range []struct {
		model   string
		age     time.Duration
		refresh bool
	}{
		{"served", 61 * time.Second, false},
		{"missing", 59 * time.Second, false},
		{"missing", 61 * time.Second, true},
		{"served", 599 * time.Second, false},
		{"served", 601 * time.Second, true},
	} {
		t.Run(fmt.Sprintf("%s after %v", tc.model, tc.age), func(t *testing.T) {
			g, url := newGateway(t, `{"data":[{"id":"served"}]}`)
			c := catalogueAt(url)
			if !c.served("k", tc.model)["served"] {
				t.Fatal("first read")
			}
			c.expire(tc.age)
			if !c.served("k", tc.model)["served"] {
				t.Fatal("second read")
			}
			if want := map[bool]int{false: 1, true: 2}[tc.refresh]; g.reads() != want {
				t.Fatalf("%d reads, want %d", g.reads(), want)
			}
		})
	}
}

// --- native edge: the map guard --------------------------------------------------------

// nativeRoutes is a route spec with no translator ("config" absent): the shim
// fronts the gateway directly and may only refuse a map it cannot serve.
func nativeRoutes(t *testing.T, upstream string, models []string, modelMap map[string]string) *ModelRoutes {
	spec, _ := json.Marshal(map[string]any{
		"profile": "p", "identity": "i", "upstream": upstream, "models": models, "model_map": modelMap,
		"timeout": 1, "key_sha256": "x",
	})
	r, err := newModelRoutes(string(spec), NewServer("").remote)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNativeCatalogueUsesTheFirstDuplicateRowEvenIfMalformed(t *testing.T) {
	const count = pathCount
	for _, tc := range []struct {
		first, second string
		path          string
		rejected      bool
		want          []string
	}{
		{`["/v1/messages"]`, `["/v1/chat/completions"]`, pathMessages, false, []string{pathMessages}},
		{`["/v1/chat/completions"]`, `["/v1/messages"]`, pathMessages, true, []string{pathChat}},
		{`null`, `["/v1/chat/completions"]`, pathMessages, false, nil},
		{`[]`, `["/v1/chat/completions"]`, pathMessages, true, []string{pathChat}},
		{`["` + count + `"]`, `["/v1/chat/completions"]`, pathMessages, true, []string{pathChat, count}},
		{`["` + count + `"]`, `["/v1/chat/completions"]`, count, false, []string{pathChat, count}},
		{`["/v1/chat/completions"]`, `["` + count + `"]`, count, false, []string{pathChat, count}},
		{`["/unknown"]`, `["/v1/responses"]`, pathMessages, true, []string{pathResp}},
	} {
		t.Run(tc.first+" then "+tc.second+" on "+tc.path, func(t *testing.T) {
			_, url := newGateway(t, `{"data":[{"id":"m","supported_endpoints":`+tc.first+`},{"id":"m","supported_endpoints":`+tc.second+`}]}`)
			routes := nativeRoutes(t, url, nil, map[string]string{"alias": "m"})
			msg := routes.ensure([]byte(`{"model":"m"}`), "k", strp("alias"), tc.path)
			if (msg != "") != tc.rejected {
				t.Fatalf("rejected=%v: %q", msg != "", msg)
			}
			if got := routes.catalogue.declared("m"); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("declared %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNativeGuardUsesTheCurrentDeclarationForAKnownMappedTarget(t *testing.T) {
	g, url := newGateway(t, `{"data":[{"id":"chat-only","supported_endpoints":["/v1/chat/completions"]}]}`)
	routes := nativeRoutes(t, url, []string{"chat-only"}, map[string]string{"claude-sonnet-5": "chat-only"})
	msg := routes.ensure([]byte(`{"model":"chat-only"}`), "k", strp("claude-sonnet-5"), pathMessages)
	if !strings.Contains(msg, "translation") || g.reads() != 1 {
		t.Fatal(msg, g.reads())
	}
}

// A key carrying the [1m] suffix never matches a wire id, so a spec that still
// holds one decides nothing: the request is not a mapped one, and the guard must
// not go to the gateway or refuse it over a map no lookup applied.
func TestNativeGuardDoesNotTreatASuffixedKeyAsAMap(t *testing.T) {
	g, url := newGateway(t, `{"data":[{"id":"chat-only","supported_endpoints":["/v1/chat/completions"]}]}`)
	routes := nativeRoutes(t, url, nil, map[string]string{"claude-sonnet-5[1m]": "chat-only"})
	if msg := routes.ensure([]byte(`{"model":"claude-sonnet-5"}`), "k", strp("claude-sonnet-5[1m]"), pathMessages); msg != "" || g.reads() != 0 {
		t.Fatal(msg, g.reads())
	}
}

func TestNativeExpiredDeclarationDoesNotVetoAfterAFailedRefresh(t *testing.T) {
	g, url := newGateway(t, "")
	g.status = 503
	routes := nativeRoutes(t, url, nil, map[string]string{"alias": "m"})
	c := routes.catalogue
	c.ids, c.endpoints = map[string]bool{"m": true}, map[string][]string{"m": {pathChat}}
	c.expire(catalogueTTL + time.Second)
	if msg := routes.ensure([]byte(`{"model":"m"}`), "k", strp("alias"), pathMessages); msg != "" {
		t.Fatal(msg)
	}
	if len(c.ids) != 0 || len(c.endpoints) != 0 {
		t.Fatal(c.ids, c.endpoints)
	}
}

// While a refresh is reading, a request for a mapped target still meets the
// declarations: never an empty set that would let an unservable map through.
func TestNativeRefreshNeverShowsAnEmptyDeclarationSet(t *testing.T) {
	g, url := newGateway(t, `{"data":[{"id":"m","supported_endpoints":["/v1/chat/completions"]}]}`)
	routes := nativeRoutes(t, url, nil, map[string]string{"alias": "m"})
	ask := func() string { return routes.ensure([]byte(`{"model":"m"}`), "k", strp("alias"), pathMessages) }
	if !strings.Contains(ask(), "translation") {
		t.Fatal("setup")
	}
	routes.catalogue.expire(catalogueTTL + time.Second)
	g.mu.Lock()
	g.gate = make(chan struct{})
	gate := g.gate
	g.mu.Unlock()
	results := make(chan string, 2)
	go func() { results <- ask() }()
	for g.reads() < 2 { // the refresh is now in flight
		time.Sleep(time.Millisecond)
	}
	go func() { results <- ask() }()
	close(gate)
	for range 2 {
		if msg := <-results; !strings.Contains(msg, "translation") {
			t.Fatalf("a request during a refresh got %q", msg)
		}
	}
}
