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
		if runsCompinit(code) || mentionsWord(code, "oh-my-zsh.sh") {
			return true
		}
	}
	return false
}

// runsCompinit reports whether code names compinit as something that runs: a
// word zsh would read as compinit, not as part of a longer one, and not an
// operand of autoload — that builtin only marks the function for a later call,
// so `autoload -Uz compinit` alone starts nothing (#582). A word zsh reads
// intact is enough even inside a quoted string: the parser answers what a line
// carries, with the loose edge the quoted shapes have always pinned.
func runsCompinit(code string) bool {
	for _, at := range wordOccurrences(code, "compinit") {
		if !autoloadOperand(code, at) {
			return true
		}
	}
	return false
}

// wordOccurrences returns where code carries word as a word of its own: a \r is
// an ordinary word character to zsh, never whitespace or a separator, so a
// CRLF file's compinit\r is the one-word command zsh fails to find — not
// compinit (#582) — and neither is \rcompinit.
func wordOccurrences(code, word string) []int {
	var at []int
	for from := 0; from < len(code); {
		i := strings.Index(code[from:], word)
		if i < 0 {
			break
		}
		i += from
		end := i + len(word)
		if (i == 0 || code[i-1] != '\r') && (end == len(code) || code[end] != '\r') {
			at = append(at, i)
		}
		from = i + 1
	}
	return at
}

// mentionsWord reports whether code carries word as a word of its own.
func mentionsWord(code, word string) bool { return len(wordOccurrences(code, word)) > 0 }

// autoloadOperand reports whether the word at position at is an operand of the
// autoload builtin in its command segment: the first word there is autoload
// (its flags sit between). `autoload -Uz compinit` loads a definition for a
// later call; the word it names does not run, so it is not the rc starting the
// completion system.
func autoloadOperand(code string, at int) bool {
	fields := strings.Fields(code[commandStart(code, at):at])
	return len(fields) > 0 && fields[0] == "autoload"
}

// commandStart returns where the command segment containing at begins: after
// the last unquoted separator before it. A quoted ; or | is part of a word,
// not a boundary, so a compinit a quoted string carries keeps the segment its
// first word gives it.
func commandStart(code string, at int) int {
	var quote byte
	start := 0
	for i := 0; i < at; i++ {
		c := code[i]
		switch {
		case c == '\\' && quote != '\'':
			i++ // the next byte is quoted, not syntax
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';', c == '&', c == '|', c == '(', c == ')':
			start = i + 1
		}
	}
	return start
}

// zshCode is line with its trailing comment cut: zsh starts one at an unquoted
// # that begins a word, so a # inside a word — `foo#compinit` — stays part of
// the code.
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
		case c == '#' && wordStart(line, i):
			return line[:i]
		}
	}
	return line
}

// wordStart reports whether position i begins a word for zsh's lexer: the start
// of the line, whitespace, or a command separator. A # there starts a comment
// (#582), the same rule zsh applies, so `setopt correct;# autoload -Uz compinit`
// runs nothing.
func wordStart(line string, i int) bool {
	if i == 0 {
		return true
	}
	switch line[i-1] {
	case ' ', '\t', ';', '&', '|', '(':
		return true
	}
	return false
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
