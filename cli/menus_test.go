package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// The effort picker and the help name the levels the conf validator accepts, in
// order: #264 had both stop at xhigh while effort=max loaded fine.
func TestTheEffortPickerAndHelpNameTheConfLevels(t *testing.T) {
	rows := splitRows(effortRows)
	if len(rows) != len(profile.Efforts) {
		t.Fatalf("the picker offers %d levels, the conf accepts %d", len(rows), len(profile.Efforts))
	}
	for i, lvl := range profile.Efforts {
		if id := rowID(rows[i]); id != lvl {
			t.Errorf("picker row %d is %q, want %q", i, id, lvl)
		}
	}
	for _, verb := range []string{"get", "set"} { // each verb has its own page (#263)
		help := usageTopics[verb][1]
		for _, lvl := range profile.Efforts {
			if !strings.Contains(help, lvl) {
				t.Errorf("ccmorph help %s does not name the effort level %q", verb, lvl)
			}
		}
	}
	// The label the picker shows names the value the commands accept, so a word
	// learned in the menu works in set (#367): "Extra high" alone did not.
	for _, lvl := range profile.Efforts {
		if label := effortLabel(lvl); !strings.Contains(strings.ToLower(label), lvl) {
			t.Errorf("the picker labels %q as %q, which does not name the value", lvl, label)
		}
	}
}

// #489: the profile menu and `list` describe the same profile, so the menu
// carries the facts the listing does — the private mark #292 put on `list`
// reached only one of the two engines. The row is the whole contract: the
// kind, the detail under it (the saved model, or Browser login for a provider
// profile with none), and the default mark.
func TestTheProfileMenuRowsCarryTheListingsMarksIssue489(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	state, profiles := filepath.Join(root, "state"), filepath.Join(root, "profiles")
	for _, d := range []string{state, filepath.Join(profiles, "gw"), filepath.Join(profiles, "cx")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	conf := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(state, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	conf("work.conf", "type=sub\nemail=demo@example.com\n")
	conf("tok.conf", "type=token\ntoken={{SECRET_e695kwiyu4nm}}\nprivate=1\n")
	conf("gw.conf", "type=apikey\napi_key={{SECRET_9wwp6b425rxn}}\nbase_url=https://gw.example\n")
	conf("cx.conf", "type=provider\nprovider=codex\n")
	if err := os.WriteFile(filepath.Join(profiles, "gw", "settings.json"), []byte(`{"model":"gw-model"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &launcher{dir: state, profileDirs: profiles}
	rows := map[string]string{}
	for _, r := range l.profileRows("tok", nil) {
		name, _, _ := strings.Cut(r, "\t")
		rows[name] = r
	}
	want := map[string]string{
		"work": "work\tClaude account\tdemo@example.com\t",
		"tok":  "tok\tClaude token\t· private\tdefault",
		"gw":   "gw\tAPI key\thttps://gw.example → gw-model\t",
		"cx":   "cx\tCodex\tBrowser login\t",
	}
	if len(rows) != len(want) {
		t.Fatalf("the menu offers rows for %q, want %q", rows, want)
	}
	for name, row := range want {
		if rows[name] != row {
			t.Errorf("the menu row for %s is %q, want %q", name, rows[name], row)
		}
	}
}

// Both model menus mark the saved model the same way: first, as the Keep row
// `set model -i` opens on (#279). #377 had the launch picker open a bare
// catalogue that ignored it, so Enter alone started the alphabetically first
// model instead of the saved one.
func TestBothModelMenusMarkTheSavedModelAsTheKeepRowIssue377(t *testing.T) {
	rows := "picker-alpha\tfake\npicker-beta\tPicker Beta\n"
	cases := []struct {
		name         string
		current      string
		keep, custom bool
		want         []string
	}{
		{
			name:    "the launch picker with a saved model",
			current: "picker-beta", keep: true, custom: false,
			want: []string{
				"@keep\tKeep picker-beta",
				"picker-alpha\tpicker-alpha\tfake",
			},
		},
		{
			name:    "the set picker with the same saved model",
			current: "picker-beta", keep: true, custom: true,
			want: []string{
				"@keep\tKeep picker-beta",
				"picker-alpha\tpicker-alpha\tfake",
				"@custom\tEnter a custom model ID…",
			},
		},
		{
			name:    "the launch picker with nothing saved",
			current: "", keep: false, custom: false,
			want: []string{
				"picker-alpha\tpicker-alpha\tfake",
				"picker-beta\tpicker-beta\tPicker Beta",
			},
		},
		{
			name:    "the set picker with nothing saved",
			current: "", keep: true, custom: true,
			want: []string{
				"@keep\tKeep none — Claude Code picks the model",
				"picker-alpha\tpicker-alpha\tfake",
				"picker-beta\tpicker-beta\tPicker Beta",
				"@custom\tEnter a custom model ID…",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := menuRows("model", tc.current, "", "", rows, tc.keep, tc.custom)
			if !slices.Equal(got, tc.want) {
				t.Errorf("menuRows(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
	// The Keep row is the row both menus share byte for byte: the marking a
	// person learns in one menu answers in the other.
	launch := menuRows("model", "picker-beta", "", "", rows, true, false)
	set := menuRows("model", "picker-beta", "", "", rows, true, true)
	if launch[0] != set[0] || launch[0] != "@keep\tKeep picker-beta" {
		t.Errorf("the launch menu marks the saved model as %q, the set menu as %q", launch[0], set[0])
	}
}
