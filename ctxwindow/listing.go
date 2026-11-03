package ctxwindow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

type (
	obj = map[string]any
	val = any
)

const (
	catalogueURL          = "https://models.dev/api.json"
	canonicalCatalogueURL = "https://models.dev/models.json"
	catalogueTimeout      = listTimeout
	sourceRetrySeconds    = 600
	// Launch waits on this, so each catalogue fetch gets less than a background run does.
	launchCatalogueTimeout = 3
)

// Unresolved is the one reason a model can end a metadata-only run with no
// number; the summary keys its "here is how to resolve it" block off this note.
const Unresolved = "no size published by the gateway, and no catalogue entry for this model id"

// libDir is where lib/ccmorph.sh lives; shareDir holds the bundled catalogue.
func libDir() string {
	if v := os.Getenv("CCMORPH_LIB_DIR"); v != "" {
		return v
	}
	return filepath.Join(ExecDir(), "..", "lib")
}

func ccmorphScript() string { return filepath.Join(libDir(), "ccmorph.sh") }

// probeShareDir is a var so tests can point it at a fixture. It follows CCMORPH_LIB_DIR; the resolver's ShareDir follows the executable (shortcut: two roots kept to preserve both commands' behaviour, unify when one is chosen).
var probeShareDir = func() string { return filepath.Join(libDir(), "..", "share") }

func asObj(v val) obj { o, _ := v.(map[string]any); return o }

// parseJSON decodes one document; numbers stay json.Number so an integer
// literal is told from a float and a big one is not rounded.
func parseJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: extra data after the document")
	}
	return v, nil
}

func jsonString(v any) string { b, _ := json.Marshal(v); return string(b) }

func asStr(v val) (string, bool) { s, ok := v.(string); return s, ok }

func asPosInt(v val) (int64, bool) {
	i, ok := anyInt(v)
	return i, ok && i > 0
}

// anyInt is `type(v) is int`.
func anyInt(v val) (int64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	return i, err == nil
}

// parseDict decodes JSON that must be an object; ok is false for anything else.
func parseDict(b []byte) obj {
	v, err := parseJSON(b)
	if err != nil {
		return nil
	}
	return asObj(v)
}

// ListingRows are the chat-capable /v1/models rows under their true ids.
func ListingRows(data val) []obj {
	var rows []obj
	list, _ := data.([]any)
	for _, r := range list {
		row := asObj(r)
		if row == nil {
			continue
		}
		id, ok := asStr(getv(row, "id"))
		if !ok || id == "" {
			continue
		}
		if caps := asObj(getv(row, "capabilities")); caps != nil {
			if b, ok := getv(caps, "chat_completion").(bool); ok && !b {
				continue
			}
		}
		c := maps.Clone(row)
		c["id"] = RealID(id)
		rows = append(rows, c)
	}
	return rows
}

func getv(o obj, k string) val { return o[k] }

// DeclaredSizes reads explicit limits; equal values are not evidence of fabrication.
func DeclaredSizes(rows []obj) map[string]int64 {
	out := map[string]int64{}
	for _, row := range rows {
		id, ok := asStr(getv(row, "id"))
		if !ok || id == "" {
			continue
		}
		for _, f := range DeclaredFields {
			if n, ok := asPosInt(getv(row, f)); ok {
				out[id] = n
				break
			}
		}
	}
	return out
}

var numCtx = regexp.MustCompile(`(?m)^\s*num_ctx\s+([0-9]+)\s*$`)

// ListModels asks base's /v1/models for ids, declared sizes and rows.
func ListModels(ctx context.Context, base, key string, timeout time.Duration, op Opener) ([]string, map[string]int64, []obj, error) {
	op = pick(op)
	body, err := op.Open(ctx, Request{Method: "GET", URL: APIURL(base, "/v1/models"), Header: [][2]string{
		{"Authorization", "Bearer " + key}, {"x-api-key", key}, {"anthropic-version", "2023-06-01"}}}, timeout)
	if err != nil {
		return nil, nil, nil, err
	}
	doc, perr := parseJSON(body)
	if perr != nil {
		return nil, nil, nil, perr
	}
	top := asObj(doc)
	if top == nil {
		return nil, nil, nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyType(doc))
	}
	rows := ListingRows(getv(top, "data"))
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i], _ = asStr(getv(r, "id"))
	}
	sizes := DeclaredSizes(rows)
	for _, row := range rows {
		model, _ := asStr(getv(row, "id"))
		if _, has := sizes[model]; has || getv(row, "owned_by") != "ollama" {
			continue
		}
		// Ollama's listing identifies the protocol but omits the served num_ctx.
		// /api/show is metadata, not generation; architectural context_length can
		// exceed the configured window and must not substitute for num_ctx.
		reqBody := obj{"model": model}
		resp, err := op.Open(ctx, Request{Method: "POST", URL: strings.TrimRight(base, "/") + "/api/show",
			Body: []byte(jsonString(reqBody)), Header: [][2]string{
				{"content-type", "application/json"}, {"Authorization", "Bearer " + key}, {"x-api-key", key}}}, timeout)
		if err != nil {
			continue // Missing metadata stays unknown; never start inference.
		}
		show := parseDict(resp)
		if show == nil {
			continue
		}
		params, ok := asStr(getv(show, "parameters"))
		if !ok {
			if _, present := show["parameters"]; present {
				continue
			}
			params = ""
		}
		if m := numCtx.FindStringSubmatch(params); m != nil {
			if n, ok := Positive(m[1]); ok {
				sizes[model] = n
			}
		}
	}
	sort.Strings(ids)
	return ids, sizes, rows, nil
}

func pyType(v val) string {
	switch t := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if strings.ContainsAny(string(t), ".eE") {
			return "float"
		}
		return "int"
	case []any:
		return "list"
	}
	return "dict"
}

// fetchJSONDict is a var so tests run offline. It never fails: unreachable,
// slow or malformed all mean "no catalogue fallback available". It sends an
// explicit User-Agent because models.dev answers 403 to a bare default one.
var fetchJSONDict = func(ctx context.Context, url string, timeout time.Duration) obj {
	body, err := pick(nil).Open(ctx, Request{Method: "GET", URL: url, Header: [][2]string{{"User-Agent", userAgent}}}, timeout)
	if err != nil {
		return obj{}
	}
	if o := parseDict(body); o != nil {
		return o
	}
	return obj{}
}

// FetchCatalogue is the models.dev provider/model catalogue, or {} on any failure.
var FetchCatalogue = func(ctx context.Context, timeout time.Duration) obj {
	return fetchJSONDict(ctx, catalogueURL, timeout)
}

// FetchCanonicalCatalogue is the flat canonical-id catalogue; offline it falls
// back to the bundled snapshot and says so.
var FetchCanonicalCatalogue = func(ctx context.Context, timeout time.Duration) obj {
	if live := fetchJSONDict(ctx, canonicalCatalogueURL, timeout); len(live) > 0 {
		return live
	}
	b, err := os.ReadFile(filepath.Join(probeShareDir(), "model-catalogue.json"))
	if err != nil {
		return obj{}
	}
	snapshot := parseDict(b)
	if snapshot == nil {
		return obj{}
	}
	m, present := snapshot["models"]
	if !present {
		m = obj{}
	}
	models := asObj(m)
	if models == nil {
		return obj{}
	}
	retrieved := "unknown date"
	if r, ok := snapshot["retrieved"]; ok {
		retrieved = pyStr(r)
	}
	fmt.Fprintf(stderr, "Catalogue unavailable; using bundled snapshot from %s (models.dev).\n", retrieved)
	return models
}

// pyStr is asStr(v) for a JSON value.
func pyStr(v val) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return t
	case json.Number:
		return string(t)
	}
	return jsonString(v)
}

// stderr is where progress goes; tests swap it.
var stderr io.Writer = os.Stderr

// CatalogueWindow is an exact provider+model or unambiguous bare-model catalogue
// match. There is no family, alias or fuzzy matching: a gateway's owned_by is not
// guaranteed to be the catalogue's provider slug, so a bare id is only trusted
// when it is unique across the whole catalogue.
func CatalogueWindow(catalogue obj, modelID, ownedBy string) *Limit {
	var entries []obj
	if ownedBy != "" {
		if prov := asObj(getv(catalogue, ownedBy)); prov != nil {
			if exact := asObj(getv(prov, "models")); exact != nil {
				if e := asObj(getv(exact, modelID)); e != nil {
					entries = []obj{e}
				}
			}
		}
	}
	if len(entries) == 0 {
		for _, k := range slices.Sorted(maps.Keys(catalogue)) {
			prov := asObj(getv(catalogue, k))
			if prov == nil {
				continue
			}
			if models := asObj(getv(prov, "models")); models != nil {
				if e := asObj(getv(models, modelID)); e != nil {
					entries = append(entries, e)
				}
			}
		}
		if len(entries) != 1 {
			return nil // absent, or present under 2+ providers: no confident pick
		}
	}
	return LimitWindow(getv(entries[0], "limit"))
}

// CatalogueTag is the provenance record stored beside a catalogue-derived window.
func CatalogueTag(found *Limit) obj {
	return obj{"source": "catalogue", "value": found.Value, "input": numOrNil(found.Input),
		"context": numOrNil(found.Context), "fetched": time.Now().UTC().Format("2006-01-02")}
}

func numOrNil(p *int64) val {
	if p == nil {
		return nil
	}
	return *p
}

// CatalogueLookup is the models.dev window for one id; cache memoizes each
// catalogue for the caller's run.
func CatalogueLookup(ctx context.Context, model, ownedBy string, cache *Cache, timeout time.Duration) *Limit {
	if cache.api == nil {
		cache.api = FetchCatalogue(ctx, timeout)
		cache.apiFetches++
	}
	found := CatalogueWindow(cache.api, model, ownedBy)
	if found == nil {
		// api.json's per-provider nesting was ambiguous or empty (a common shape
		// for reselling gateways); try the flat, provider-independent catalogue.
		if cache.flat == nil {
			cache.flat = FetchCanonicalCatalogue(ctx, timeout)
			cache.flatFetches++
		}
		found = CanonicalCatalogueWindow(cache.flat, model, ownedBy)
	}
	return found
}

// Cache holds what one run fetched once: the catalogues and window sources.
type Cache struct {
	api, flat               obj
	apiFetches, flatFetches int
	sources                 []val
	sourcesLoaded           bool
	tables                  map[string]map[string][2]int64 // url -> model -> {default (-1 = none), window}
	launch                  bool
}

// NewCache returns an empty run cache; launch marks the launch path, which
// remembers an unreachable window source for ten minutes.
func NewCache(launch bool) *Cache {
	return &Cache{launch: launch, tables: map[string]map[string][2]int64{}}
}

func (c *Cache) sourceTable(ctx context.Context, source obj, timeout time.Duration) map[string][2]int64 {
	u, _ := asStr(getv(source, "url"))
	rowsKey, _ := asStr(getv(source, "rows"))
	idKey, _ := asStr(getv(source, "id"))
	windowKey, _ := asStr(getv(source, "window"))
	defaultKey, _ := asStr(getv(source, "default"))
	name := "unnamed"
	if n, ok := source["name"]; ok {
		name = pyStr(n)
	}
	slot := "source:" + u
	down := filepath.Join(StateDir(), "cache", "window-source-"+name+".unreachable")
	if _, ok := c.tables[slot]; !ok && c.launch {
		if fi, err := os.Stat(down); err == nil && time.Since(fi.ModTime()).Seconds() < sourceRetrySeconds {
			c.tables[slot] = map[string][2]int64{}
		}
	}
	if _, ok := c.tables[slot]; !ok {
		table := map[string][2]int64{}
		if _, isStr := asStr(getv(source, "url")); isStr {
			rows := fetchJSONDict(ctx, u, timeout)[rowsKey]
			list, _ := rows.([]any)
			for _, r := range list {
				row := asObj(r)
				if row == nil {
					continue
				}
				model, ok := asStr(getv(row, idKey))
				if !ok {
					continue
				}
				if window, ok := asPosInt(getv(row, windowKey)); ok {
					def := int64(-1)
					if defaultKey != "" {
						if d, ok := anyInt(getv(row, defaultKey)); ok {
							def = d
						}
					}
					table[model] = [2]int64{def, window}
				}
			}
		}
		c.tables[slot] = table
		if len(table) > 0 {
			os.Remove(down)
		} else if c.launch {
			if os.MkdirAll(filepath.Dir(down), 0o777) == nil {
				if f, err := os.OpenFile(down, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
					f.Close()
					now := time.Now()
					os.Chtimes(down, now, now)
				}
			}
		}
	}
	return c.tables[slot]
}

// ProviderWindows applies the window sources a profile opts into (contract:
// share/window-sources.json). It returns the raised sizes and the models whose
// source could not be read: those stay undecided this run, because a stored
// bare declaration would pin them.
func ProviderWindows(ctx context.Context, declared map[string]int64, rows []obj, fields map[string]string, timeout time.Duration, cache *Cache) (map[string]int64, map[string]bool) {
	wanted := map[string]bool{}
	for _, n := range strings.Split(fields["window_sources"], ",") {
		if n != "" {
			wanted[n] = true
		}
	}
	pending := map[string]bool{}
	if len(wanted) == 0 || len(declared) == 0 {
		return declared, pending
	}
	if !cache.sourcesLoaded {
		cache.sourcesLoaded = true
		if b, err := os.ReadFile(filepath.Join(probeShareDir(), "window-sources.json")); err == nil {
			if doc := parseDict(b); doc != nil {
				cache.sources, _ = getv(doc, "sources").([]any)
			}
		}
	}
	out := maps.Clone(declared)
	for _, s := range cache.sources {
		source := asObj(s)
		if source == nil {
			continue
		}
		name, _ := asStr(getv(source, "name"))
		ownedBy, isList := getv(source, "owned_by").([]any)
		if !wanted[name] || !isList {
			continue
		}
		models := map[string]bool{}
		for _, row := range rows {
			owner := getv(row, "owned_by")
			for _, o := range ownedBy {
				if reflect.DeepEqual(owner, o) {
					if id, _ := asStr(getv(row, "id")); out[id] != 0 {
						models[id] = true
					}
					break
				}
			}
		}
		if len(models) == 0 {
			continue // nothing this source speaks for: no fetch
		}
		table := cache.sourceTable(ctx, source, timeout)
		if len(table) == 0 {
			for m := range models {
				pending[m] = true
			}
			continue
		}
		for m := range models {
			entry, ok := table[m]
			if !ok {
				continue
			}
			cur := out[m]
			if entry[0] != -1 && cur == entry[0] && entry[1] > cur {
				out[m] = entry[1]
			}
		}
	}
	return out, pending
}
