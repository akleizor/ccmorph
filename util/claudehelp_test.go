package util

import (
	"strings"
	"testing"
)

const sampleHelp = `Usage: claude [options] [command] [prompt]

Options:
  -d, --debug [filter]            Enable debug mode
  -p, --print                     Print response and exit
  --model <model>                 Model for the current session
                                  -- wrapped description line
  --system-prompt <prompt>        System prompt

Commands:
  mcp|m                           Configure MCP servers
  doctor                          Check health
  update                          Update
`

func TestClaudeHelpParsers(t *testing.T) {
	if got := strings.Join(ClaudeVerbs(sampleHelp), " "); got != "mcp m doctor update" {
		t.Fatalf("verbs %q", got)
	}
	want := map[string]string{"-d": "optional", "--debug": "optional", "-p": "flag", "--print": "flag", "--model": "value", "--system-prompt": "value"}
	got := ClaudeOptArities(sampleHelp)
	if len(got) != len(want) {
		t.Fatalf("arities %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("arities %v", got)
		}
	}
}
