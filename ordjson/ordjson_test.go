package ordjson

import "testing"

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
	for _, s := range []string{``, `{"a":1`, `{"a":1} x`, `[1,]`, `{1:2}`} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
}
