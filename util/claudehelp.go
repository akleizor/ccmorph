package util

import (
	"regexp"
	"strings"
)

// ClaudeVerbs lists the subcommands in `claude --help`: the Commands: section's
// two-space-indented leading words (an alias row like `mcp|m` lists both).
// Anything not listed is not a subcommand.
func ClaudeVerbs(help string) (verbs []string) {
	i := strings.Index(help, "\nCommands:")
	if !strings.HasPrefix(help, "Commands:") && i < 0 {
		return nil
	}
	for _, l := range strings.Split(help[i+1:], "\n") {
		if m := verbRow.FindString(l); m != "" {
			verbs = append(verbs, strings.Split(strings.TrimSpace(m), "|")...)
		}
	}
	return verbs
}

var verbRow = regexp.MustCompile(`^  [a-z][a-z0-9|-]*`)

// ClaudeOptArities maps each spelling in the Options: section of `claude --help` to
// flag, value or optional. The spec is the row up to the description column; a
// trailing <x> takes a value, a trailing [x] an optional one.
func ClaudeOptArities(help string) map[string]string {
	arity := map[string]string{}
	inOpts := false
	for _, l := range strings.Split(help, "\n") {
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
		kind := "flag"
		if k := strings.LastIndex(spec, " "); k >= 0 {
			switch spec[k+1] {
			case '<':
				kind, spec = "value", spec[:k]
			case '[':
				kind, spec = "optional", spec[:k]
			}
		}
		for _, s := range strings.Split(spec, ", ") {
			arity[s] = kind
		}
	}
	return arity
}
