package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GH-862: the one wording doctor, run and the adapter share for a model a
// launch cannot use.

func modelBlockConf(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gw.conf")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestModelOrigin(t *testing.T) {
	if got := ModelOrigin("claude-settings"); got != "from ~/.claude/settings.json" {
		t.Fatalf("imported: got %q", got)
	}
	for _, source := range []string{"", "anything-else"} {
		if got := ModelOrigin(source); got != "saved in this profile" {
			t.Fatalf("%q: got %q", source, got)
		}
	}
}

func TestServedSample(t *testing.T) {
	conf := modelBlockConf(t, "type=apikey\napi_key=k\n")
	if got := ServedSample(conf, `{"data":[{"id":"model-b"},{"id":"model-a"}]}`); strings.Join(got, ",") != "model-a,model-b" {
		t.Fatalf("two ids: %v", got)
	}
	seven := `{"data":[{"id":"m7"},{"id":"m3"},{"id":"m1"},{"id":"m6"},{"id":"m2"},{"id":"m5"},{"id":"m4"}]}`
	if got := ServedSample(conf, seven); strings.Join(got, ",") != "m1,m2,m3,m4,m5,…" {
		t.Fatalf("seven ids: %v", got)
	}
	excluded := modelBlockConf(t, "type=apikey\napi_key=k\nexcluded_models=hidden\n")
	got := ServedSample(excluded, `{"data":[{"id":"hidden"},{"id":"kept"},{"id":"embed-only","capabilities":{"chat_completion":false}}]}`)
	if strings.Join(got, ",") != "kept" {
		t.Fatalf("an excluded and a non-chat id were named: %v", got)
	}
	if got := ServedSample(conf, "garbage"); got != nil {
		t.Fatalf("garbage: %v", got)
	}
	if got := ServedSample(conf, `{"data":[]}`); got != nil {
		t.Fatalf("empty listing: %v", got)
	}
}

func TestUnservedTier(t *testing.T) {
	want := "opus, from ~/.claude/settings.json, is a Claude model alias this gateway does not serve; " +
		"pick a model this gateway serves: ccmorph set gw862 model -i"
	if got := UnservedTier("gw862", "opus", "from ~/.claude/settings.json", nil); got != want {
		t.Fatalf("got %q", got)
	}
	if got := UnservedTier("gw862", "opus", "from ~/.claude/settings.json", []string{"model-a", "model-b"}); got != want+" (it serves model-a, model-b)" {
		t.Fatalf("with served ids, got %q", got)
	}
}

func TestNoModelReason(t *testing.T) {
	got := NoModelReason("demo")
	for _, want := range []string{"demo has no saved model", "ccmorph models demo", "ccmorph set demo model <model-id>", "a few short test requests", "may incur a charge"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q does not name %q", got, want)
		}
	}
	for _, never := range []string{"declared", "compatibility check"} {
		if strings.Contains(got, never) {
			t.Fatalf("%q still says %q", got, never)
		}
	}
}

func TestExclusionsLeaveNone(t *testing.T) {
	all := modelBlockConf(t, "type=apikey\napi_key=k\nexcluded_models=model-a,model-b\n")
	one := modelBlockConf(t, "type=apikey\napi_key=k\nexcluded_models=model-a\n")
	none := modelBlockConf(t, "type=apikey\napi_key=k\n")
	listing := `{"data":[{"id":"model-a"},{"id":"model-b"}]}`
	if !ExclusionsLeaveNone(all, listing) {
		t.Fatal("every served model excluded: must be true")
	}
	if ExclusionsLeaveNone(one, listing) {
		t.Fatal("one model left: must be false")
	}
	if ExclusionsLeaveNone(none, listing) {
		t.Fatal("no excluded_models: must be false")
	}
	if ExclusionsLeaveNone(all, `{"error":"x"}`) {
		t.Fatal("a body that is not a listing: must be false")
	}
	if ExclusionsLeaveNone(none, `{"data":[]}`) {
		t.Fatal("an empty listing without exclusions is not the list's fault")
	}
	if !ExclusionsLeaveNone(all, `{"data":[]}`) {
		t.Fatal("empty listing with exclusions set: must be true")
	}
	if ExclusionsLeaveNone(filepath.Join(t.TempDir(), "gone.conf"), listing) {
		t.Fatal("a missing conf: must be false")
	}
}

func TestExclusionsRefusal(t *testing.T) {
	got := ExclusionsRefusal("demo", "/state/demo.conf")
	for _, want := range []string{"no cached or served picker models remain", "/state/demo.conf", "ccmorph models demo"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%q does not name %q", got, want)
		}
	}
	for _, never := range []string{"restore gateway access", "endpoint returned no usable model list"} {
		if strings.Contains(got, never) {
			t.Fatalf("%q still says %q", got, never)
		}
	}
}

func TestNoModelToPick(t *testing.T) {
	plain := modelBlockConf(t, "type=apikey\nbase_url=http://gw.invalid\napi_key=k\n")
	excl := modelBlockConf(t, "type=apikey\nbase_url=http://gw.invalid\napi_key=k\nexcluded_models=model-a,model-b\n")
	gone := filepath.Join(t.TempDir(), "gone.conf")
	for _, tc := range []struct {
		name, conf, body string
		want             bool
	}{
		{"empty list", plain, `{"data":[]}`, true},
		{"no chat model", plain, `{"data":[{"id":"emb","capabilities":{"chat_completion":false}}]}`, true},
		{"a chat model", plain, `{"data":[{"id":"model-a"}]}`, false},
		{"an error object", plain, `{"error":"x"}`, false},
		{"garbage", plain, "garbage", false},
		{"no body", plain, "", false},
		{"exclusions remove all", excl, `{"data":[{"id":"model-a"},{"id":"model-b"}]}`, false},
		{"exclusions and an empty list", excl, `{"data":[]}`, false},
		{"a missing conf", gone, `{"data":[]}`, false},
	} {
		if got := NoModelToPick(tc.conf, tc.body); got != tc.want {
			t.Errorf("%s: NoModelToPick = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestNoUsableModel(t *testing.T) {
	want := "the 'demo' gateway's model list names no chat model to pick; save a model id this gateway serves: ccmorph set demo model <model-id>"
	if got := NoUsableModel("demo"); got != want {
		t.Fatalf("NoUsableModel = %q, want %q", got, want)
	}
	if !strings.HasSuffix(want, NoUsableModelFix("demo")) {
		t.Fatalf("the refusal does not end with its fix %q", NoUsableModelFix("demo"))
	}
}
