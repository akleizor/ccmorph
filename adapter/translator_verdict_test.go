package adapter

import (
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
