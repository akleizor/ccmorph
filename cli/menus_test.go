package cli

import (
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
	help := usageTopics["get"][1] // "set" is the same entry
	for _, lvl := range profile.Efforts {
		if !strings.Contains(help, lvl) {
			t.Errorf("ccmorph help get/set does not name the effort level %q", lvl)
		}
	}
}
