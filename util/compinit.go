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
	for _, code := range strings.Split(zshCode(string(rc)), "\n") {
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

// zshCode is rc with each comment cut out where zsh's lexer cuts one (#582)
// and each backslash-newline removed, as zsh joins the two lines. It reads the
// whole file, not one line at a time, because zsh does: a quote, a $( ), an
// array or a case statement opened on one line is still open on the next, so
// a closing ) there returns to the word it belongs to and a # after it is that
// word's byte, not a comment.
//
// Where zsh reads commands, a # is a comment only where a token begins: at a
// line's start, after a blank, after an unquoted ; & | or newline, after the )
// that ends a subshell or an array, and after a ( zsh reads as syntax (a
// subshell in command position, the ( of an array assignment such as fpath=(
// or local -a a=( ). A command substitution's own commands ($( ), <( ), >( ),
// =( ), ` `) take comments too, but its closing ) or ` leaves the word around
// it going on. Nothing inside a word starts a token: an escaped byte (x\;#,
// x\ #) is a word byte, and so is everything zsh reads as text, not commands:
// a quoted string (', ", $'...'), ${...}, a word's own parentheses ((#i), a
// (#qN) qualifier, $(( )) arithmetic), a case pattern and a here-document
// body. Text runs nothing, so a mention of compinit in it is answered as the
// scan before #582 answered it (#555): a line's text from an unquoted # at the
// line's start or after a blank is cut, the rest of the text counts.
func zshCode(rc string) string {
	l := lexer{src: rc + "\n", drop: make([]bool, len(rc)+1), text: make([]bool, len(rc)+1)}
	l.stack = []*lexFrame{newCode(false)}
	for i := 0; i < len(l.src); i++ {
		inText := l.inText()
		j := l.step(i)
		for k := i; inText && k <= j; k++ {
			l.text[k] = true
		}
		i = j
	}
	for start := 0; start < len(rc); {
		end := strings.IndexByte(l.src[start:], '\n') + start
		if k := textComment(rc[start:end]); k >= 0 {
			for i := start + k; i < end; i++ {
				l.drop[i] = l.drop[i] || l.text[i]
			}
		}
		start = end + 1
	}
	var code strings.Builder
	for i := 0; i < len(rc); i++ {
		if !l.drop[i] {
			code.WriteByte(rc[i])
		}
	}
	return code.String()
}

// frameKind names what the lexer is inside.
type frameKind uint8

const (
	codeFrame     frameKind = iota // commands: the file, a subshell, $( ), <( ), >( ), =( )
	arrayFrame                     // name=( ... ): words, blanks, newlines and comments
	caseFrame                      // case ... esac
	backtickFrame                  // ` ... `: commands zsh finds the end of as text, so both
	wordFrame                      // text: a word's own ( ), $(( )) or (( ))
	braceFrame                     // text: ${ ... }
	dquoteFrame                    // text: " ... "
	squoteFrame                    // text: ' ... '
	ansiFrame                      // text: $' ... ', where a backslash escapes the next byte
)

// casePhase is where a case frame is in its statement.
type casePhase uint8

const (
	caseSubject  casePhase = iota // case WORD in
	casePatterns                  // after in or ;; and before the next pattern
	casePattern                   // text: inside a pattern, up to its )
	caseBody                      // the commands after a pattern's )
)

// lexFrame is one open construct and, for code, array and case frames, where
// its current token stands.
type lexFrame struct {
	kind       frameKind
	sub        bool      // a code frame opened by $( <( >( =( : its ) leaves the word around it going on
	cmd        bool      // a word frame opened by (( where a token begins: its )) ends a token
	dquoted    bool      // a ${ or $(( frame inside double quotes, where ' is text
	tokenStart bool      // the next byte begins a token
	cmdPos     bool      // a word that begins there is in command position
	declaring  bool      // the command is typeset or kin, whose name=( opens an array
	word       int       // where the current word began; -1 between words
	wordCmd    bool      // the current word began in command position
	phase      casePhase // a case frame's place in its statement
	seen       bool      // a case frame has read its subject word
	lead       bool      // the case pattern began with (
	depth      int       // ( or { open inside a word, brace or pattern frame
}

func newCode(sub bool) *lexFrame {
	return &lexFrame{kind: codeFrame, sub: sub, tokenStart: true, cmdPos: true, word: -1}
}

// heredoc is a here-document whose body begins after the next newline.
type heredoc struct {
	term  string // the line that ends the body, its quotes removed
	strip bool   // <<- : leading tabs do not count against term
}

type lexer struct {
	src      string // the rc with a newline added, so its last word and comment end
	drop     []bool // bytes the code leaves out
	text     []bool // bytes zsh reads as text, not commands
	stack    []*lexFrame
	heredocs []heredoc
}

func (l *lexer) push(f *lexFrame) { l.stack = append(l.stack, f) }

// pop closes the innermost frame. A subshell, an array and (( )) end a token,
// so the next byte begins one; a substitution, quote or word group was part of
// the word around it, which goes on.
func (l *lexer) pop() {
	f := l.stack[len(l.stack)-1]
	l.stack = l.stack[:len(l.stack)-1]
	p := l.stack[len(l.stack)-1]
	switch {
	case f.kind == codeFrame && !f.sub, f.kind == arrayFrame, f.kind == wordFrame && f.cmd:
		p.word, p.tokenStart, p.cmdPos = -1, true, true
	case f.kind == caseFrame:
		p.word, p.tokenStart = -1, true
	}
}

// inText reports whether the next byte is text: inside a quote, ${ }, a
// word's own ( ), a case pattern or ` `.
func (l *lexer) inText() bool {
	f := l.stack[len(l.stack)-1]
	return f.kind >= backtickFrame || f.kind == caseFrame && f.phase == casePattern
}

// step reads the byte at i and returns the index of the last byte it used,
// or i-1 when the frame it closed leaves the byte to its parent.
func (l *lexer) step(i int) int {
	f := l.stack[len(l.stack)-1]
	c := l.src[i]
	if c == '\\' && f.kind != squoteFrame && f.kind != ansiFrame && l.src[i+1] == '\n' {
		l.drop[i], l.drop[i+1] = true, true
		return i + 1
	}
	switch f.kind {
	case squoteFrame:
		if c == '\'' {
			l.pop()
		}
		return i
	case ansiFrame:
		if c == '\\' {
			return i + 1
		}
		if c == '\'' {
			l.pop()
		}
		return i
	case backtickFrame:
		return l.backtick(f, i)
	case dquoteFrame, braceFrame, wordFrame:
		return l.inWord(f, i)
	case caseFrame:
		switch f.phase {
		case casePatterns:
			return l.patterns(f, i)
		case casePattern:
			return l.pattern(f, i)
		}
	}
	return l.code(f, i)
}

// code reads a byte where commands or array words are: a code frame, an
// array, or a case frame's subject and bodies.
func (l *lexer) code(f *lexFrame, i int) int {
	c := l.src[i]
	switch c {
	case ' ', '\t', '\n', ';', '&', '|', ')':
		if l.endWord(f, i) {
			return i - 1
		}
	}
	switch c {
	case ' ', '\t':
		f.word, f.tokenStart = -1, true
	case '\n':
		f.word, f.tokenStart = -1, true
		if f.kind != arrayFrame {
			f.cmdPos, f.declaring = true, false
		}
		return l.newline(i)
	case ';', '&', '|':
		if f.kind == caseFrame && c == ';' && strings.IndexByte(";&|", l.src[i+1]) >= 0 {
			f.phase = casePatterns // ;; ;& ;| end a case item
			i++
		}
		f.word, f.tokenStart, f.cmdPos, f.declaring = -1, true, true, false
	case ')':
		if len(l.stack) > 1 && f.kind != caseFrame {
			l.pop()
			return i
		}
		f.word, f.tokenStart, f.cmdPos = -1, true, true
	case '#':
		if f.tokenStart {
			return l.comment(i, strings.IndexByte(l.src[i:], '\n')+i)
		}
		l.wordByte(f, i)
	case '(':
		return l.open(f, i)
	case '<', '>':
		l.wordByte(f, i)
		switch {
		case l.src[i+1] == '(':
			l.push(newCode(true)) // <( ) or >( )
			return i + 1
		case strings.HasPrefix(l.src[i:], "<<<"):
			return i + 2 // a here-string, not a here-document
		case strings.HasPrefix(l.src[i:], "<<"):
			return l.heredoc(f, i)
		}
	case '=':
		atStart := f.tokenStart || f.word == i
		l.wordByte(f, i)
		if atStart && l.src[i+1] == '(' {
			l.push(newCode(true)) // =( )
			return i + 1
		}
	case '$':
		l.wordByte(f, i)
		return l.dollar(i, false)
	case '\\':
		l.wordByte(f, i)
		return i + 1
	case '\'':
		l.wordByte(f, i)
		l.push(&lexFrame{kind: squoteFrame})
	case '"':
		l.wordByte(f, i)
		l.push(&lexFrame{kind: dquoteFrame})
	case '`':
		l.wordByte(f, i)
		l.push(&lexFrame{kind: backtickFrame, tokenStart: true})
	default:
		l.wordByte(f, i)
	}
	return i
}

// wordByte notes that the byte at i belongs to a word, which begins there when
// a token does.
func (l *lexer) wordByte(f *lexFrame, i int) {
	if f.tokenStart {
		f.word, f.wordCmd, f.tokenStart, f.cmdPos = i, f.cmdPos, false, false
	}
}

// comment drops src[i:end], a comment, and returns the index before end, so
// the byte that ended it is read next.
func (l *lexer) comment(i, end int) int {
	for k := i; k < end; k++ {
		l.drop[k] = true
	}
	return end - 1
}

// endWord acts on the word that ends at i, if one does, and reports whether
// that closed the frame (a case statement's esac).
func (l *lexer) endWord(f *lexFrame, i int) bool {
	if f.word < 0 {
		return false
	}
	w := l.src[f.word:i]
	switch {
	case f.kind == caseFrame && f.phase == caseSubject:
		if f.seen && w == "in" {
			f.phase = casePatterns
		}
		f.seen = true
	case f.kind == arrayFrame || !f.wordCmd:
	case f.kind == caseFrame && w == "esac":
		l.pop()
		return true
	case w == "case":
		l.push(&lexFrame{kind: caseFrame, phase: caseSubject, tokenStart: true, word: -1})
	case leadsCommand(w) || assignment(w) || funcDef(w):
		f.cmdPos = true
	case declares(w):
		f.declaring = true
	}
	return false
}

// funcDef reports whether word is NAME(), a function definition whose body
// zsh reads in command position.
func funcDef(word string) bool {
	name, ok := strings.CutSuffix(word, "()")
	return ok && name != "" && !strings.ContainsAny(name, "<>$=(){}[]'\"`\\|&;*?#")
}

// open reads a ( in a code-like frame: (( )) arithmetic where a token begins,
// a subshell in command position, an array after NAME=, and otherwise a group
// inside a word.
func (l *lexer) open(f *lexFrame, i int) int {
	switch {
	case f.tokenStart && f.kind != arrayFrame && l.src[i+1] == '(':
		l.push(&lexFrame{kind: wordFrame, cmd: true, depth: 1})
		return i + 1
	case f.tokenStart && f.cmdPos && f.kind != arrayFrame:
		l.push(newCode(false))
		return i
	case f.word >= 0 && f.kind != arrayFrame && (f.wordCmd || f.declaring) && namePrefix(l.src[f.word:i]):
		l.push(&lexFrame{kind: arrayFrame, tokenStart: true, word: -1})
		return i
	}
	l.wordByte(f, i)
	l.push(&lexFrame{kind: wordFrame})
	return i
}

// dollar reads what follows the $ at i: $( ), $(( )), ${ }, and outside
// double quotes $'...' and $"...".
func (l *lexer) dollar(i int, dquoted bool) int {
	switch l.src[i+1] {
	case '(':
		if l.src[i+2] == '(' {
			l.push(&lexFrame{kind: wordFrame, depth: 1, dquoted: dquoted})
			return i + 2
		}
		l.push(newCode(true))
		return i + 1
	case '{':
		l.push(&lexFrame{kind: braceFrame, dquoted: dquoted})
		return i + 1
	case '\'':
		if !dquoted {
			l.push(&lexFrame{kind: ansiFrame})
			return i + 1
		}
	case '"':
		if !dquoted {
			l.push(&lexFrame{kind: dquoteFrame})
			return i + 1
		}
	}
	return i
}

// inWord reads a byte inside double quotes, ${ } or a word's own ( ).
func (l *lexer) inWord(f *lexFrame, i int) int {
	c := l.src[i]
	switch {
	case c == '\\':
		return i + 1
	case c == '$':
		return l.dollar(i, f.kind == dquoteFrame || f.dquoted)
	case c == '`':
		l.push(&lexFrame{kind: backtickFrame, tokenStart: true})
	case f.kind == dquoteFrame:
		if c == '"' {
			l.pop()
		}
	case c == '"':
		l.push(&lexFrame{kind: dquoteFrame})
	case c == '\'' && !f.dquoted:
		l.push(&lexFrame{kind: squoteFrame})
	case f.kind == braceFrame && c == '{', f.kind == wordFrame && c == '(':
		f.depth++
	case f.kind == braceFrame && c == '}', f.kind == wordFrame && c == ')':
		if f.depth == 0 {
			l.pop()
		} else {
			f.depth--
		}
	}
	return i
}

// backtick reads a byte inside ` `. zsh finds the closing ` without reading
// quotes, then runs what is inside as commands, so a # that begins a token
// there is a comment up to the line's end or the closing `. Its bytes are text
// as well, read as the old line reading read them, since zsh found their end
// as text.
func (l *lexer) backtick(f *lexFrame, i int) int {
	c := l.src[i]
	switch {
	case c == '`':
		l.pop()
	case f.depth != 0:
		// Inside a quote of the body: a # is a quoted byte. The closing `
		// above still ends the frame, as zsh finds it without reading quotes.
		switch {
		case c == '\\' && f.depth == '"':
			return i + 1
		case int(c) == f.depth:
			f.depth = 0
		}
	case c == '\'' || c == '"':
		f.depth, f.tokenStart = int(c), false
	case c == '#' && f.tokenStart:
		j := i
		for j < len(l.src) && l.src[j] != '\n' && l.src[j] != '`' {
			if l.src[j] == '\\' {
				j++
			}
			j++
		}
		return l.comment(i, j)
	case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '&' || c == '|':
		f.tokenStart = true
	case c == '\\':
		f.tokenStart = false
		return i + 1
	default:
		f.tokenStart = false
	}
	return i
}

// patterns reads a byte between a case's in (or ;;) and its next pattern.
func (l *lexer) patterns(f *lexFrame, i int) int {
	switch l.src[i] {
	case ' ', '\t', '\n':
		return i
	case '#':
		return l.comment(i, strings.IndexByte(l.src[i:], '\n')+i)
	}
	f.phase, f.word, f.lead, f.depth = casePattern, i, l.src[i] == '(', 0
	return i - 1 // read this byte as the pattern's first
}

// pattern reads a byte of a case pattern. Its blanks and | are pattern bytes;
// the ) that closes it starts the item's commands. A pattern that begins with (
// may end at that group's ), as `(a|b) cmd` does, or go on, as `(#i)x)` does.
// esac where a pattern would begin ends the statement.
func (l *lexer) pattern(f *lexFrame, i int) int {
	c := l.src[i]
	if f.depth == 0 && strings.IndexByte(" \t\n;&|)", c) >= 0 && l.src[f.word:i] == "esac" {
		l.pop()
		return i - 1
	}
	switch c {
	case '\\':
		return i + 1
	case '$':
		return l.dollar(i, false)
	case '\'':
		l.push(&lexFrame{kind: squoteFrame})
	case '"':
		l.push(&lexFrame{kind: dquoteFrame})
	case '`':
		l.push(&lexFrame{kind: backtickFrame, tokenStart: true})
	case '(':
		f.depth++
	case ')':
		if f.depth > 0 {
			f.depth--
			if f.depth > 0 || !f.lead || strings.IndexByte(" \t\n;", l.src[i+1]) < 0 {
				f.lead = f.lead && f.depth > 0
				return i
			}
		}
		f.phase, f.word, f.tokenStart, f.cmdPos = caseBody, -1, true, true
	}
	return i
}

// heredoc reads the << or <<- at i and its delimiter word, and queues the body
// for the next newline. The word ends at a blank, an operator or a byte that
// opens a substitution or group, which the lexer then reads as usual. A <<
// with no word after it is left as it is.
func (l *lexer) heredoc(f *lexFrame, i int) int {
	j := i + 2
	strip := l.src[j] == '-'
	if strip {
		j++
	}
	for l.src[j] == ' ' || l.src[j] == '\t' {
		j++
	}
	var term strings.Builder
	quoted := false
	for j < len(l.src) && strings.IndexByte(" \t\n;&|<>()`$", l.src[j]) < 0 {
		switch c := l.src[j]; c {
		case '\\':
			term.WriteByte(l.src[j+1])
			j += 2
		case '\'', '"':
			end := strings.IndexByte(l.src[j+1:], c)
			if end < 0 {
				return i
			}
			term.WriteString(l.src[j+1 : j+1+end])
			quoted = true
			j += end + 2
		default:
			term.WriteByte(c)
			j++
		}
	}
	if term.Len() == 0 && !quoted {
		return i // no delimiter word; <<'' is an empty one and does count
	}
	l.heredocs = append(l.heredocs, heredoc{term: term.String(), strip: strip})
	f.word, f.tokenStart = -1, false
	return j - 1
}

// newline reads the bodies of the here-documents queued on the line the
// newline at i ends, marks them text, and returns the index before the line
// after them.
func (l *lexer) newline(i int) int {
	j := i + 1
	for _, h := range l.heredocs {
		for j < len(l.src) {
			start := j
			end := strings.IndexByte(l.src[j:], '\n') + j
			for k := start; k <= end; k++ {
				l.text[k] = true
			}
			j = end + 1
			line := l.src[start:end]
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.term {
				break
			}
		}
	}
	l.heredocs = nil
	return j - 1
}

// textComment is where the scan before #582 cut a line: at an unquoted # at
// the line's start or after a blank, reading quotes on that line alone. It is
// -1 when the line has none.
func textComment(line string) int {
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\\' && quote != '\'':
			i++
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return i
		}
	}
	return -1
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
