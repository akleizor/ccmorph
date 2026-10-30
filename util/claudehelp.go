package util

import (
	"fmt"
	"io"
	"regexp"
	"strings"
)

// claudeVerbs is `util claude-verbs` over `claude --help` on stdin: one subcommand
// per line, from the Commands: section's two-space-indented leading words (an
// alias row like `mcp|m` lists both). Anything not listed is not a subcommand.
func claudeVerbs(_ []string, in io.Reader, out, _ io.Writer) int {
	help := string(readAll(in))
	i := strings.Index(help, "\nCommands:")
	if !strings.HasPrefix(help, "Commands:") && i < 0 {
		return 0
	}
	for _, l := range strings.Split(help[i+1:], "\n") {
		if m := verbRow.FindString(l); m != "" {
			for _, v := range strings.Split(strings.TrimSpace(m), "|") {
				fmt.Fprintln(out, v)
			}
		}
	}
	return 0
}

var verbRow = regexp.MustCompile(`^  [a-z][a-z0-9|-]*`)

// claudeOptArities is `util claude-opt-arities` over `claude --help` on stdin:
// `<spelling> <flag|value|optional>` per option row of the Options: section. The
// spec is the row up to the description column; a trailing <x> takes a value, a
// trailing [x] an optional one.
func claudeOptArities(_ []string, in io.Reader, out, _ io.Writer) int {
	inOpts := false
	for _, l := range strings.Split(string(readAll(in)), "\n") {
		switch {
		case strings.HasPrefix(l, "Options:"):
			inOpts = true
		case strings.HasPrefix(l, "Commands:"):
			inOpts = false
		}
		if !inOpts || !strings.HasPrefix(l, "  -") {
			continue
		}
		spec := l[2:]
		if j := strings.Index(spec, "  "); j >= 0 {
			spec = spec[:j]
		}
		arity := "flag"
		if k := strings.LastIndex(spec, " "); k >= 0 {
			switch spec[k+1] {
			case '<':
				arity, spec = "value", spec[:k]
			case '[':
				arity, spec = "optional", spec[:k]
			}
		}
		for _, s := range strings.Split(spec, ", ") {
			fmt.Fprintln(out, s, arity)
		}
	}
	return 0
}
