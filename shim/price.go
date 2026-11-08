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
	pricesWait   = 3 * time.Second
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
	host       string
	path       string
	fetch      func() ([]byte, error)
	now        func() time.Time
	once       sync.Once
	done       chan struct{}                 // closed when the first load ends
	waited     sync.Once                     // only the first lookup waits for it
	parse      func([]byte) map[string]price // nil: parsePrices by host
	cachedOnly bool                          // read the disk slice, never fetch
	mu         sync.RWMutex
	table      map[string]price // models.dev id of this host's provider models
	fallback   *priceBook       // asked when this table has no price for a model
}

func newPriceBook(upstream string) *priceBook {
	u, err := url.Parse(upstream)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return &priceBook{done: make(chan struct{}), host: strings.ToLower(u.Hostname()), path: filepath.Join(stateDir(), "prices-"+strings.ToLower(u.Hostname())+".json"), fetch: fetchPrices, now: time.Now}
}

func fetchPrices() ([]byte, error) {
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", pricesURL, nil)
	req.Header.Set("User-Agent", "ccmorph-shim") // models.dev answers 403 to Go's default
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

// priceDoc is the slice of models.dev/api.json both price parsers read.
type priceDoc = map[string]struct {
	API    string `json:"api"`
	NPM    string `json:"npm"`
	Models map[string]struct {
		Cost *price `json:"cost"`
	} `json:"models"`
}

// parsePrices is the models of every models.dev provider whose api host is host
// that state a price, first provider id winning a shared model id.
func parsePrices(api []byte, host string) map[string]price {
	var doc priceDoc
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

// parseOwnedPrices is the models of the models.dev provider named only, or of
// every provider when only is empty, first provider id winning a shared model id.
// models.dev gives a first-party vendor (anthropic, openai) no `api` host, so the
// host lookup above can never price a subscription's Claude models; this one asks
// by provider id instead. With no id, a vendor's own entry is preferred to a
// reseller's: models.dev nests a vendor under `npm: "@ai-sdk/<vendor>"`, and the
// first sorted id otherwise wins.
func parseOwnedPrices(api []byte, only string) map[string]price {
	var doc priceDoc
	if jsonv2.Unmarshal(api, &doc) != nil {
		return nil
	}
	ids := make([]string, 0, len(doc))
	for id := range doc {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if only == "" {
		vendor := func(id string) bool {
			npm := doc[id].NPM
			return strings.HasPrefix(npm, "@ai-sdk/") && npm != "@ai-sdk/openai-compatible"
		}
		slices.SortStableFunc(ids, func(a, b string) int {
			switch va, vb := vendor(a), vendor(b); {
			case va == vb:
				return strings.Compare(a, b)
			case va:
				return -1
			default:
				return 1
			}
		})
	}
	out := map[string]price{}
	for _, id := range ids {
		if only != "" && id != only {
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

// newOwnedPriceBook prices by models.dev provider id: only names one provider
// (a subscription's own vendor), empty means any provider, its vendor's entry
// first. It reads the same api.json and keeps the same state-dir cache.
func newOwnedPriceBook(only string) *priceBook {
	label := only
	if label == "" || label == "any" { // "any" is the name a launch's hook passes
		label, only = "any", ""
	}
	return &priceBook{
		done:  make(chan struct{}),
		host:  "owned:" + label,
		path:  filepath.Join(stateDir(), "prices-owned-"+label+".json"),
		fetch: fetchPrices,
		now:   time.Now,
		parse: func(api []byte) map[string]price { return parseOwnedPrices(api, only) },
	}
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
	if b.parse != nil {
		models = b.parse(api)
	}
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

// CachedOwnedPriceBook prices one models.dev provider from the slice already on
// disk. A Stop hook runs once a turn inside the session's stop: it must answer
// from the cache or not at all, never wait on a catalogue fetch.
func CachedOwnedPriceBook(only string) *priceBook {
	b := newOwnedPriceBook(only)
	b.cachedOnly = true
	return b
}

// RefreshOwnedPrices fetches the models.dev slice and caches it, so a later
// hook can price a turn. The launcher calls it in the background at launch.
func RefreshOwnedPrices(only string) { newOwnedPriceBook(only).load() }

// readCache is the cached slice for this book, or an empty table.
func (b *priceBook) readCache() map[string]price {
	var c priceCache
	data, err := os.ReadFile(b.path)
	if err != nil || jsonv2.Unmarshal(data, &c) != nil || c.Host != b.host {
		return map[string]price{}
	}
	return c.Models
}

// lookup is the price of model, starting the load on first use.
func (b *priceBook) lookup(model string) (price, bool) {
	if b.cachedOnly {
		b.mu.Lock()
		if b.table == nil {
			b.table = b.readCache()
		}
		table := b.table
		b.mu.Unlock()
		return lookupIn(table, model)
	}
	b.once.Do(func() { go func() { defer close(b.done); b.load() }() })
	// The session's first turn waits briefly for the cached table (a disk read) so it is
	// priced; a cold fetch that outlasts the wait leaves that row unpriced.
	b.waited.Do(func() {
		select {
		case <-b.done:
		case <-time.After(pricesWait):
		}
	})
	b.mu.RLock()
	p, ok := lookupIn(b.table, model)
	b.mu.RUnlock()
	if !ok && b.fallback != nil {
		return b.fallback.lookup(model)
	}
	return p, ok
}

// lookupIn finds model in one price table. Claude Code can name a dated id
// (claude-opus-4-5-20251101) for a model models.dev lists undated too; the date
// is spelling, not another model.
func lookupIn(table map[string]price, model string) (price, bool) {
	name := strings.TrimSuffix(model, "[1m]")
	if p, ok := table[name]; ok {
		return p, true
	}
	if i := strings.LastIndexByte(name, '-'); i > 0 && len(name)-i-1 == 8 && allDigits(name[i+1:]) {
		p, ok := table[name[:i]]
		return p, ok
	}
	return price{}, false
}
