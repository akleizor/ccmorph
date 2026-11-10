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
	"time"
)

const (
	oversizedMaxTokens = 100_000_000
	// A model nothing could size is asked about again after this many days, not
	// on every launch: a new catalogue entry is worth finding, a repeated wait is not.
	missRetryDays = 7
)

var validationWindow = regexpMust(`(?i)(?:max_model_len|max_total_tokens|maximum context length is)\D{0,20}(\d[\d,]*)`)

// ValidationWindow is the window a gateway states when it refuses an impossible
// max_tokens. The request is rejected at validation, before any generation, and
// many servers (vLLM behind most internal gateways) name the model's total
// length in that refusal.
func ValidationWindow(ctx context.Context, base, key, model string, op Opener, timeout time.Duration) int64 {
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
			return 0 // accepted: nothing stated, nothing more to ask
		}
		he, ok := err.(*HTTPError)
		if !ok {
			continue // a slow or rate-limited shape says nothing; ask the other one
		}
		detail := unicodeGT.Replace(decodeReplace(he.Body))
		if m := validationWindow.FindStringSubmatch(detail); m != nil {
			if n, ok := parseCount(m[1]); ok {
				return n
			}
		}
	}
	return 0
}

func regexpMust(s string) *regexp.Regexp { return regexp.MustCompile(s) }

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

// SizeListing sizes, before launch, every listed model this profile has no
// limit for yet. The listing is the one launch already fetched, so a launch
// where every model is known costs a local comparison and nothing else. A model
// the gateway added since gets its declared size, else a models.dev estimate,
// else a dated miss that stops the lookup repeating for a week. Silent by
// design: the outcome goes to the log, never to the person launching.
func SizeListing(ctx context.Context, profile, body, base, key string, op Opener) int {
	var rows []obj
	var fields map[string]string
	doc, err := parseJSON([]byte(body))
	top := asObj(doc)
	path, perr := ProfilePath(profile)
	if err != nil || top == nil || perr != nil {
		return 0 // not a listing: nothing to size
	}
	rows = ListingRows(getv(top, "data"))
	if _, fields, err = ReadConf(path); err != nil {
		return 0
	}
	expected := ProfileIdentity(profile)
	known := DiscoveredValues(fields)
	for m := range ManualValues(fields) {
		known[m] = 0
	}
	excluded := map[string]bool{}
	for _, m := range strings.Split(fields["excluded_models"], ",") {
		excluded[m] = true
	}
	today := time.Now().UTC().Format("2006-01-02")
	misses := DiscoveredMisses(fields)
	var todo []obj
	for _, row := range rows {
		id, _ := asStr(getv(row, "id"))
		if !ValidModel(id) || excluded[id] {
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
	values := map[string]int64{}
	sources := map[string]obj{}
	missed := map[string]string{}
	cache := NewCache(true)
	to := launchCatalogueTimeout * time.Second
	declared, pending := ProviderWindows(ctx, DeclaredSizes(todo), todo, fields, to, cache)
	var rest []obj
	for _, row := range todo {
		if id, _ := asStr(getv(row, "id")); !pending[id] { // retried next launch
			rest = append(rest, row)
		}
	}
	for _, row := range rest {
		model, _ := asStr(getv(row, "id"))
		if v, ok := declared[model]; ok {
			values[model] = v
			continue
		}
		ownedBy, _ := asStr(getv(row, "owned_by"))
		if found := CatalogueLookup(ctx, model, ownedBy, cache, to); found != nil {
			values[model] = found.Value
			sources[model] = CatalogueTag(found)
			continue
		}
		// A provider bridge answers from the user's subscription, and a probe it
		// accepts generates a reply: size such a model from metadata only.
		paid := fields["type"] == "provider"
		var stated int64
		if base != "" && key != "" && !paid {
			stated = validationWindowFn(ctx, base, key, model, op, to)
		}
		if stated > 0 {
			values[model] = stated
		} else {
			missed[model] = today
		}
	}
	if saved, err := SaveDiscovered(profile, values, expected, false, sources, missed); err != nil {
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
			row := fmt.Sprintf("  %s %s%s", pad(model, 30), padLeft(commas(changed[model]), 11), note)
			if runes(row) <= reportWidth {
				fmt.Fprintln(stderr, row)
				continue
			}
			// A model id too long for the row goes on its own line, with its
			// size and provenance under it, so the id is never split.
			fmt.Fprintln(stderr, "  "+model)
			fmt.Fprintln(stderr, "    "+commas(changed[model])+note)
		}
	}
	if failed {
		return 1, false
	}
	return 0, false
}
