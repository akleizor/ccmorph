package pycompat

import (
	"strings"
	"testing"
)

// Expected strings are what Python 3 prints for the same input.
func TestRoundTripMatchesPython(t *testing.T) {
	cases := []struct {
		in, pretty, compact string
	}{
		{`{"b":1,"a":[1.0,2.50,1e22,1e-7,-0,123456789012345678901234567890],"é":"日本😀\n\u0001"}`,
			"{\n  \"b\": 1,\n  \"a\": [\n    1.0,\n    2.5,\n    1e+22,\n    1e-07,\n    0,\n    123456789012345678901234567890\n  ],\n  \"é\": \"日本😀\\n\\u0001\"\n}",
			`{"b":1,"a":[1.0,2.5,1e+22,1e-07,0,123456789012345678901234567890],"\u00e9":"\u65e5\u672c\ud83d\ude00\n\u0001"}`},
		{`{}`, `{}`, `{}`},
		{`[[],{}]`, "[\n  [],\n  {}\n]", `[[],{}]`},
		{`[NaN, Infinity, -Infinity, 1E5, 0.1, 100000000000000000.0, 12345.678]`,
			"[\n  NaN,\n  Infinity,\n  -Infinity,\n  100000.0,\n  0.1,\n  1e+17,\n  12345.678\n]",
			`[NaN,Infinity,-Infinity,100000.0,0.1,1e+17,12345.678]`},
	}
	for _, c := range cases {
		v, err := Parse([]byte(c.in))
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		pretty := Marshal(v, Format{Indent: 2})
		if pretty != c.pretty {
			t.Errorf("pretty %s\n got %q\nwant %q", c.in, pretty, c.pretty)
		}
		compact := Marshal(v, Format{Compact: true, ASCII: true})
		if compact != c.compact {
			t.Errorf("compact %s\n got %q\nwant %q", c.in, compact, c.compact)
		}
	}
}

func TestSortedDefaultSeparatorsMatchJsonDumps(t *testing.T) {
	v, _ := Parse([]byte(`{"b":"é","a":{"z":1,"y":[1,2]}}`))
	got := Marshal(v, Format{SortKeys: true, ASCII: true})
	want := `{"a": {"y": [1, 2], "z": 1}, "b": "\u00e9"}`
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
}

func TestParseErrorsUsePythonWording(t *testing.T) {
	cases := map[string]string{
		``:           "Expecting value: line 1 column 1 (char 0)",
		`{`:          "Expecting property name enclosed in double quotes: line 1 column 2 (char 1)",
		`{"a" 1}`:    "Expecting ':' delimiter: line 1 column 6 (char 5)",
		`[1 2]`:      "Expecting ',' delimiter: line 1 column 4 (char 3)",
		"{}\n x":     "Extra data: line 2 column 2 (char 4)",
		`"abc`:       "Unterminated string starting at: line 1 column 1 (char 0)",
		"\"a\x01\"": "Invalid control character at: line 1 column 3 (char 2)",
	}
	for in, want := range cases {
		_, err := Parse([]byte(in))
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", in, err, want)
		}
	}
	if _, err := Parse([]byte("\xff")); err == nil || !strings.Contains(err.Error(), "can't decode byte 0xff in position 0") {
		t.Errorf("bad utf-8: %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat("[", 5000))); err == nil || !strings.Contains(err.Error(), "recursion") {
		t.Errorf("deep: %v", err)
	}
}

func TestEqualFollowsPython(t *testing.T) {
	a, _ := Parse([]byte(`{"a":1,"b":[true]}`))
	b, _ := Parse([]byte(`{"b":[1.0],"a":1.0}`))
	if !Equal(a, b) {
		t.Fatal("1 == 1.0 == True and dict order is irrelevant")
	}
	c, _ := Parse([]byte(`{"a":1}`))
	if Equal(a, c) {
		t.Fatal("different key sets are not equal")
	}
}

func TestObjKeepsFirstPositionOnDuplicateKey(t *testing.T) {
	v, _ := Parse([]byte(`{"a":1,"b":2,"a":3}`))
	if got := Marshal(v, Format{Compact: true}); got != `{"a":3,"b":2}` {
		t.Fatal(got)
	}
}
