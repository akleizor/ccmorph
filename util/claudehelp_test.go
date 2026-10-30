package util

import (
	"bytes"
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
	var out bytes.Buffer
	claudeVerbs(nil, strings.NewReader(sampleHelp), &out, nil)
	if out.String() != "mcp\nm\ndoctor\nupdate\n" {
		t.Fatalf("verbs %q", out.String())
	}
	out.Reset()
	claudeOptArities(nil, strings.NewReader(sampleHelp), &out, nil)
	want := "-d optional\n--debug optional\n-p flag\n--print flag\n--model value\n--system-prompt value\n"
	if out.String() != want {
		t.Fatalf("arities %q", out.String())
	}
}
