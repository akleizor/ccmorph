package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #358: the fold erases every row the value line took, and no more.
func TestValueRowsCountsEveryRowTheValueTook(t *testing.T) {
	for _, c := range []struct {
		value   string
		columns int
		want    int
	}{
		{"high", 100, 1},
		{"", 80, 1},
		{strings.Repeat("a", 100), 100, 1},
		{strings.Repeat("a", 101), 100, 2},
		{strings.Repeat("a", 200), 100, 2},
		{strings.Repeat("a", 201), 100, 3},
		{"picker-alpha", 10, 2},
	} {
		if got := valueRows(c.value, c.columns); got != c.want {
			t.Errorf("valueRows(%d cells, %d columns) = %d, want %d", len(c.value), c.columns, got, c.want)
		}
	}
}

// #788: --json reads the same before or after the profile. A leading flag moves
// behind the profile slot; every other shape is left exactly as it came.
func TestJSONLastMovesTheFlagBehindTheProfile(t *testing.T) {
	for _, c := range []struct {
		args  []string
		want  []string
		moved bool
	}{
		{nil, nil, false},
		{[]string{"work"}, []string{"work"}, false},
		{[]string{"work", "--json"}, []string{"work", "--json"}, false},
		{[]string{"model", "base_url"}, []string{"model", "base_url"}, false},
		{[]string{"--json"}, []string{"", "--json"}, true},
		{[]string{"--json", "work"}, []string{"work", "--json"}, true},
	} {
		got, moved := jsonLast(c.args)
		if moved != c.moved || strings.Join(got, "\x00") != strings.Join(c.want, "\x00") {
			t.Errorf("jsonLast(%q) = %q, %v; want %q, %v", c.args, got, moved, c.want, c.moved)
		}
	}
}

// GH-862 review: a config pass that keeps the saved model (enter through the
// prompts) is not a model preference. It must leave the imported model_source
// marker alone, or list and inspect would start calling the person's Claude
// model the profile's own after a connection edit.
func TestConfigKeepingTheModelKeepsItsImportedSourceIssue862(t *testing.T) {
	root := t.TempDir()
	state, profiles, home := filepath.Join(root, "state"), filepath.Join(root, "profiles"), filepath.Join(root, "home")
	for _, d := range []string{state, filepath.Join(profiles, "gw"), filepath.Join(home, ".claude")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	conf := filepath.Join(state, "gw.conf")
	before := "type=apikey\nbase_url=https://gw.test\napi_key={{SECRET_2x8mwnb2ps6p}}\nmodel=opus\nmodel_source=claude-settings\nmodel_defaults=1\n"
	if err := os.WriteFile(conf, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	// Enter keeps the endpoint, n keeps the credential, then Enter keeps the
	// model and the effort: no prompt asks for a new model.
	stdin := filepath.Join(root, "stdin")
	if err := os.WriteFile(stdin, []byte("\nn\n\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(stdin)
	if err != nil {
		t.Fatal(err)
	}
	oldIn, oldTTY := os.Stdin, isTTY
	os.Stdin, isTTY = in, func(int) bool { return false }
	t.Cleanup(func() { os.Stdin, isTTY = oldIn, oldTTY; in.Close() })
	t.Setenv("HOME", home)
	t.Setenv("CCMORPH_CONTEXT_LOCKED", "gw")
	l := &launcher{dir: state, profileDirs: profiles}
	if rc := l.config([]string{"gw"}); rc != 0 {
		t.Fatalf("config rc %d, want 0", rc)
	}
	after, err := os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"model=opus", "model_source=claude-settings", "model_defaults=1"} {
		if !strings.Contains(string(after), want) {
			t.Fatalf("keeping the model dropped %q:\n%s", want, after)
		}
	}
}
