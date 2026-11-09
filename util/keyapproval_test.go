package util

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/ordjson"
)

func writeClaudeJSON(t *testing.T, home, body string) string {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApproveKeyRecordsTheTailAndClearsARefusal(t *testing.T) {
	const key = "sk-issue-236-test-key"
	const tail = "k-issue-236-test-key" // 20 characters, as Claude Code stores it
	if len(key) != 21 || keyTail(key) != tail {
		t.Fatalf("fixture key is not 21 characters: %q", key)
	}
	cases := []struct {
		name      string
		body      string
		want      []string // substrings the rewritten file must carry
		untouched bool
	}{
		{
			name: "no file yet",
			want: []string{`"approved":["` + tail + `"]`, `"rejected":[]`},
		},
		{
			name: "an earlier refusal is cleared and other keys stay",
			body: `{"numStartups":3,"customApiKeyResponses":{"approved":[],"rejected":["` + tail + `"]},"theme":"dark"}`,
			want: []string{`"numStartups":3`, `"approved":["` + tail + `"]`, `"rejected":[]`, `"theme":"dark"`},
		},
		{
			name:      "an already approved key leaves the file alone",
			body:      `{"customApiKeyResponses":{"approved":["` + tail + `"],"rejected":[]},"theme":"dark"}`,
			untouched: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profiles := filepath.Join(t.TempDir(), "profiles")
			home := filepath.Join(profiles, "p")
			path := filepath.Join(home, ".claude.json")
			if tc.body != "" {
				path = writeClaudeJSON(t, home, tc.body)
			} else if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if err := ApproveKey(profiles, "p", key); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.untouched {
				if string(after) != string(before) {
					t.Fatalf("the file was rewritten: %s", after)
				}
				return
			}
			for _, want := range tc.want {
				if !strings.Contains(string(after), want) {
					t.Fatalf("missing %s in %s", want, after)
				}
			}
			if strings.Contains(string(after), `"rejected":["`+tail) {
				t.Fatalf("the refusal survived: %s", after)
			}
			if KeyResponse(home, key) != KeyApproved {
				t.Fatalf("the key does not read back as approved: %s", after)
			}
		})
	}
}

func TestApproveKeyRefusesWhatItCannotSafelyRewrite(t *testing.T) {
	profiles := filepath.Join(t.TempDir(), "profiles")
	home := filepath.Join(profiles, "p")
	path := writeClaudeJSON(t, home, `{"customApiKeyResponses":"oops"}`)
	if err := ApproveKey(profiles, "p", "sk-issue-236-test-key"); err == nil {
		t.Fatal("a non-object customApiKeyResponses must be refused")
	}
	if data, _ := os.ReadFile(path); string(data) != `{"customApiKeyResponses":"oops"}` {
		t.Fatalf("the file was changed: %s", data)
	}
	link := filepath.Join(profiles, "x")
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(link, ".claude.json")); err != nil {
		t.Skip("no symlink support")
	}
	if err := ApproveKey(profiles, "x", "sk-issue-236-test-key"); err == nil {
		t.Fatal("a symlinked .claude.json must be refused")
	}
	// A name that is not a profile name names no profile to approve for.
	if err := ApproveKey(profiles, "active", "sk-issue-236-test-key"); err == nil {
		t.Fatal("an invalid profile name must be refused")
	}
}

// #299: a sibling writer of the same .claude.json — SeedPersonAnswers, a live
// session — rewrites it only holding the profile's live lock. A key approval
// that reads and renames without that lock loses a write made between them.
func TestApproveKeyKeepsAWriteMadeBetweenItsReadAndItsRename(t *testing.T) {
	const key = "sk-issue-236-test-key"
	const tail = "k-issue-236-test-key"
	profiles := filepath.Join(t.TempDir(), "profiles")
	if err := os.MkdirAll(filepath.Dir(LiveLockPath(profiles, "p")), 0o700); err != nil {
		t.Fatal(err)
	}
	path := writeClaudeJSON(t, filepath.Join(profiles, "p"), `{"numStartups":3}`)

	// otherWriter behaves like every sibling writer: it rewrites the file only
	// while it holds the live lock exclusively, and reports whether it did.
	otherWriter := func() bool {
		f, err := os.OpenFile(LiveLockPath(profiles, "p"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			return false
		}
		defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		doc, err := loadObj(path, ordjson.New())
		if err != nil {
			t.Fatal(err)
		}
		doc.Set("otherWriter", true)
		if err := atomicJSON(path, doc); err != nil {
			t.Fatal(err)
		}
		return true
	}

	old := approveKeyRead
	t.Cleanup(func() { approveKeyRead = old })
	pending := false
	approveKeyRead = func() { pending = !otherWriter() }

	if err := ApproveKey(profiles, "p", key); err != nil {
		t.Fatal(err)
	}
	if pending && !otherWriter() {
		t.Fatal("the live lock outlived ApproveKey")
	}
	got := mustRead(t, path)
	for _, want := range []string{`"numStartups":3`, `"otherWriter":true`, `"` + tail + `"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in %s — the write made between ApproveKey's read and its rename was lost", want, got)
		}
	}
}

func TestApproveKeyDefersWhileALiveSessionHoldsTheLock(t *testing.T) {
	cases := []struct {
		name string
		how  int
	}{
		{"a live session holds it shared", syscall.LOCK_SH},
		{"a merge holds it exclusively", syscall.LOCK_EX},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profiles := filepath.Join(t.TempDir(), "profiles")
			body := `{"numStartups":3}`
			path := writeClaudeJSON(t, filepath.Join(profiles, "p"), body)
			if err := os.MkdirAll(filepath.Dir(LiveLockPath(profiles, "p")), 0o700); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(LiveLockPath(profiles, "p"), os.O_CREATE|os.O_RDWR, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := syscall.Flock(int(f.Fd()), tc.how); err != nil {
				t.Fatal(err)
			}
			defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			restore := SetLockWait(0)
			defer restore()

			err = ApproveKey(profiles, "p", "sk-issue-236-test-key")

			if err == nil {
				t.Fatal("an approval must not write while another holder has the live lock")
			}
			if !strings.Contains(err.Error(), "a running session or launch of 'p'") {
				t.Fatalf("the refusal does not name the holder: %v", err)
			}
			if after := mustRead(t, path); after != body {
				t.Fatalf("the file was rewritten: %s", after)
			}
		})
	}
}

func TestKeyResponseReadsClaudeCodesAnswer(t *testing.T) {
	const key = "sk-issue-236-test-key"
	const tail = "k-issue-236-test-key"
	cases := []struct {
		name string
		body string
		want KeyState
	}{
		{"no file", "", KeyUnanswered},
		{"no responses object", `{"theme":"dark"}`, KeyUnanswered},
		{"approved", `{"customApiKeyResponses":{"approved":["` + tail + `"],"rejected":[]}}`, KeyApproved},
		{"refused", `{"customApiKeyResponses":{"approved":[],"rejected":["` + tail + `"]}}`, KeyRefused},
		{"refused wins over approved", `{"customApiKeyResponses":{"approved":["` + tail + `"],"rejected":["` + tail + `"]}}`, KeyRefused},
		{"malformed JSON", `{"customApiKeyResponses":`, KeyUnanswered},
		{"responses not an object", `{"customApiKeyResponses":[]}`, KeyUnanswered},
		{"a short key is stored whole", `{"customApiKeyResponses":{"approved":["abc"],"rejected":[]}}`, KeyApproved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.body != "" {
				writeClaudeJSON(t, home, tc.body)
			}
			k := key
			if tc.name == "a short key is stored whole" {
				k = "abc"
			}
			if got := KeyResponse(home, k); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
