package shim

import (
	"errors"
	"os"
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
