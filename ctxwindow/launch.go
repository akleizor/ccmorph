package ctxwindow

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	oversizedMaxTokens = 100_000_000
	// A model nothing could size is asked about again after this many days, not
	// on every launch: a new catalogue entry is worth finding, a repeated wait is not.
	missRetryDays = 7
	// Launch sizes unknown models a few at a time: each one can cost two bounded
	// validation POSTs, and serial they made a slow gateway's launch wait N × 6 s
	// before Claude started (#603).
	sizingWorkers = 8
)

var validationWindow = regexp.MustCompile(`(?i)(?:max_model_len|max_total_tokens|maximum context length is)\D{0,20}(\d[\d,]*)`)

// retryLater reports an HTTP status that defers a request instead of judging
// it: a timeout (408, 504), a rate limit (429), or an overloaded or unreachable
// upstream (502, 503). Such a reply says nothing about the model (#775).
func retryLater(code int) bool {
	switch code {
	case 408, 429, 502, 503, 504:
		return true
	}
	return false
}

// ValidationWindow is the window a gateway states when it refuses an impossible
// max_tokens. The request is rejected at validation, before any generation, and
// many servers (vLLM behind most internal gateways) name the model's total
// length in that refusal. answered reports whether the gateway judged the
// request: it accepted it, or replied with a status other than a retry-later one
// (408, 429, 502, 503, 504). A transport error, a request deadline, the caller's
// cancellation or a retry-later reply states nothing and is not a verdict (#775).
func ValidationWindow(ctx context.Context, base, key, model string, op Opener, timeout time.Duration) (window int64, answered bool) {
	op = pick(op)
	for _, sh := range shapes {
		body := obj{}
		body["model"] = model
		body["max_tokens"] = oversizedMaxTokens
		msg := obj{}
		msg["role"] = "user"
		msg["content"] = "hi"
		body["messages"] = []any{msg}
		stop := "stop"
		if sh.name == "anthropic" {
			stop = "stop_sequences"
		}
		body[stop] = []any{"\n\n"}
		headers := append([][2]string{{"content-type", "application/json"}}, sh.headers(key)...)
		_, err := op.Open(ctx, Request{Method: "POST", URL: APIURL(base, sh.path), Header: headers,
			Body: []byte(jsonString(body))}, timeout)
		if err == nil {
			return 0, true // accepted: nothing stated, nothing more to ask
		}
		he, ok := err.(*HTTPError)
		if !ok {
			continue // no response at all says nothing; ask the other shape
		}
		detail := unicodeGT.Replace(decodeReplace(he.Body))
		if m := validationWindow.FindStringSubmatch(detail); m != nil {
			if n, ok := parseCount(m[1]); ok {
				return n, true
			}
		}
		if !retryLater(he.Code) {
			answered = true // the gateway judged the request and named no window
		}
	}
	return 0, answered
}

func parseCount(s string) (int64, bool) {
	n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
	return n, err == nil && n > 0
}

var unicodeGT = strings.NewReplacer(`\u003e`, ">")

// RecentlyMissed reports whether a dated miss is still inside the retry window.
func RecentlyMissed(day, today string) bool {
	d, ok1 := parseDay(day)
	t, ok2 := parseDay(today)
	if !ok1 || !ok2 {
		return false
	}
	return int(t.Sub(d).Hours()/24) < missRetryDays
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse("2006-01-02", s)
	return t, err == nil
}

// sized is one model's outcome from the sizing pass. answered says the pass
// reached a verdict for it — a size, an answer naming none, or a mode where no
// probe may ask (a provider bridge). A lookup the caller's deadline or the
// probe's own request deadline cut short has answered false and must not become
// a dated miss (#775).
type sized struct {
	model         string
	value         int64
	stated        int64
	source        obj
	declared      bool
	fromCatalogue bool
	answered      bool
}

// collectSizes turns the workers' outcomes into the save's tables. A model
// without a size becomes a dated miss only when the pass reached a verdict for
// it; a cancelled lookup stays unsized and unrecorded, so the next launch asks
// again (#775).
func collectSizes(results []sized, today string) (values map[string]int64, sources map[string]obj, missed map[string]string) {
	values = map[string]int64{}
	sources = map[string]obj{}
	missed = map[string]string{}
	for _, r := range results {
		switch {
		case r.declared:
			values[r.model] = r.value
		case r.fromCatalogue:
			values[r.model] = r.value
			sources[r.model] = r.source
		case r.stated > 0:
			values[r.model] = r.stated
		case r.answered:
			missed[r.model] = today
		}
	}
	return values, sources, missed
}

// SizeListing sizes, before launch, every listed model this profile has no
// limit for yet. The listing is the one launch already fetched, so a launch
// where every model is known costs a local comparison and nothing else. A model
// the gateway added since gets its declared size, else a models.dev estimate,
// else — when the pass reached a verdict — a dated miss that stops the lookup
// repeating for a week. Silent by design: the outcome goes to the log, never to
// the person launching.
func SizeListing(ctx context.Context, profile, body, base, key string, op Opener) int {
	doc, err := parseJSON([]byte(body))
	if err != nil {
		return 0 // not a listing: nothing to size
	}
	data, verdict := ListingShape(doc)
	if verdict == ListingNotAListing {
		// A 200 error envelope is no endpoint's empty listing, and reading it
		// as one is the confident verdict #767's guard forbids. Nothing to
		// size, and no profile read is spent on it.
		return 0
	}
	// One read of the profile carries both the fields and the identity the
	// save pins, so the conf and its sidecar are each opened once — the
	// snapshot form the probe and migrate readers already use (#485, GH-522).
	snap, serr := profileSnapshot(profile)
	if serr != nil || snap == nil {
		return 0
	}
	rows := ListingRows(data)
	fields := snap.Fields
	expected := snap.ID
	// One parse of the discovered record serves the sized table and the misses
	// (GH-561's one-parse rule, carried to the launch read, #636).
	disc := ParseDiscovered(fields)
	known := disc.Values
	for m := range ManualValues(fields) {
		known[m] = 0
	}
	today := time.Now().UTC().Format("2006-01-02")
	misses := disc.Misses
	var todo []obj
	for _, row := range rows {
		id, _ := asStr(getv(row, "id"))
		// IsExcluded is the picker's rule: a [1m] variant and a tier alias of an
		// excluded model are hidden there, so they need no window here (#636).
		if !ValidModel(id) || IsExcluded(fields, id) {
			continue
		}
		if day, ok := misses[id]; ok && RecentlyMissed(day, today) {
			continue
		}
		covered := false
		for pattern := range known {
			if glob(id, pattern) {
				covered = true
				break
			}
		}
		if !covered {
			todo = append(todo, row)
		}
	}
	if len(todo) == 0 {
		return 0
	}
	cache := NewCache(true)
	to := launchCatalogueTimeout * time.Second
	declared, pending := ProviderWindows(ctx, DeclaredSizes(todo), todo, fields, to, cache)
	var rest []obj
	for _, row := range todo {
		if id, _ := asStr(getv(row, "id")); !pending[id] { // retried next launch
			rest = append(rest, row)
		}
	}
	// The remaining models are independent: the catalogue fetch is shared once
	// and each validation POST is bounded on its own, so size a few at a time.
	// Serial, a slow gateway listing N unknown models spent N × 6 s before
	// Claude started (#603). Each row keeps its own outcome, applied in listing
	// order afterwards: a listing can name one model twice, and the serial loop
	// let the later row's answer win.
	results := make([]sized, len(rest))
	work := make(chan int, len(rest))
	var wg sync.WaitGroup
	for range min(sizingWorkers, len(rest)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				row := rest[i]
				r := &results[i]
				r.model, _ = asStr(getv(row, "id"))
				if v, ok := declared[r.model]; ok {
					r.declared, r.value, r.answered = true, v, true
					continue
				}
				ownedBy, _ := asStr(getv(row, "owned_by"))
				if found := CatalogueLookup(ctx, r.model, ownedBy, cache, to); found != nil {
					r.fromCatalogue, r.value, r.source, r.answered = true, found.Value, catalogueTag(found), true
					continue
				}
				// A provider bridge answers from the user's subscription, and a probe it
				// accepts generates a reply: size such a model from metadata only.
				paid := fields["type"] == "provider"
				switch {
				case paid:
					// No request may ask a bridge, so its catalogue-only lookup is the
					// pass's verdict.
					r.answered = true
				case base != "" && key != "":
					// The probe reports whether it got an answer at all: a request that
					// timed out on its own deadline or was cancelled stated nothing and
					// must not become a miss (#775).
					r.stated, r.answered = validationWindowFn(ctx, base, key, r.model, op, to)
				}
				// With no route there was nothing to ask, so there is no verdict: the
				// model stays unsized and unrecorded for a launch that has a route.
			}
		}()
	}
	for i := range rest {
		work <- i
	}
	close(work)
	wg.Wait()
	values, sources, missed := collectSizes(results, today)
	// SaveDiscoveredCtx, not SaveDiscovered: the profile lock this save takes
	// may be held by a person at `ccmorph rm`'s prompt, and the caller's
	// deadline — the launch's --deadline — bounds that wait (#757).
	if saved, err := SaveDiscoveredCtx(ctx, profile, values, expected, false, sources, missed); err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", prog, err)
		return 1
	} else if !saved {
		return 1
	}
	for _, model := range slices.Sorted(maps.Keys(values)) {
		note := ""
		if _, tagged := sources[model]; tagged {
			note = " (catalogue estimate)"
		}
		fmt.Fprintf(stderr, "sized %s:%s %d%s\n", profile, model, values[model], note)
	}
	for _, model := range slices.Sorted(maps.Keys(missed)) {
		fmt.Fprintf(stderr, "no size for %s:%s; launch fallback applies\n", profile, model)
	}
	return 0
}

var validationWindowFn = ValidationWindow

// Commit gives each profile its own write: no shared registry or cross-profile fallback.
func Commit(ctx context.Context, values map[string]int64, baseline map[string]int64, identities map[string]*Identity, sources map[string]obj) (code int, cut bool) {
	grouped := map[string]map[string]int64{}
	groupedSources := map[string]map[string]obj{}
	changes := map[string]map[string]int64{}
	var order []string
	for _, scoped := range slices.Sorted(maps.Keys(values)) {
		value := values[scoped]
		profile, model, _ := strings.Cut(scoped, ":")
		if grouped[profile] == nil {
			grouped[profile] = map[string]int64{}
			order = append(order, profile)
		}
		grouped[profile][model] = value
		if b, ok := baseline[scoped]; !ok || b != value {
			if changes[profile] == nil {
				changes[profile] = map[string]int64{}
			}
			changes[profile][model] = value
		}
		if tag := sources[scoped]; tag != nil {
			if groupedSources[profile] == nil {
				groupedSources[profile] = map[string]obj{}
			}
			groupedSources[profile][model] = tag
		}
	}
	failed := false
	for _, profile := range order {
		if ctx.Err() != nil {
			return 1, true
		}
		models := grouped[profile]
		expected := identities[profile]
		saved, err := false, error(nil)
		if expected != nil {
			saved, err = SaveDiscoveredCtx(ctx, profile, models, expected, true, groupedSources[profile], nil)
		}
		if err != nil {
			fmt.Fprintf(stderr, "%s: %s\n", prog, err)
			return 1, false
		}
		if !saved {
			if ctx.Err() != nil {
				return 1, true
			}
			wrapLine(stderr, fmt.Sprintf("%s: changed during discovery; its context window sizes were not saved", profile), "", "  ")
			failed = true
			continue
		}
		// "wrote 1 context entries to gateway.conf" answers a question nobody has;
		// say what is different from before the command ran.
		changed := changes[profile]
		if len(changed) == 0 {
			wrapLine(stderr, fmt.Sprintf("%s: found context window sizes for %s; all were already saved.",
				profile, plural(len(models), "model")), "", "  ")
			continue
		}
		wrapLine(stderr, fmt.Sprintf("%s: found context window sizes for %s; %d saved for new sessions:",
			profile, plural(len(models), "model"), len(changed)), "", "  ")
		names := make([]string, 0, len(changed))
		for m := range changed {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, model := range names {
			note := ""
			if _, tagged := groupedSources[profile][model]; tagged {
				note = "  (catalogue estimate)"
			}
			row := fmt.Sprintf("  %s %s%s", pad(model, 30), padLeft(Commas(changed[model]), 11), note)
			if Cells(row) <= reportWidth {
				fmt.Fprintln(stderr, row)
				continue
			}
			// A model id too long for the row goes on its own line, with its
			// size and provenance under it, so the id is never split.
			fmt.Fprintln(stderr, "  "+model)
			fmt.Fprintln(stderr, "    "+Commas(changed[model])+note)
		}
	}
	if failed {
		return 1, false
	}
	return 0, false
}
