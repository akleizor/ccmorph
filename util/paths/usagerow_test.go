package paths

import (
	"strings"
	"testing"
)

func TestSameNamedRowsIsTheStoresMergeRule(t *testing.T) {
	for _, step := range []struct {
		name string
		a, b string
		want bool
	}{
		{"identical bytes", `{"response_id":"r","turn_id":"t"}`, `{"response_id":"r","turn_id":"t"}`, true},
		{"same id and turn, different cost",
			`{"response_id":"r","turn_id":"t","usage":{"cost":1}}`,
			`{"response_id":"r","turn_id":"t","usage":{"cost":2}}`, true},
		{"same id different turn",
			`{"response_id":"r","turn_id":"a"}`, `{"response_id":"r","turn_id":"b"}`, false},
		{"turn only on one side",
			`{"response_id":"r","turn_id":"t","timestamp":"s"}`,
			`{"response_id":"r","timestamp":"s"}`, false},
		{"different id", `{"response_id":"a","turn_id":"t"}`, `{"response_id":"b","turn_id":"t"}`, false},
		{"missing id", `{"turn_id":"t","usage":{"cost":1}}`, `{"turn_id":"t","usage":{"cost":2}}`, false},
		{"empty turn needs both timestamps",
			`{"response_id":"r","turn_id":"","timestamp":"s"}`,
			`{"response_id":"r","timestamp":"s"}`, true},
		{"empty turn with different timestamps",
			`{"response_id":"r","timestamp":"s1"}`, `{"response_id":"r","timestamp":"s2"}`, false},
		{"empty turn with a missing timestamp",
			`{"response_id":"r","timestamp":"s"}`, `{"response_id":"r"}`, false},
		{"a NaN cost is damage in the price, not a second turn",
			`{"response_id":"r","turn_id":"t","usage":{"cost":1}}`,
			`{"response_id":"r","turn_id":"t","usage":{"cost":NaN}}`, true},
		{"a NaN turn token is not a turn",
			`{"response_id":"r","turn_id":"t"}`, `{"response_id":"r","turn_id":NaN}`, false},
		{"a NaN timestamp is not a timestamp",
			`{"response_id":"r","timestamp":NaN,"usage":{"cost":1}}`,
			`{"response_id":"r","timestamp":NaN,"usage":{"cost":2}}`, false},
		{"a numeric timestamp beside an empty turn",
			`{"response_id":"r","timestamp":"s"}`, `{"response_id":"r","timestamp":5}`, false},
		{"a numeric timestamp beside a turn still merges",
			`{"response_id":"r","turn_id":"t","timestamp":"s1"}`,
			`{"response_id":"r","turn_id":"t","timestamp":5}`, true},
		{"a duplicated member reads as its last spelling",
			`{"response_id":"a","response_id":"r","turn_id":"t"}`,
			`{"response_id":"r","turn_id":"t"}`, true},
		{"unparseable is never a merge", `{`, `{"response_id":"r"}`, false},
	} {
		t.Run(step.name, func(t *testing.T) {
			if got := SameNamedRows([]byte(step.a), []byte(step.b)); got != step.want {
				t.Fatalf("SameNamedRows(%s, %s) = %v, want %v", step.a, step.b, got, step.want)
			}
			if got := SameNamedRows([]byte(step.b), []byte(step.a)); got != step.want {
				t.Fatalf("rule is not symmetric on %s: %v", step.name, got)
			}
		})
	}
}

func TestNonFiniteToNullRewritesTokensNotText(t *testing.T) {
	out := NonFiniteToNull([]byte(`{"note":"NaN for you","text":"Infinity\\NaN","cost":NaN,"e":Infinity,"i":-Infinity}`))
	want := `{"note":"NaN for you","text":"Infinity\\NaN","cost":null,"e":null,"i":null}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
	if !strings.Contains(string(out), "NaN for you") {
		t.Fatal("stored text was rewritten")
	}
}
