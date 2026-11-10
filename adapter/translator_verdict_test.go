package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TranslatorVerdict answers from what an automatic launch reads without
// inference: the cache, then the listing's declared protocols (GH-272).
func TestTranslatorVerdict(t *testing.T) {
	const base = "https://gw.test/"
	cached := func(model, dialect string) Row {
		return Row{Model: model, Dialect: dialect, Note: "n", Status: 200}
	}
	declares := func(id string, endpoints ...string) string {
		eps := ""
		for i, e := range endpoints {
			if i > 0 {
				eps += ","
			}
			eps += `"` + e + `"`
		}
		return `{"data":[{"id":"` + id + `","supported_endpoints":[` + eps + `]}]}`
	}
	cases := []struct {
		name       string
		rows       []Row
		listing    string
		model      string
		needed, ok bool
	}{
		{"no cache, no listing", nil, "", "", false, false},
		{"cached anthropic", []Row{cached("m-a", "anthropic")}, "", "", false, true},
		{"cached responses", []Row{cached("m-o", "responses")}, "", "", true, true},
		{"declared messages", nil, declares("m-a", "/v1/messages"), "", false, true},
		{"declared chat", nil, declares("m-o", "/v1/chat/completions"), "", true, true},
		{"undeclared listing", nil, `{"data":[{"id":"m-x"}]}`, "", false, false},
		{"model undeclared", nil, `{"data":[{"id":"m-x"}]}`, "m-x", false, false},
		{"cached responses plus undeclared model", []Row{cached("m-o", "responses")}, `{"data":[{"id":"m-x"}]}`, "m-x", true, true},
		{"cached anthropic plus undeclared model", []Row{cached("m-a", "anthropic")}, `{"data":[{"id":"m-x"}]}`, "m-x", false, false},
		{"model with a suffix, declared chat", nil, declares("m-o", "/v1/chat/completions"), "m-o[1m]", true, true},
		{"chat_completion false is not counted", nil,
			`{"data":[{"id":"m-n","capabilities":{"chat_completion":false},"supported_endpoints":["/v1/chat/completions"]}]}`, "", false, false},
		{"messages wins over responses", nil, declares("m-a", "/v1/responses", "/v1/messages"), "", false, true},
		// A refusal or no answer is not knowledge about the model: a launch
		// re-asks it (#375), so the listing's declaration decides (#379).
		{"unusable from a refused message, where the listing declares messages",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 402}}, declares("m-a", "/v1/messages"), "", false, true},
		{"unusable from no answer, where the listing declares chat",
			[]Row{{Model: "m-o", Dialect: "unusable", Note: "n", Status: 0}}, declares("m-o", "/v1/chat/completions"), "", true, true},
		{"named model, unusable from a refused message, declared messages",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 402}}, declares("m-a", "/v1/messages"), "m-a", false, true},
		{"settled unusable stands where the listing declares messages",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 404}}, declares("m-a", "/v1/messages"), "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.rows != nil {
				cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gw.test")[:16]+".json")
				if err := writeCache(cache, "https://gw.test", c.rows); err != nil {
					t.Fatal(err)
				}
			}
			needed, ok := TranslatorVerdict(base, dir, c.model, c.listing)
			if needed != c.needed || ok != c.ok {
				t.Fatalf("got (needed=%v, known=%v), want (%v, %v)", needed, ok, c.needed, c.ok)
			}
		})
	}
}

// AutoPickable answers what an automatic launch — no model named — could pick
// from THIS listing: a model the listing serves that is cached usable or
// declares its dialect (#379). A cached row for a model the listing no longer
// serves decides nothing, exactly as classify() drops it.
func TestAutoPickable(t *testing.T) {
	const base = "https://gw.test/"
	cached := func(model, dialect string) Row {
		return Row{Model: model, Dialect: dialect, Note: "n", Status: 200}
	}
	serves := func(ids ...string) string {
		rows := make([]string, len(ids))
		for i, id := range ids {
			rows[i] = `{"id":"` + id + `"}`
		}
		return `{"data":[` + strings.Join(rows, ",") + `]}`
	}
	declares := func(id string, endpoints ...string) string {
		eps := ""
		for i, e := range endpoints {
			if i > 0 {
				eps += ","
			}
			eps += `"` + e + `"`
		}
		return `{"data":[{"id":"` + id + `","supported_endpoints":[` + eps + `]}]}`
	}
	cases := []struct {
		name    string
		rows    []Row
		listing string
		want    bool
	}{
		{"nothing cached, nothing declared", nil, serves("m-x"), false},
		{"cached usable, still served", []Row{cached("m-a", "anthropic")}, serves("m-a"), true},
		{"cached usable, no longer served", []Row{cached("m-a", "anthropic")}, serves("m-x"), false},
		{"cached usable, empty listing", []Row{cached("m-a", "anthropic")}, `{"data":[]}`, false},
		{"cached unusable, still served", []Row{cached("m-x", "unusable")}, serves("m-x"), false},
		{"cached unusable where the listing declares one", []Row{cached("m-x", "unusable")},
			declares("m-x", "/v1/chat/completions"), false},
		// A gateway's refusal or silence is not knowledge about the model: the
		// launch re-asks it (#375), so it decides nothing here either.
		{"unusable from a refused message, where the listing declares one",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 402}},
			declares("m-a", "/v1/messages"), true},
		{"unusable from no answer, where the listing declares one",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 0}},
			declares("m-a", "/v1/messages"), true},
		{"unusable from a refused message, nothing declared",
			[]Row{{Model: "m-a", Dialect: "unusable", Note: "n", Status: 402}}, serves("m-a"), false},
		{"declared, nothing cached", nil, declares("m-o", "/v1/chat/completions"), true},
		{"chat_completion false is not pickable", nil,
			`{"data":[{"id":"m-n","capabilities":{"chat_completion":false},"supported_endpoints":["/v1/chat/completions"]}]}`, false},
		{"listing that does not parse, usable cache", []Row{cached("m-a", "anthropic")}, "not json", true},
		{"listing that does not parse, unusable cache", []Row{cached("m-a", "unusable")}, "not json", false},
		{"listing that does not parse, no cache", nil, "not json", false},
		{"served model beside a stale one", []Row{cached("old-model", "anthropic")}, serves("m-a"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			if c.rows != nil {
				cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gw.test")[:16]+".json")
				if err := writeCache(cache, "https://gw.test", c.rows); err != nil {
					t.Fatal(err)
				}
			}
			if got := AutoPickable(base, dir, c.listing); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// A cache written before refusal_verdicts may hold an unusable row that was a
// refused message in disguise. A launch re-asks it, so the hop preflight must
// not call the model unpickable on its word: it agrees with classification
// (#379, after #375).
func TestAutoPickableIgnoresAnOldCachesUnusableRow(t *testing.T) {
	const base = "https://gw.test/"
	listing := `{"data":[{"id":"m-1","supported_endpoints":["/v1/messages"]}]}`
	for _, c := range []struct {
		name     string
		refusals any // the cache's refusal_verdicts marker; nil leaves it out
		want     bool
	}{
		{"cache from before refusal_verdicts", nil, true},
		{"cache with the current refusal_verdicts", float64(refusalVerdicts), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			blob := map[string]any{
				"endpoint": "https://gw.test", "anthropic_path": "/v1/messages",
				"thinking_probe": thinkingProbeVersion,
				"rows":           []any{rowJSON(Row{Model: "m-1", Dialect: "unusable", Note: "n", Status: 404})},
			}
			if c.refusals != nil {
				blob["refusal_verdicts"] = c.refusals
			}
			cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gw.test")[:16]+".json")
			if err := os.MkdirAll(filepath.Dir(cache), 0o777); err != nil {
				t.Fatal(err)
			}
			writeCacheJSON(t, cache, blob)
			if got := AutoPickable(base, dir, listing); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The doctor's translator verdict reads the cache as a launch does: an unusable
// row from a cache older than refusal_verdicts is re-asked, so the listing's
// declaration decides, and a model AutoPickable can pick never leaves the
// verdict unknown (#379, after #375).
func TestTranslatorVerdictIgnoresAnOldCachesUnusableRow(t *testing.T) {
	const base = "https://gw.test/"
	listing := `{"data":[{"id":"m-1","supported_endpoints":["/v1/messages"]}]}`
	for _, c := range []struct {
		name       string
		refusals   any // the cache's refusal_verdicts marker; nil leaves it out
		needed, ok bool
	}{
		{"cache from before refusal_verdicts", nil, false, true},
		{"cache with the current refusal_verdicts", float64(refusalVerdicts), false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			blob := map[string]any{
				"endpoint": "https://gw.test", "anthropic_path": "/v1/messages",
				"thinking_probe": thinkingProbeVersion,
				"rows":           []any{rowJSON(Row{Model: "m-1", Dialect: "unusable", Note: "n", Status: 404})},
			}
			if c.refusals != nil {
				blob["refusal_verdicts"] = c.refusals
			}
			cache := filepath.Join(dir, ".adapter-cache", hashHex("https://gw.test")[:16]+".json")
			if err := os.MkdirAll(filepath.Dir(cache), 0o777); err != nil {
				t.Fatal(err)
			}
			writeCacheJSON(t, cache, blob)
			needed, known := TranslatorVerdict(base, dir, "", listing)
			if needed != c.needed || known != c.ok {
				t.Fatalf("got (needed=%v, known=%v), want (%v, %v)", needed, known, c.needed, c.ok)
			}
			if AutoPickable(base, dir, listing) && !known {
				t.Fatal("a model can be picked, yet the translator verdict is unknown")
			}
		})
	}
}
