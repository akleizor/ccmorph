package shim

import (
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- estimated cost for upstreams that bill outside the response -----------------
// A gateway like llmgate puts its billed `cost` in the usage object. A provider
// reached directly (DeepSeek's own API) returns tokens only, so its rows would
// carry no spend at all. For those rows the shim computes the cost from the four
// token classes and the provider's published per-million-token price, read from
// models.dev (api.json), the catalogue ccmorph already uses for context windows.
// The provider is the one whose `api` host is this route's upstream host, so no
// per-profile table exists; an upstream models.dev does not know gets no cost,
// exactly as before. A billed cost always wins.

const (
	pricesURL    = "https://models.dev/api.json"
	pricesMaxAge = 24 * time.Hour
)

// price is US dollars per million tokens, as models.dev states it.
type price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

// estimate is the dollar cost of a usage row at price p, or false when the row
// has no usable token count. A class the provider lists no price for (DeepSeek
// has no cache write price) is charged at the input price.
// shortcut: models.dev context tiers (a dearer rate past ~272k tokens) are not
// applied, so a very long OpenAI-style prompt is under-estimated; upgrade when a
// direct profile on such a provider needs it.
func (p price) estimate(u usageRow) (float64, bool) {
	count := func(key string) float64 {
		for _, n := range u {
			if n.key == key {
				if f, ok := finiteNumber(n.raw); ok && f > 0 {
					return f
				}
			}
		}
		return 0
	}
	or := func(v *float64) float64 {
		if v == nil {
			return p.Input
		}
		return *v
	}
	in, out := count("input_tokens"), count("output_tokens")
	read, write := count("cache_read_input_tokens"), count("cache_creation_input_tokens")
	if in+out+read+write == 0 {
		return 0, false
	}
	c := (in*p.Input + out*p.Output + read*or(p.CacheRead) + write*or(p.CacheWrite)) / 1e6
	return math.Round(c*1e9) / 1e9, c >= 0 && c < costLimit
}

// priceBook resolves a served model's price for one upstream. It loads lazily on
// the first row that needs it and never blocks the relay: until it has loaded, a
// lookup finds nothing, and the row is rewritten with every usage update.
type priceBook struct {
	host  string
	path  string
	fetch func() ([]byte, error)
	now   func() time.Time
	once  sync.Once
	mu    sync.RWMutex
	table map[string]price // models.dev id of this host's provider models
}

func newPriceBook(upstream string) *priceBook {
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return &priceBook{host: strings.ToLower(u.Hostname()), path: filepath.Join(stateDir(), "prices-"+strings.ToLower(u.Hostname())+".json"), fetch: fetchPrices, now: time.Now}
}

func fetchPrices() ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", pricesURL, nil)
	req.Header.Set("User-Agent", "ccmorph-gateway-shim") // models.dev answers 403 to Go's default
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("models.dev: %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

// parsePrices is the models of every models.dev provider whose api host is host
// that state a price, first provider id winning a shared model id.
func parsePrices(api []byte, host string) map[string]price {
	var doc map[string]struct {
		API    string `json:"api"`
		Models map[string]struct {
			Cost *price `json:"cost"`
		} `json:"models"`
	}
	if jsonv2.Unmarshal(api, &doc) != nil {
		return nil
	}
	out := map[string]price{}
	ids := make([]string, 0, len(doc))
	for id := range doc {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if u, err := url.Parse(doc[id].API); err != nil || strings.ToLower(u.Hostname()) != host {
			continue
		}
		for m, v := range doc[id].Models {
			if _, taken := out[m]; !taken && v.Cost != nil {
				out[m] = *v.Cost
			}
		}
	}
	return out
}

// priceCache is one host's slice of api.json, kept in the state dir so a session
// does not re-download 6 MB.
type priceCache struct {
	Host    string           `json:"host"`
	Fetched int64            `json:"fetched"`
	Models  map[string]price `json:"models"`
}

func (b *priceBook) load() {
	var c priceCache
	if data, err := os.ReadFile(b.path); err == nil && jsonv2.Unmarshal(data, &c) == nil && c.Host == b.host {
		b.set(c.Models)
	}
	if c.Host == b.host && b.now().Sub(time.Unix(c.Fetched, 0)) < pricesMaxAge {
		return
	}
	api, err := b.fetch()
	if err != nil {
		return // a stale table keeps serving; the next session retries
	}
	models := parsePrices(api, b.host)
	if models == nil {
		return
	}
	b.set(models)
	if data, err := jsonv2.Marshal(priceCache{b.host, b.now().Unix(), models}); err == nil {
		os.MkdirAll(filepath.Dir(b.path), 0o700)
		tmp := b.path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil && os.Rename(tmp, b.path) != nil {
			os.Remove(tmp)
		}
	}
}

func (b *priceBook) set(m map[string]price) {
	b.mu.Lock()
	b.table = m
	b.mu.Unlock()
}

// lookup is the price of model, starting the load on first use.
func (b *priceBook) lookup(model string) (price, bool) {
	b.once.Do(func() { go b.load() })
	b.mu.RLock()
	defer b.mu.RUnlock()
	p, ok := b.table[strings.TrimSuffix(model, "[1m]")]
	return p, ok
}
