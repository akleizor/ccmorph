package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/util/paths"
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
//
// A \r is an ordinary word character to zsh (#582), so a CRLF rc's
// `compinit\r` is a command zsh fails to find, and `autoload -Uz compinit\r`
// marks a function named "compinit\r": a later intact compinit call finds no
// function then, unless an intact autoload of compinit came first. An autoload
// operand itself only marks the function for a later call, so it starts
// nothing.
func CompinitRuns(rc []byte) bool {
	autoloaded, misnamed := false, false
	for _, line := range strings.Split(string(rc), "\n") {
		code := zshCode(line)
		for _, at := range occurrences(code, "compinit") {
			intact := !crTouches(code, at, at+len("compinit"))
			switch {
			case autoloadOperand(code, at):
				autoloaded = autoloaded || intact
				misnamed = misnamed || !intact
			case intact && (autoloaded || !misnamed):
				return true
			}
		}
		for _, at := range occurrences(code, "oh-my-zsh.sh") {
			if !crTouches(code, at, at+len("oh-my-zsh.sh")) {
				return true
			}
		}
	}
	return false
}

// occurrences returns every position where code carries word. A word inside a
// quoted string counts: the parser answers what a line carries, with the loose
// edge the quoted shapes have always pinned (#555).
func occurrences(code, word string) []int {
	var at []int
	for from := 0; ; {
		i := strings.Index(code[from:], word)
		if i < 0 {
			return at
		}
		at = append(at, from+i)
		from += i + 1
	}
}

// crTouches reports whether a \r is part of the zsh word that carries
// code[start:end] (#582): it sits right before or after those bytes once the
// quote marks and backslashes between them are passed over, so `compinit\r`,
// `"compinit"\r`, `compinit""\r` and `\r"compinit"` all name a word zsh reads
// with a CR in it, while `compinit \r` or `compinit;\r` leaves compinit whole.
func crTouches(code string, start, end int) bool {
	i := start - 1
	for i >= 0 && quoteMark(code[i]) {
		i--
	}
	if i >= 0 && code[i] == '\r' {
		return true
	}
	j := end
	for j < len(code) && quoteMark(code[j]) {
		j++
	}
	return j < len(code) && code[j] == '\r'
}

// quoteMark reports whether c is a byte zsh removes from a word while keeping
// the word whole: a quote or a backslash.
func quoteMark(c byte) bool { return c == '"' || c == '\'' || c == '\\' }

// autoloadOperand reports whether the word at position at is an operand of the
// autoload builtin in its command segment: the first word there is autoload
// (its flags sit between). `autoload -Uz compinit` loads a definition for a
// later call; the word it names does not run, so it is not the rc starting the
// completion system.
func autoloadOperand(code string, at int) bool {
	fields := strings.Fields(code[commandStart(code, at):at])
	for len(fields) > 0 && (fields[0] == "builtin" || fields[0] == "noglob") {
		fields = fields[1:]
	}
	return len(fields) > 0 && fields[0] == "autoload"
}

// substitutionBefore reports whether the ( at i opens a command substitution:
// $(, <(, >(, or =( when the = began the word. Their contents are commands, so
// a # that begins a word in them starts a comment (#582). A ( inside a glob or
// pattern — (#i), (a|b), x=(y) — opens none, and a # there stays code.
func substitutionBefore(line string, i, word int) bool {
	switch line[i-1] {
	case '$', '<', '>':
		return true
	case '=':
		return word == i-1
	}
	return false
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

// zshCode is line with its comment cut where zsh's lexer cuts it (#582): at an
// unquoted, unescaped # that begins a token. A token begins at the start of the
// line, after a blank, after ; & | or a closing ), and after an opening ( that
// zsh reads as syntax: a subshell's ( in command position, or the ( of an
// array assignment (fpath=(, a+=(, local -a a=(). Nothing inside a word starts
// a token: an escaped separator (x\;#, x\ #) is part of the word, and so is
// everything between a word's own parentheses, so extendedglob's (#i), a glob
// qualifier's (#qN) and $(...) stay code wherever they sit.
func zshCode(line string) string {
	var quote byte
	tokenStart := true // the next byte begins a token
	cmdPos := true     // a word that begins there is in command position
	declaring := false // the command is typeset or kin, whose name=( opens an array
	depth := 0         // parentheses open inside the current word
	subs := []bool{}   // parens that close a command: true for a substitution
	word := -1         // where the current word began; -1 between words
	wordCmd := false   // the current word began in command position
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case c == '\\' && quote == '"':
				i++ // the next byte is quoted, not syntax
			case c == quote:
				quote = 0
			}
			continue
		}
		if depth == 0 {
			switch c {
			case ' ', '\t':
				if word >= 0 && wordCmd {
					w := line[word:i]
					switch {
					case leadsCommand(w) || assignment(w):
						cmdPos = true
					case declares(w):
						declaring = true
					}
				}
				word, tokenStart = -1, true
				continue
			case ';', '&', '|':
				word, tokenStart, cmdPos, declaring = -1, true, true, false
				continue
			case ')':
				if len(subs) > 0 {
					// A substitution returns to the word it was part of: a #
					// right after it is still that word's byte, not a comment.
					continuing := subs[len(subs)-1]
					subs = subs[:len(subs)-1]
					if continuing {
						word, tokenStart, cmdPos = -1, false, false
					} else {
						word, tokenStart, cmdPos = -1, true, true
					}
					continue
				}
				word, tokenStart, cmdPos = -1, true, true
				continue
			case '#':
				if tokenStart {
					return line[:i]
				}
			case '(':
				if i > 0 && substitutionBefore(line, i, word) { // $( ), <( ), >( ), =( )
					subs = append(subs, true)
					word, tokenStart, cmdPos, declaring = -1, true, true, false
					continue
				}
				if tokenStart && cmdPos { // a subshell
					subs = append(subs, false)
					word, tokenStart, cmdPos, declaring = -1, true, true, false
					continue
				}
				if word >= 0 && (wordCmd || declaring) && namePrefix(line[word:i]) {
					subs = append(subs, false)
					word, tokenStart, cmdPos = -1, true, false
					continue
				}
			}
		}
		if tokenStart {
			word, wordCmd, tokenStart, cmdPos = i, cmdPos, false, false
		}
		switch c {
		case '\\':
			i++ // the next byte is quoted, not syntax
		case '\'', '"':
			quote = c
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	return line
}

// leadsCommand reports whether word is a reserved word after which zsh reads
// the next word in command position, so `then (` opens a subshell.
func leadsCommand(word string) bool {
	switch word {
	case "!", "{", "do", "elif", "else", "if", "then", "time", "until", "while":
		return true
	}
	return false
}

// declares reports whether word is a builtin whose name=( operands zsh lexes
// as array assignments.
func declares(word string) bool {
	switch word {
	case "declare", "export", "float", "integer", "local", "readonly", "typeset":
		return true
	}
	return false
}

// assignment reports whether word is a parameter assignment (FOO=1), after
// which zsh still reads the next word in command position.
func assignment(word string) bool {
	eq := strings.IndexByte(word, '=')
	return eq >= 0 && namePrefix(word[:eq+1])
}

// namePrefix reports whether word is exactly a parameter name and its
// assignment operator: NAME=, NAME+= or NAME[subscript]=.
func namePrefix(word string) bool {
	name, ok := strings.CutSuffix(word, "=")
	if !ok {
		return false
	}
	name = strings.TrimSuffix(name, "+")
	if open := strings.IndexByte(name, '['); open >= 0 && strings.HasSuffix(name, "]") {
		name = name[:open]
	}
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9' {
			continue
		}
		return false
	}
	return true
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

// FpathBound reports whether a zsh with this rc and this fpath already
// searches dir for completions: the rc names dir or ccmorph's site-functions
// directory, or fpath (the caller's live FPATH) holds dir or another directory
// with a _ccmorph file. The one owner of the rule (#638): doctor asks it
// in-process and install.sh asks it through `util fpath-bound`, so the two
// verdicts cannot drift apart. An unreadable rc is empty, as doctor reads it,
// so a live fpath still counts.
func FpathBound(rc []byte, dir, fpath string) bool {
	text := string(rc)
	if strings.Contains(text, dir) || strings.Contains(text, "ccmorph/share/zsh/site-functions") {
		return true
	}
	for _, d := range filepath.SplitList(fpath) {
		if d == dir || paths.IsFile(filepath.Join(d, "_ccmorph")) {
			return true
		}
	}
	return false
}

// fpathBoundVerb is `util fpath-bound <rc> <dir>`, a verb install.sh runs for
// its completion hint: exit 0 when the rc or the inherited FPATH leaves dir on
// fpath, 1 when neither does — the status is the verdict, so no output has to
// be parsed. The live FPATH is the installer's own, so a .zshenv that exports
// it counts exactly as it does for doctor.
func fpathBoundVerb(args []string, _ io.Reader, _, _ io.Writer) int {
	if len(args) != 2 {
		return 2
	}
	rc, _ := os.ReadFile(args[0])
	if FpathBound(rc, args[1], os.Getenv("FPATH")) {
		return 0
	}
	return 1
}
