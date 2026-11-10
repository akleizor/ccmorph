package cli

import (
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
				"@keep\tKeep Claude default",
				"picker-alpha\tpicker-alpha\tfake",
				"picker-beta\tpicker-beta\tPicker Beta",
				"@custom\tEnter a custom model ID…",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := menuRows("model", tc.current, rows, tc.keep, tc.custom)
			if !slices.Equal(got, tc.want) {
				t.Errorf("menuRows(%q) = %q, want %q", tc.current, got, tc.want)
			}
		})
	}
	// The Keep row is the row both menus share byte for byte: the marking a
	// person learns in one menu answers in the other.
	launch := menuRows("model", "picker-beta", rows, true, false)
	set := menuRows("model", "picker-beta", rows, true, true)
	if launch[0] != set[0] || launch[0] != "@keep\tKeep picker-beta" {
		t.Errorf("the launch menu marks the saved model as %q, the set menu as %q", launch[0], set[0])
	}
}
