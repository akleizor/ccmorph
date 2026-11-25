package shim

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// apiDoc is the slice of models.dev/api.json the lookup reads: two providers on
// one host (the first id wins a shared model), one elsewhere.
const apiDoc = `{
 "deepseek":{"api":"https://api.deepseek.com","models":{
   "deepseek-flash":{"cost":{"input":0.15,"output":0.6,"reasoning":0.6,"cache_read":0.003}},
   "deepseek-v4-pro":{"cost":{"input":0.66,"output":1.98,"cache_read":0.022}},
   "free":{"name":"no cost key"}}},
 "zz-reseller":{"api":"https://api.deepseek.com/v1","models":{"deepseek-flash":{"cost":{"input":9,"output":9}}}},
 "openai":{"api":"https://api.openai.com/v1","models":{"gpt":{"cost":{"input":2,"output":10,"cache_read":0.1,"cache_write":2.5}}}}}`

func usageOf(s string) usageRow {
	in := []byte(s)
	return bucketsOf(in, Span{0, len(in)}, true)
}

func TestCachedOwnedPriceLookup(t *testing.T) {
	dir := stateHome(t)
	seedOwnedPrices(t, dir, "any", map[string]price{"example": {Input: 0.15, Output: 0.6}})
	book := CachedOwnedPriceBook("")
	if in, out, ok := book.Lookup("example-FP8"); !ok || in != 0.15 || out != 0.6 {
		t.Fatalf("got %v, %v, %v", in, out, ok)
	}
	if _, _, ok := book.Lookup("missing"); ok {
		t.Fatal("missing model must be unpriced")
	}
}

func TestEstimateIsTokenClassesTimesPrice(t *testing.T) {
	flash := parsePrices([]byte(apiDoc), "api.deepseek.com")["deepseek-flash"]
	// The row from a real ds turn: 2659 input + 9 output + 30336 cached tokens.
	c, ok := flash.estimate(usageOf(`{"input_tokens":2659,"output_tokens":9,"cache_read_input_tokens":30336}`))
	if want := (2659*0.15 + 9*0.6 + 30336*0.003) / 1e6; !ok || c != float64(int64(want*1e9+0.5))/1e9 {
		t.Fatalf("%v %v want %v", c, ok, want)
	}
	// No cache write price: cache creation is charged as input.
	c, _ = flash.estimate(usageOf(`{"cache_creation_input_tokens":1000000}`))
	if c != 0.15 {
		t.Fatal(c)
	}
	oa := parsePrices([]byte(apiDoc), "api.openai.com")["gpt"]
	if c, _ = oa.estimate(usageOf(`{"cache_creation_input_tokens":1000000,"cache_read_input_tokens":1000000}`)); c != 2.6 {
		t.Fatal(c)
	}
	if _, ok := flash.estimate(usageOf(`{}`)); ok {
		t.Fatal("a row with no tokens is not priced")
	}
}

func TestPricesAreThisHostsProvidersOnly(t *testing.T) {
	m := parsePrices([]byte(apiDoc), "api.deepseek.com")
	if len(m) != 2 || m["deepseek-flash"].Input != 0.15 || m["gpt"].Input != 0 || m["free"].Output != 0 {
		t.Fatal(m)
	}
	if len(parsePrices([]byte(apiDoc), "llm.example.com")) != 0 || parsePrices([]byte("nope"), "x") != nil {
		t.Fatal("unknown host or bad json must price nothing")
	}
}

func TestRowGetsAComputedCostOnlyWhenNoneWasBilled(t *testing.T) {
	dir := stateHome(t)
	u := newSidecar("ds", "identity")
	u.prices = newPriceBook("https://api.deepseek.com/anthropic")
	u.prices.fetch = func() ([]byte, error) { return []byte(apiDoc), nil }
	u.prices.lookup("deepseek-flash") // starts the load
	deadline := time.Now().Add(5 * time.Second)
	for _, ok := u.prices.lookup("deepseek-flash"); !ok && time.Now().Before(deadline); _, ok = u.prices.lookup("deepseek-flash") {
		time.Sleep(5 * time.Millisecond)
	}
	post := func(id, model, usage string) {
		u.response([]byte(`{"id":"`+id+`","model":"`+model+`","usage":`+usage+`}`), strp(model))
	}
	post("a", "deepseek-flash", `{"input_tokens":1000000,"output_tokens":1000000}`)
	post("b", "deepseek-flash", `{"input_tokens":1000000,"cost":3}`)
	post("c", "unlisted", `{"input_tokens":1000000}`)
	got := strings.Join(rows(t, dir), "\n")
	// A computed amount is its own key: `cost` stays what the gateway billed, so a
	// reader that sums billed spend can never add a list price it did not ask for.
	if !strings.Contains(got, `"estimated_cost":0.75`) || !strings.Contains(got, `"cost":3}`) {
		t.Fatal(got)
	}
	if strings.Count(got, `"cost"`) != 1 || strings.Count(got, `"estimated_cost"`) != 1 {
		t.Fatal(got)
	}
}

func TestPriceCacheServesWithoutFetchingUntilStale(t *testing.T) {
	stateHome(t)
	var fetches atomic.Int32
	now := time.Unix(1_000_000, 0)
	book := func() *priceBook {
		b := newPriceBook("https://api.deepseek.com")
		b.now = func() time.Time { return now }
		b.fetch = func() ([]byte, error) { fetches.Add(1); return []byte(apiDoc), nil }
		return b
	}
	book().load()
	b := book()
	b.load()
	if fetches.Load() != 1 || b.table["deepseek-flash"].Output != 0.6 {
		t.Fatal(fetches.Load(), b.table)
	}
	now = now.Add(pricesMaxAge + time.Second)
	b = book()
	b.fetch = func() ([]byte, error) { return nil, errors.New("offline") }
	b.load()
	if b.table["deepseek-flash"].Output != 0.6 {
		t.Fatal("a stale cache must keep serving when models.dev is unreachable")
	}
	if info, err := os.Stat(b.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal(err, info)
	}
}

func TestFirstLookupWaitsForTheLoad(t *testing.T) {
	stateHome(t)
	b := newPriceBook("https://api.deepseek.com")
	b.fetch = func() ([]byte, error) { time.Sleep(200 * time.Millisecond); return []byte(apiDoc), nil }
	if _, ok := b.lookup("deepseek-flash"); !ok {
		t.Fatal("the first lookup must see the table the load produced")
	}
}

// ownedDoc is models.dev's shape for a first-party vendor: anthropic and openai
// state no `api` host, and a reseller shares one model id with the vendor.
const ownedDoc = `{
 "anthropic":{"npm":"@ai-sdk/anthropic","models":{
   "claude-opus-5-5":{"cost":{"input":4,"output":20,"cache_read":0.2,"cache_write":5}},
   "claude-haiku-5-5":{"cost":{"input":1,"output":5}}}},
 "openai":{"npm":"@ai-sdk/openai","models":{"gpt-5.6-sol":{"cost":{"input":2,"output":10}}}},
 "zz-reseller":{"npm":"other","api":"https://api.example.com","models":{
   "claude-opus-5-5":{"cost":{"input":9,"output":9}},
   "gpt-5.6-sol":{"cost":{"input":9,"output":9}}}}}`

func TestOwnedPricesReadAProviderWithNoAPIHost(t *testing.T) {
	m := parseOwnedPrices([]byte(ownedDoc), "anthropic")
	if len(m) != 2 || m["claude-opus-5-5"].Input != 4 || m["claude-opus-5-5"].Output != 20 {
		t.Fatalf("%v", m)
	}
	if _, ok := m["gpt-5.6-sol"]; ok {
		t.Fatal("one provider's table must not carry another's model")
	}
	// The host-keyed parser finds nothing here: that is why this exists.
	if len(parsePrices([]byte(ownedDoc), "api.anthropic.com")) != 0 {
		t.Fatal("an entry with no api host is never found by host")
	}
}

func TestOwnedPricesPreferTheVendorOverAReseller(t *testing.T) {
	m := parseOwnedPrices([]byte(ownedDoc), "")
	if m["claude-opus-5-5"].Input != 4 || m["gpt-5.6-sol"].Input != 2 {
		t.Fatalf("a vendor's own entry must win its model: %v", m)
	}
}

// TestPriceTablesAreGolden pins the full parse of the fixture documents
// byte-for-byte (#564): whatever walk serves both price tables must keep the
// shared-model first-wins, the vendor-over-reseller preference and the
// named-provider slice exactly as the two walks before it produced them.
func TestPriceTablesAreGolden(t *testing.T) {
	table := func(m map[string]price) string {
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"host table, first sorted provider wins a shared id",
			table(parsePrices([]byte(apiDoc), "api.deepseek.com")),
			`{"deepseek-flash":{"input":0.15,"output":0.6,"cache_read":0.003,"cache_write":null},"deepseek-v4-pro":{"input":0.66,"output":1.98,"cache_read":0.022,"cache_write":null}}`},
		{"host table of the other host",
			table(parsePrices([]byte(apiDoc), "api.openai.com")),
			`{"gpt":{"input":2,"output":10,"cache_read":0.1,"cache_write":2.5}}`},
		{"host table of a reseller the vendors share ids with",
			table(parsePrices([]byte(ownedDoc), "api.example.com")),
			`{"claude-opus-5-5":{"input":9,"output":9,"cache_read":null,"cache_write":null},"gpt-5.6-sol":{"input":9,"output":9,"cache_read":null,"cache_write":null}}`},
		{"any provider, no vendor nesting in the document",
			table(parseOwnedPrices([]byte(apiDoc), "")),
			`{"deepseek-flash":{"input":0.15,"output":0.6,"cache_read":0.003,"cache_write":null},"deepseek-v4-pro":{"input":0.66,"output":1.98,"cache_read":0.022,"cache_write":null},"gpt":{"input":2,"output":10,"cache_read":0.1,"cache_write":2.5}}`},
		{"one named provider",
			table(parseOwnedPrices([]byte(ownedDoc), "anthropic")),
			`{"claude-haiku-5-5":{"input":1,"output":5,"cache_read":null,"cache_write":null},"claude-opus-5-5":{"input":4,"output":20,"cache_read":0.2,"cache_write":5}}`},
		{"any provider, the vendor's own entry beats a reseller's",
			table(parseOwnedPrices([]byte(ownedDoc), "")),
			`{"claude-haiku-5-5":{"input":1,"output":5,"cache_read":null,"cache_write":null},"claude-opus-5-5":{"input":4,"output":20,"cache_read":0.2,"cache_write":5},"gpt-5.6-sol":{"input":2,"output":10,"cache_read":null,"cache_write":null}}`},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestNewOwnedPriceBookKeysItsCacheByProvider(t *testing.T) {
	stateHome(t)
	b := newOwnedPriceBook("anthropic")
	if b.host != "owned:anthropic" || !strings.HasSuffix(b.path, "prices-owned-anthropic.json") {
		t.Fatalf("%q %q", b.host, b.path)
	}
	if b.parse == nil || b.parse([]byte(ownedDoc))["claude-opus-5-5"].Input != 4 {
		t.Fatal("the owned book must parse by provider id")
	}
	if any := newOwnedPriceBook(""); any.host != "owned:any" || !strings.HasSuffix(any.path, "prices-owned-any.json") {
		t.Fatalf("%q %q", any.host, any.path)
	}
}

func TestLookupIgnoresADateSuffix(t *testing.T) {
	stateHome(t)
	b := newOwnedPriceBook("anthropic")
	b.fetch = func() ([]byte, error) { return []byte(ownedDoc), nil }
	if p, ok := b.lookup("claude-opus-5-5-20251101"); !ok || p.Input != 4 {
		t.Fatalf("%v %v", p, ok)
	}
	if p, ok := b.lookup("claude-opus-5-5[1m]"); !ok || p.Input != 4 {
		t.Fatalf("%v %v", p, ok)
	}
	if _, ok := b.lookup("claude-opus-5-5-notadate"); ok {
		t.Fatal("a suffix that is not a date must not be stripped")
	}
}

func TestLookupIgnoresAServedPrecisionSuffix(t *testing.T) {
	table := map[string]price{"Qwen3.8-Flash-Next": {Input: 0, Output: 0}, "glm-5.3": {Input: 1, Output: 3}}
	for _, m := range []string{"Qwen3.8-Flash-Next-FP8", "glm-5.3-NVFP4", "glm-5.3_awq"} {
		if _, ok := lookupIn(table, m); !ok {
			t.Fatalf("%s must price as its base model", m)
		}
	}
	if _, ok := lookupIn(table, "glm-5.3-air"); ok {
		t.Fatal("a suffix that is not a precision must not be stripped")
	}
}

func TestGatewayRowWithNoBilledCostIsPricedFromTheVendor(t *testing.T) {
	stateHome(t)
	b := newPriceBook("https://llmgate.example.com")
	b.fetch = func() ([]byte, error) { return []byte(apiDoc), nil }
	b.fallback = newOwnedPriceBook("")
	b.fallback.fetch = func() ([]byte, error) { return []byte(ownedDoc), nil }
	if p, ok := b.lookup("gpt-5.6-sol"); !ok || p.Input != 2 {
		t.Fatal("a model the gateway host does not list must take the vendor list price", p, ok)
	}
	if _, ok := b.lookup("unlisted"); ok {
		t.Fatal("a model no provider lists stays unpriced")
	}
}

// edgePriceBody is what the edge's models.dev answer carries.
const edgePriceBody = `{"deepseek":{"api":"https://api.deepseek.com"}}`

// TestExchangeTrustsTheSessionsExtraCA is #829's regression: inside a Remote
// Control edge every host is signed by the session CA, and Go's x509 never
// reads NODE_EXTRA_CA_CERTS by itself. The fetch runs in a fresh test binary
// because net/http reads its proxy environment once per process (the same seam
// netroute's TestStdlibProxyChild uses).
func TestExchangeTrustsTheSessionsExtraCA(t *testing.T) {
	if target := os.Getenv("CCMORPH_TEST_PRICE_EDGE_URL"); target != "" {
		body, err := exchange(context.Background(), target, nil, 5*time.Second)
		if err != nil {
			t.Fatalf("price exchange through the edge proxy: %v", err)
		}
		if string(body) != edgePriceBody {
			t.Fatalf("price exchange returned %q; want %q", body, edgePriceBody)
		}
		return
	}
	cert, ca := edgeCertificate(t)
	proxy := edgeProxyURL(t, cert)
	target := "https://example.com/api.json"
	// The tunnel is used: without the CA the same route stops at x509.
	out, err := runPriceEdgeChild(t, proxy, target, "")
	if err == nil || !strings.Contains(string(out), "certificate signed by unknown authority") {
		t.Fatalf("without NODE_EXTRA_CA_CERTS the exchange was not refused by x509 (err %v): %s", err, out)
	}
	if out, err = runPriceEdgeChild(t, proxy, target, ca); err != nil {
		t.Fatalf("the session's CA was not trusted through the edge proxy (err %v): %s", err, out)
	}
}

// edgeCertificate is the session's CA and a certificate signed with it, in the
// shape mitmproxy presents: the local test server's own certificate, valid for
// example.com and this machine's loopback names.
func edgeCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "session-ca.pem")
	block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, block, 0o600); err != nil {
		t.Fatal(err)
	}
	return srv.TLS.Certificates[0], ca
}

// edgeProxyURL is the edge's route for one test: a CONNECT proxy that answers
// the tunnel with TLS signed by cert, as mitmproxy signs every host. The target
// name is never resolved -- the proxy is the host -- so reaching it needs the
// CA and no network.
func edgeProxyURL(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
				tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
				if err := tc.Handshake(); err != nil {
					return
				}
				defer tc.Close()
				if _, err := http.ReadRequest(bufio.NewReader(tc)); err != nil {
					return
				}
				fmt.Fprintf(tc, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(edgePriceBody), edgePriceBody)
			}(conn)
		}
	}()
	return "http://" + ln.Addr().String()
}

// runPriceEdgeChild asks a fresh test binary for the price exchange's answer,
// through proxy, trusting ca when it names one. net/http caches the proxy
// environment once per process, so only a fresh binary can be asked twice.
func runPriceEdgeChild(t *testing.T, proxy, target, ca string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestExchangeTrustsTheSessionsExtraCA$")
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "NO_PROXY", "no_proxy",
			"REQUEST_METHOD", "NODE_EXTRA_CA_CERTS", "CCMORPH_TEST_PRICE_EDGE_URL":
			continue // the test decides the route and the CA
		}
		env = append(env, kv)
	}
	env = append(env, "HTTPS_PROXY="+proxy, "https_proxy="+proxy, "NO_PROXY=", "no_proxy=",
		"CCMORPH_TEST_PRICE_EDGE_URL="+target)
	if ca != "" {
		env = append(env, "NODE_EXTRA_CA_CERTS="+ca)
	}
	cmd.Env = env
	return cmd.CombinedOutput()
}
