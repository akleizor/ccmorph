package util

import (
	"io"
	"os"
	"strings"
)

// CompinitRuns reports whether the rc starts zsh's completion system: a line
// that runs compinit, or the Oh My Zsh loader that runs it. A commented-out
// compinit starts nothing, and naming the Oh My Zsh directory does not load
// it, so neither counts (#449). The one owner of the rule (#555): doctor's
// completion row asks it in-process and install.sh's hint asks it through
// `util compinit-runs`, so the two verdicts cannot drift apart — the
// installer's own zsh spelling of the scan once disagreed with doctor on an
// rc whose last line has no trailing newline, on a user's extendedglob, and
// on a # inside quotes.
func CompinitRuns(rc []byte) bool {
	for _, line := range strings.Split(string(rc), "\n") {
		code := zshCode(line)
		if strings.Contains(code, "compinit") || strings.Contains(code, "oh-my-zsh.sh") {
			return true
		}
	}
	return false
}

// zshCode is line with its trailing comment cut: zsh starts one at an unquoted
// # that begins a word, so a # inside a word stays part of the code.
func zshCode(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'':
			i++ // the next byte is quoted, not syntax
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return line[:i]
		}
	}
	return line
}

// compinitRunsVerb is `util compinit-runs <rc>`, a verb install.sh runs for
// its completion hint: exit 0 when the rc starts zsh's completion system, 1
// when it does not or the rc could not be read — the status is the verdict,
// so no output has to be parsed.
func compinitRunsVerb(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 1 {
		return 2
	}
	rc, err := os.ReadFile(args[0])
	if err != nil {
		return 1
	}
	if CompinitRuns(rc) {
		return 0
	}
	return 1
}
