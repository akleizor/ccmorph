package util

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

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
// (#qN) qualifier, $(( )) arithmetic) and a case pattern. Text runs nothing,
// so a mention of compinit in it is answered as the scan before #582 answered
// it (#555): a line's text from an unquoted # at the line's start or after a
// blank is cut, the rest of the text counts. A here-document body is not rc
// commands at all, so it is cut, all but the $( ) and ` ` an unquoted one
// expands, which run (heredocBody). A case statement ends at esac or }, as
// zsh's does, whether it opened with in or {.
func zshCode(rc string) string {
	l := newLexer(rc)
	l.run()
	cut := l.cut()
	var code strings.Builder
	for i := 0; i < len(rc); i++ {
		if !cut[i] {
			code.WriteByte(rc[i])
		}
	}
	return code.String()
}

// frameKind names what the lexer is inside.
type frameKind uint8

const (
	codeFrame   frameKind = iota // commands: the file, a subshell, $( ), <( ), >( ), =( )
	arrayFrame                   // name=( ... ): words, blanks, newlines and comments
	caseFrame                    // case ... esac
	wordFrame                    // text: a word's own ( ), $(( )) or (( ))
	braceFrame                   // text: ${ ... }
	dquoteFrame                  // text: " ... "
	squoteFrame                  // text: ' ... '
	ansiFrame                    // text: $' ... ', where a backslash escapes the next byte
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
	braced     bool      // a case frame opened with { rather than in: } ends it
	groups     int       // { } groups open in a braced case's commands
	here       bool      // a dquote frame holding an unquoted here-document body: " does not close it
	depth      int       // ( or { open inside a word, brace or pattern frame
}

func newCode(sub bool) *lexFrame {
	return &lexFrame{kind: codeFrame, sub: sub, tokenStart: true, cmdPos: true, word: -1}
}

// heredoc is a here-document whose body begins after the next newline.
type heredoc struct {
	term   string // the line that ends the body, its quotes removed
	strip  bool   // <<- : leading tabs do not count against term
	quoted bool   // the delimiter is quoted, so the body is data zsh never expands
}

type lexer struct {
	src      string // the rc with a newline added, so its last word and comment end
	drop     []bool // bytes the code leaves out
	text     []bool // bytes zsh reads as text, not commands
	stack    []*lexFrame
	heredocs []heredoc
}

// newLexer starts a lexer on src, read as an rc of its own.
func newLexer(src string) *lexer {
	l := &lexer{src: src + "\n", drop: make([]bool, len(src)+1), text: make([]bool, len(src)+1)}
	l.stack = []*lexFrame{newCode(false)}
	return l
}

// run reads src to its end, marking the comments it drops and the bytes that
// are text. A ` ` met inside text is not text itself: backquote reads its body
// as commands.
func (l *lexer) run() {
	for i := 0; i < len(l.src); i++ {
		f := l.stack[len(l.stack)-1]
		inText := l.inText() && !(l.src[i] == '`' && f.kind != squoteFrame && f.kind != ansiFrame)
		j := l.step(i)
		for k := i; inText && k <= j; k++ {
			l.text[k] = true
		}
		i = j
	}
}

// cut returns, after run, every byte zshCode leaves out: the comments run
// dropped, and in each line's text the part the scan before #582 cut, from an
// unquoted # at the line's start or after a blank.
func (l *lexer) cut() []bool {
	rc := l.src[:len(l.src)-1]
	for start := 0; start < len(rc); {
		end := strings.IndexByte(l.src[start:], '\n') + start
		if k := textComment(rc[start:end]); k >= 0 {
			for i := start + k; i < end; i++ {
				l.drop[i] = l.drop[i] || l.text[i]
			}
		}
		start = end + 1
	}
	return l.drop
}

// parses reports whether, after run, src closed everything it opened that
// zsh needs closed: an unclosed quote, ${ }, substitution, subshell, array or
// case statement is a parse error. A word's own ( ) may stay open, as zsh
// ends that word where it ends any other and fails it later as a bad
// pattern; the lexer reads on past it as text instead, so a frame it opened
// after an open ( proves nothing, and src is taken to parse.
func (l *lexer) parses() bool {
	for _, f := range l.stack[1:] {
		if f.kind == wordFrame {
			return true
		}
	}
	return len(l.stack) == 1
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
// word's own ( ) or a case pattern.
func (l *lexer) inText() bool {
	f := l.stack[len(l.stack)-1]
	return f.kind >= wordFrame || f.kind == caseFrame && f.phase == casePattern
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
	if c == '{' && f.kind == caseFrame && f.phase == caseSubject && f.seen && f.tokenStart {
		f.phase, f.braced = casePatterns, true // case WORD { ... }
		return i
	}
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
		return l.backquote(i, false)
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
	case f.kind == caseFrame && f.braced && w == "}":
		if f.groups == 0 {
			l.pop()
			return true
		}
		f.groups--
	case f.kind == arrayFrame || !f.wordCmd:
	case f.kind == caseFrame && w == "{":
		f.groups++
		f.cmdPos = true
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
		return l.backquote(i, f.kind == dquoteFrame || f.dquoted)
	case f.kind == dquoteFrame:
		if c == '"' && !f.here {
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

// backquote reads the ` ` that opens at i and returns the index of its closing
// `. zsh finds that ` without reading quotes, a backslash keeping the byte
// after it, and then runs the body as commands once it has taken the backslash
// off \`, \\ and \$ (and \" inside double quotes) and joined each
// backslash-newline. So the body is read here as zsh reads it: unescaped the
// same way, then lexed as an rc of its own, its quotes ($'...' among them),
// substitutions and comments read as they read anywhere, and what that cuts
// from the body is cut from the rc. A body whose own quote, substitution,
// array or case statement does not close is one zsh cannot parse, so it runs
// nothing and all of it is cut.
func (l *lexer) backquote(i int, dquoted bool) int {
	var body strings.Builder
	var at []int // at[k] is where the source of the body's byte k begins in src
	j := i + 1
	for ; j < len(l.src) && l.src[j] != '`'; j++ {
		if l.src[j] == '\\' && j+1 < len(l.src) {
			switch c := l.src[j+1]; {
			case c == '\n':
				l.drop[j], l.drop[j+1] = true, true
				j++
				continue
			case c == '`', c == '\\', c == '$', c == '"' && dquoted:
				at = append(at, j)
				body.WriteByte(c)
				j++
				continue
			}
		}
		at = append(at, j)
		body.WriteByte(l.src[j])
	}
	inner := newLexer(body.String())
	inner.run()
	cut, closed := inner.cut(), inner.parses()
	at = append(at, j)
	for k := 0; k+1 < len(at); k++ {
		for s := at[k]; (cut[k] || !closed) && s < at[k+1]; s++ {
			l.drop[s] = true
		}
	}
	if j == len(l.src) {
		j-- // no closing `: the body runs to the end of the rc
	}
	return j
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
	if w := l.src[f.word:i]; f.depth == 0 && strings.IndexByte(" \t\n;&|)<>", c) >= 0 && (w == "esac" || w == "}") {
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
		return l.backquote(i, false)
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
// for the next newline. zsh ends that word at a blank or an operator and only
// takes its quotes off, never expanding it: \X is X, '...' is its text, "..."
// is its text as zsh's lexer reads a double-quoted string (dquoteWord), $'...'
// is decoded as zsh decodes it (ansiDecode), and any other $ is a literal
// byte, so $x and $"EOF" end at lines reading $x and $EOF. A word quoted
// anywhere makes the body data (heredocBody). zsh only matches the word
// against lines and never runs it, so its bytes are cut: <<compinit starts
// nothing. A quote in the word that never closes is a parse error, so zsh runs
// nothing from that command on, and the rc is cut from the << to its end
// (unclosed). A << with no word after it is left as it is.
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
	for j < len(l.src) && strings.IndexByte(" \t\n;&|<>()`", l.src[j]) < 0 {
		switch c := l.src[j]; {
		case c == '$' && l.src[j+1] == '\'':
			end := ansiEnd(l.src, j+2)
			if end < 0 {
				return l.unclosed(i)
			}
			term.WriteString(ansiDecode(l.src[j+2 : end]))
			quoted, j = true, end+1
		case c == '\\' && l.src[j+1] == '\n':
			j += 2 // a backslash-newline joins the lines
		case c == '\\':
			term.WriteByte(l.src[j+1])
			quoted, j = true, j+2
		case c == '\'':
			end := strings.IndexByte(l.src[j+1:], '\'')
			if end < 0 {
				return l.unclosed(i)
			}
			term.WriteString(l.src[j+1 : j+1+end])
			quoted, j = true, j+end+2
		case c == '"':
			text, end := dquoteWord(l.src, j+1, '"')
			if end < 0 {
				return l.unclosed(i)
			}
			term.WriteString(text)
			quoted, j = true, end+1
		default:
			term.WriteByte(c)
			j++
		}
	}
	if term.Len() == 0 && !quoted {
		return i // no delimiter word; <<'' is an empty one and does count
	}
	l.heredocs = append(l.heredocs, heredoc{term: term.String(), strip: strip, quoted: quoted})
	for k := i + 2; k < j; k++ {
		l.drop[k] = l.drop[k] || l.src[k] != '\n' // the word is matched, never run
	}
	f.word, f.tokenStart = -1, false
	return j - 1
}

// unclosed cuts src from i to its end, all but its newlines, and returns the
// index of its last byte, so the lexer reads no further: zsh cannot parse a
// command whose here-document delimiter opens a quote that never closes, so
// it runs nothing from there on.
func (l *lexer) unclosed(i int) int {
	for k := i; k < len(l.src); k++ {
		l.drop[k] = l.drop[k] || l.src[k] != '\n'
	}
	return len(l.src) - 1
}

// ansiEnd returns the index of the ' that closes the $'...' whose text begins
// at from, where a backslash escapes the byte after it, or -1 when none does.
func ansiEnd(src string, from int) int {
	for k := from; k < len(src); k++ {
		switch src[k] {
		case '\\':
			k++
		case '\'':
			return k
		}
	}
	return -1
}

// dquoteWord reads src from from as zsh's lexer reads a double-quoted string
// up to its closing byte end: " for "...", ) for $(( )) and ] for $[ ]. It
// returns the text zsh keeps once the quotes come off and the index of that
// closing byte, or -1 when src ends first. A backslash goes only from \$ \\ \`
// and the end byte, from \} inside ${ }, and from \[ \] \( \) \{ \} inside
// $[ ]; a backslash-newline goes whole; any other backslash stays. ${ } holds
// its own } and nested "..."; a ` ` holds " and $ as plain bytes; $(( )) and
// $[ ] are read the same way to their own end. A $( ) is copied as written,
// backslashes and all, through the ) that closes its commands (substEnd), as
// zsh keeps a command substitution's text raw: "$(print \$x)" ends at a line
// reading $(print \$x), while "\$x" ends at one reading $x.
func dquoteWord(src string, from int, end byte) (string, int) {
	var b strings.Builder
	math := end != '"'
	braces, parens, brackets, tick := 0, 0, 0, false
	for k := from; k < len(src); k++ {
		c := src[k]
		if c == end && braces == 0 && !tick && !(math && (parens > 0 || brackets > 0)) {
			return b.String(), k
		}
		switch {
		case c == '\\' && k+1 < len(src) && src[k+1] == '\n':
			k++ // a backslash-newline joins the lines
		case c == '\\' && k+1 < len(src) && dquoteEscape(src[k+1], end, braces > 0 && !tick):
			k++
			b.WriteByte(src[k])
		case c == '$' && !tick && k+1 < len(src) && src[k+1] == '(':
			if k+2 < len(src) && src[k+2] == '(' {
				text, stop := dquoteWord(src, k+3, ')')
				if stop >= 0 && stop+1 < len(src) && src[stop+1] == ')' {
					b.WriteString("$((" + text + "))")
					k = stop + 1
					continue
				}
			}
			stop := substEnd(src, k+2)
			if stop < 0 {
				return "", -1
			}
			// zsh drops a backslash-newline right after $( and keeps any other
			b.WriteString("$(" + strings.TrimPrefix(src[k+2:stop+1], "\\\n"))
			k = stop
		case c == '$' && !tick && k+1 < len(src) && src[k+1] == '[':
			text, stop := dquoteWord(src, k+2, ']')
			if stop < 0 {
				return "", -1
			}
			b.WriteString("$[" + text + "]")
			k = stop
		case c == '$' && !tick && k+1 < len(src) && (src[k+1] == '{' || src[k+1] == '$'):
			if src[k+1] == '{' {
				braces++
			}
			b.WriteString(src[k : k+2])
			k++
		case c == '}' && !tick && braces > 0:
			braces--
			b.WriteByte(c)
		case c == '`':
			tick = !tick
			b.WriteByte(c)
		case c == '"' && !tick && braces > 0:
			text, stop := dquoteWord(src, k+1, '"')
			if stop < 0 {
				return "", -1
			}
			b.WriteString(text)
			k = stop
		case math && !tick && c == '(':
			parens++
			b.WriteByte(c)
		case math && !tick && c == ')':
			parens--
			b.WriteByte(c)
		case math && !tick && c == '[':
			brackets++
			b.WriteByte(c)
		case math && !tick && c == ']':
			brackets--
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return "", -1
}

// dquoteEscape reports whether zsh takes the backslash off \n inside a
// double-quoted string that ends at end; inBrace is a ${ } outside any ` `.
func dquoteEscape(n, end byte, inBrace bool) bool {
	switch {
	case n == '$', n == '\\', n == '`', n == end, n == '}' && inBrace:
		return true
	}
	return end == ']' && strings.IndexByte("[](){}", n) >= 0
}

// substEnd returns the index in src of the ) that closes the $( ) whose
// commands begin at from, read as the lexer reads any commands, or -1 when
// src ends first.
func substEnd(src string, from int) int {
	l := newLexer(src[from:])
	l.push(newCode(true))
	for i := 0; i < len(l.src); i++ {
		i = l.step(i)
		if len(l.stack) == 1 {
			return from + i
		}
	}
	return -1
}

// ansiDecode returns the text of a $'...' decoded as zsh decodes it: \a \b
// \e \E \f \n \r \t \v, an octal \NNN (up to three digits), \xHH (up to two),
// \uHHHH and \UHHHHHHHH (written as UTF-8), \C-x for a control byte and \M-x
// for a meta byte (the - is optional; a \M- after a \C- applies first, and
// both wait past a \u or \U for the next byte), and any other \X is X. An escape with
// no digits after it (\x, \u) is a NUL byte, as it is to zsh, so a
// here-document delimiter spelled $'E\x46' or $'\u0045OF' ends where zsh ends
// it.
func ansiDecode(s string) string {
	var b []byte
	control, meta := false, 0 // meta: 1 applies after control, 2 before it
	for k := 0; k < len(s); k++ {
		c := s[k]
		if c == '\\' && k+1 < len(s) {
			k++
			switch e := s[k]; e {
			case 'a':
				c = '\a'
			case 'b':
				c = '\b'
			case 'e', 'E':
				c = 0x1b
			case 'f':
				c = '\f'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			case 't':
				c = '\t'
			case 'v':
				c = '\v'
			case 'C', 'M':
				if k+1 < len(s) && s[k+1] == '-' {
					k++
				}
				switch {
				case e == 'C':
					control = true
				case control:
					meta = 2
				default:
					meta = 1
				}
				continue
			case 'u', 'U':
				digits := 4
				if e == 'U' {
					digits = 8
				}
				var n rune
				for used := 0; used < digits && k+1 < len(s) && hexDigit(s[k+1]) >= 0; used++ {
					n = n*16 + rune(hexDigit(s[k+1]))
					k++
				}
				b = utf8.AppendRune(b, n)
				continue
			case 'x':
				n := 0
				for used := 0; used < 2 && k+1 < len(s) && hexDigit(s[k+1]) >= 0; used++ {
					n = n*16 + hexDigit(s[k+1])
					k++
				}
				c = byte(n)
			case '0', '1', '2', '3', '4', '5', '6', '7':
				n := int(e - '0')
				for used := 1; used < 3 && k+1 < len(s) && '0' <= s[k+1] && s[k+1] <= '7'; used++ {
					n = n*8 + int(s[k+1]-'0')
					k++
				}
				c = byte(n)
			default:
				c = e
			}
		}
		b = append(b, c)
		b, control, meta = controlMeta(b, control, meta)
	}
	return string(b)
}

// controlMeta applies a pending \C- and \M- to the byte just written, as zsh
// does: control keeps the low five bits (\C-? is DEL), meta sets the high bit,
// first when it was asked for after the control.
func controlMeta(b []byte, control bool, meta int) ([]byte, bool, int) {
	last := len(b) - 1
	if meta == 2 {
		b[last] |= 0x80
	}
	if control {
		if b[last] == '?' {
			b[last] = 0x7f
		} else {
			b[last] &= 0x9f
		}
	}
	if meta == 1 {
		b[last] |= 0x80
	}
	return b, false, 0
}

// hexDigit is the value of the hex digit c, or -1 when c is not one.
func hexDigit(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// newline reads the bodies of the here-documents queued on the line the
// newline at i ends, marks them text, and returns the index before the line
// after them.
func (l *lexer) newline(i int) int {
	j := i + 1
	for _, h := range l.heredocs {
		body, end := j, len(l.src) // the body ends at its delimiter line, or with the rc
		for j < len(l.src) {
			start := j
			stop := strings.IndexByte(l.src[j:], '\n') + j
			j = stop + 1
			line := l.src[start:stop]
			if h.strip {
				line = strings.TrimLeft(line, "\t")
			}
			if line == h.term {
				end = start
				for k := start; k < stop; k++ {
					l.drop[k] = true // the delimiter line
				}
				break
			}
		}
		l.heredocBody(body, end, h.quoted)
	}
	l.heredocs = nil
	return j - 1
}

// heredocBody cuts the here-document body src[from:to], which zsh never runs
// as commands: a quoted delimiter's body is data, and an unquoted one's is
// expanded as a double-quoted string is, so only the commands of a $( ) or
// ` ` inside it run. Those are lexed as they are anywhere else; the rest is
// cut, its newlines kept so the lines stay apart.
func (l *lexer) heredocBody(from, to int, quoted bool) {
	if quoted {
		for k := from; k < to; k++ {
			if l.src[k] != '\n' {
				l.drop[k] = true
			}
		}
		return
	}
	inner := newLexer(l.src[from:to])
	inner.push(&lexFrame{kind: dquoteFrame, here: true})
	inner.run()
	for k := from; k < to; k++ {
		if inner.drop[k-from] || inner.text[k-from] && l.src[k] != '\n' {
			l.drop[k] = true
		}
	}
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
