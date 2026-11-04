package ordjson

import (
	"encoding/json"
	"testing"
)

func TestRoundTripKeepsKeyOrderAndNumbers(t *testing.T) {
	in := `{"z":1.0,"a":{"y":[1e22,"é<>"],"b":null},"m":true}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(Marshal(v, false)); got != in {
		t.Fatalf("got %s", got)
	}
	o := v.(*Obj)
	o.Set("z", "new")
	o.Set("n", 2)
	o.Delete("a")
	if got := string(Marshal(o, false)); got != `{"z":"new","m":true,"n":2}` {
		t.Fatalf("got %s", got)
	}
	if got := string(Marshal(Clone(New()), true)); got != "{}" {
		t.Fatalf("got %s", got)
	}
}

func TestParseRejectsBadInput(t *testing.T) {
	for _, s := range []string{``, `{"a":1`, `{"a":1} x`, `[1,]`, `{1:2}`, "{\"k\":\"l\xe9on\"}", "\xff"} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
}

// A lone surrogate is what Node's JSON.stringify writes for a broken pasted
// pair ("a b\ud83d c"). Python kept the escape as it was; decoding it to U+FFFD
// and writing that back silently mutated .claude.json (#48, again in #163).
func TestRoundTripKeepsALoneSurrogate(t *testing.T) {
	in := `{"h":["a b\ud83d c","\udc00"]}`
	v, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(Marshal(v, false)); got != in {
		t.Fatalf("compact:\n got %q\nwant %q", got, in)
	}
	pretty := "{\n  \"h\": [\n    \"a b\\ud83d c\",\n    \"\\udc00\"\n  ]\n}"
	if got := string(Marshal(v, true)); got != pretty {
		t.Fatalf("pretty:\n got %q\nwant %q", got, pretty)
	}
	key := `{"k\ud800":1}`
	kv, err := Parse([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(Marshal(kv, false)); got != key {
		t.Fatalf("key:\n got %q\nwant %q", got, key)
	}
	// A valid pair is one code point, not two escapes.
	pair, err := Parse([]byte(`"\ud83d\ude00"`))
	if err != nil {
		t.Fatal(err)
	}
	if string(Marshal(pair, false)) != `"😀"` {
		t.Fatalf("pair: %q", Marshal(pair, false))
	}
}

// Two integer literals above 2^53 are different numbers; the Float64 fallback
// compared them equal, so an edit between them was skipped on rewrite.
func TestEqualKeepsDistinctNumbersDistinct(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"9007199254740993", "9007199254740992", false},
		{"9007199254740992", "9007199254740993", false},
		{"12345678901234567890", "12345678901234567891", false},
		{"1", "1.0", true},
		{"-0", "0", true},
		{"1e2", "100", true},
		{"1.0000000000000000000001", "1", false},
	} {
		if got := Equal(json.Number(c.a), json.Number(c.b)); got != c.want {
			t.Errorf("%s vs %s = %v want %v", c.a, c.b, got, c.want)
		}
	}
}
