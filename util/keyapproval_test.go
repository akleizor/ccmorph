package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			home := t.TempDir()
			path := filepath.Join(home, ".claude.json")
			if tc.body != "" {
				path = writeClaudeJSON(t, home, tc.body)
			}
			before, _ := os.ReadFile(path)
			if err := ApproveKey(home, key); err != nil {
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
	home := t.TempDir()
	path := writeClaudeJSON(t, home, `{"customApiKeyResponses":"oops"}`)
	if err := ApproveKey(home, "sk-issue-236-test-key"); err == nil {
		t.Fatal("a non-object customApiKeyResponses must be refused")
	}
	if data, _ := os.ReadFile(path); string(data) != `{"customApiKeyResponses":"oops"}` {
		t.Fatalf("the file was changed: %s", data)
	}
	link := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(link, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, filepath.Join(link, ".claude.json")); err != nil {
		t.Skip("no symlink support")
	}
	if err := ApproveKey(link, "sk-issue-236-test-key"); err == nil {
		t.Fatal("a symlinked .claude.json must be refused")
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
