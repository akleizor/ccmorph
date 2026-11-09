package util

import (
	"os"
	"path/filepath"
	"testing"
)

// A hop may only resume a session whose transcript holds a conversation Claude
// Code can resume: before the first message the file is empty or holds no record
// with the identity Claude Code resumes by, and --resume on such an id ends the
// session at Claude Code's own "No conversation found" (#288).
func TestHasConversationNeedsARecordClaudeCodeCanResume(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	spoken := `{"type":"user","sessionId":"s1","uuid":"u1","timestamp":"2026-01-01T00:00:00.000Z","message":{"role":"user","content":"hi"}}` + "\n"
	for _, c := range []struct {
		name string
		path string
		want bool
	}{
		{"no transcript at all", filepath.Join(dir, "none.jsonl"), false},
		{"an empty transcript", write("empty.jsonl", ""), false},
		{"unparsable lines only", write("plain.jsonl", "first\n"), false},
		{"a record without the session identity", write("sparse.jsonl", `{"type":"user","uuid":"u1","timestamp":"t"}`+"\n"), false},
		{"a record whose identity is blank", write("blank.jsonl", `{"type":"user","sessionId":"","uuid":"","timestamp":""}`+"\n"), false},
		{"the record a first message writes", write("spoken.jsonl", spoken), true},
	} {
		if got := HasConversation(c.path); got != c.want {
			t.Errorf("%s: HasConversation = %v, want %v", c.name, got, c.want)
		}
	}
}
