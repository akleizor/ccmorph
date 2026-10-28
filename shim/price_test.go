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
	if !strings.Contains(got, `"cost":0.75`) || !strings.Contains(got, `"cost":3}`) || strings.Count(got, `"cost"`) != 2 {
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
