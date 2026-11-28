package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

// GH-862 review: a model the person answers at config's model prompt is theirs,
// even when it is the imported id itself: the store drops model_source. Enter
// keeps it, and the marker with it.
func TestConfigAnExplicitModelAnswerDropsTheImportedSourceIssue862(t *testing.T) {
	for _, tc := range []struct {
		name, answer string
		source       bool
	}{
		{"the same id typed", "opus", false},
		{"another id typed", "model-a", false},
		{"Enter keeps it", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			state, profiles, home := filepath.Join(root, "state"), filepath.Join(root, "profiles"), filepath.Join(root, "home")
			for _, d := range []string{state, filepath.Join(profiles, "gw"), filepath.Join(home, ".claude")} {
				if err := os.MkdirAll(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			conf := filepath.Join(state, "gw.conf")
			if err := os.WriteFile(conf, []byte("type=apikey\nbase_url=https://gw.test\napi_key=test-key\nmodel=opus\nmodel_source=claude-settings\nmodel_defaults=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Enter keeps the endpoint, n keeps the credential, then the model
			// answer, then Enter keeps the effort.
			stdin := filepath.Join(root, "stdin")
			if err := os.WriteFile(stdin, []byte("\nn\n"+tc.answer+"\n\n"), 0o600); err != nil {
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
			after := readBack(t, conf)
			want := tc.answer
			if want == "" {
				want = "opus"
			}
			if !strings.Contains(after, "\nmodel="+want+"\n") {
				t.Fatalf("the conf does not hold model=%s:\n%s", want, after)
			}
			if got := strings.Contains(after, "model_source=claude-settings"); got != tc.source {
				t.Fatalf("model_source kept=%v, want %v:\n%s", got, tc.source, after)
			}
		})
	}
}
