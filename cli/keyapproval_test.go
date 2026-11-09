package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/util"
)

// keyApprovalLauncher builds a profile home holding gw's .claude.json and the
// launcher that would start it (#299).
func keyApprovalLauncher(t *testing.T, body string) (*launcher, string) {
	t.Helper()
	profiles := filepath.Join(t.TempDir(), "profiles")
	home := filepath.Join(profiles, "gw")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return &launcher{profileDirs: profiles}, path
}

// gh299Key is the key of the profile the saved-approval tests refuse and then
// approve; its last 20 characters are what Claude Code stores.
const gh299Key = "sk-issue-299-test-key-0001"

func gh299RefusedBody() string {
	return `{"numStartups":3,"customApiKeyResponses":{"approved":[],"rejected":["` + gh299Key[len(gh299Key)-20:] + `"]}}`
}

// #299: a refusal with no saved approval is the standing refusal — the launch
// stops and the file is not touched behind the person's back.
func TestKeyApprovalWithoutASavedApprovalStillRefuses(t *testing.T) {
	l, path := keyApprovalLauncher(t, gh299RefusedBody())
	before, _ := os.ReadFile(path)
	if l.keyApproval("gw", gh299Key) {
		t.Fatal("a refused key with no saved approval must stop the launch")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatalf("the file was rewritten: %s", after)
	}
}

// #299: the answer `ccmorph login` saved while a session was open is recorded
// here, over the refusal, before Claude Code starts.
func TestKeyApprovalRecordsASavedApprovalOverARefusal(t *testing.T) {
	l, path := keyApprovalLauncher(t, gh299RefusedBody())
	if err := util.SaveApproval(l.profileDirs, "gw", gh299Key); err != nil {
		t.Fatal(err)
	}
	if !l.keyApproval("gw", gh299Key) {
		t.Fatal("a saved approval must be recorded, not waited on")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"numStartups":3`, `"approved":["` + gh299Key[len(gh299Key)-20:] + `"]`} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %s in %s", want, got)
		}
	}
	if util.KeyResponse(filepath.Join(l.profileDirs, "gw"), gh299Key) != util.KeyApproved {
		t.Fatalf("the key does not read back as approved: %s", got)
	}
	if util.ApprovalSaved(l.profileDirs, "gw", gh299Key) {
		t.Fatal("the saved approval stayed after it was recorded")
	}
}

// #299: once Claude Code holds the answer itself, a marker left behind is
// dropped — nothing waits that has already landed.
func TestKeyApprovalDropsASavedApprovalOnceAnswered(t *testing.T) {
	body := `{"customApiKeyResponses":{"approved":["` + gh299Key[len(gh299Key)-20:] + `"],"rejected":[]}}`
	l, _ := keyApprovalLauncher(t, body)
	if err := util.SaveApproval(l.profileDirs, "gw", gh299Key); err != nil {
		t.Fatal(err)
	}
	if !l.keyApproval("gw", gh299Key) {
		t.Fatal("an approved key must let the launch through")
	}
	if util.ApprovalSaved(l.profileDirs, "gw", gh299Key) {
		t.Fatal("the saved approval stayed after the key was already approved")
	}
}
