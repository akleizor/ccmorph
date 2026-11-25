package util

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// #449: the static read must follow what a startup file runs, not what it
// mentions. A commented-out compinit starts nothing; naming the oh-my-zsh
// directory does not load Oh My Zsh; only an uncommented compinit or the
// loader that runs it (oh-my-zsh.sh) counts. #555: this parser is the one
// owner of the rule — doctor asks it in-process and install.sh asks it through
// `util compinit-runs` — so the shapes the installer's retired zsh spelling
// got wrong are pinned here as the owner's verdicts: a last line without a
// trailing newline still runs, and a # inside quotes is not a comment, so the
// compinit a quoted string carries still counts.
func TestCompinitDetectionReadsOnlyLinesThatRun(t *testing.T) {
	for _, c := range []struct {
		rc   string
		want bool
	}{
		{"autoload -Uz compinit && compinit\n", true},
		{"autoload -Uz compinit && compinit -u\n", true},
		{"source $ZSH/oh-my-zsh.sh\n", true},
		{`. "$ZSH/oh-my-zsh.sh"` + "\n", true},
		{"# autoload -Uz compinit && compinit\n", false},
		{"  # source $ZSH/oh-my-zsh.sh\n", false},
		{`export ZSH="$HOME/.oh-my-zsh"` + "\n", false},
		{"fpath=(/x $fpath) # compinit comes later\n", false},
		{"", false},
		{"autoload -Uz compinit && compinit", true},
		{"source $ZSH/oh-my-zsh.sh", true},
		{`print -r -- "run compinit yourself"` + "\n", true},
		{`print -r -- "run # compinit yourself"` + "\n", true},
		// #582: zsh starts a comment at a # that begins a word, so the one
		// right after a command separator starts nothing. A quoted # is not a
		// comment and stays code.
		{"setopt correct;# autoload -Uz compinit && compinit\n", false},
		{"true&# autoload -Uz compinit && compinit\n", false},
		{"true|# autoload -Uz compinit && compinit\n", false},
		{"fpath=(# autoload -Uz compinit && compinit\n", false},
		{"print -r -- 'x;# compinit'\n", true},
		// A quoted separator is not a command boundary: the quoted autoload
		// does not make the compinit beside it an autoload operand.
		{"print -r -- 'x; autoload compinit'\n", true},
		// #582: \r is an ordinary word character to zsh, never whitespace or a
		// separator, so a CRLF rc's compinit\r is the command zsh names
		// "compinit\r" and fails to find — not compinit.
		{"autoload -Uz compinit && compinit\r\n", false},
		{"# a comment\r\nautoload -Uz compinit && compinit\r\n", false},
		{"autoload -Uz compinit\r\n", false},
		// autoload only marks compinit for a later call; the word it names
		// starts nothing, so an rc that stops there has not started completion.
		{"autoload -Uz compinit\n", false},
		{"autoload -Uz compinit; compinit\n", true},
		{"source $ZSH/oh-my-zsh.sh\r\n", false},
		{"print \rcompinit\n", false},
		{"autoload -Uz compinit && compinit\r # start completions\n", false},
		// The CR touches only what follows compinit: the command name is
		// intact, so zsh runs it.
		{"autoload -Uz compinit && compinit # start completions\r\n", true},
		{"autoload -Uz compinit && compinit -u\r\n", true},
		{"autoload -Uz compinit && compinit;\r\n", true},
		{"autoload -Uz compinit && compinit \r\n", true},
		{"source $ZSH/oh-my-zsh.sh # load\r\n", true},
		// #582 review: an escaped separator or blank is part of a word, so the
		// # after it begins no token and the line runs on.
		{"print -r -- x\\;# ; autoload -Uz compinit && compinit\n", true},
		{"print -r -- x\\ # ; autoload -Uz compinit && compinit\n", true},
		// A # inside a word's own parentheses is extendedglob syntax, never a
		// comment: (#i), a (#q...) glob qualifier, the once-a-day zcompdump idiom.
		{"setopt extendedglob; [[ x = (#i)X ]] && autoload -Uz compinit && compinit\n", true},
		{"[[ -n ${ZDOTDIR:-$HOME}/.zcompdump(#qN.mh+24) ]] && compinit -C || compinit\n", true},
		{"autoload -Uz compinit\nif [[ -n ${ZDOTDIR:-$HOME}/.zcompdump(#qN.mh+24) ]]; then\n  compinit\nelse\n  compinit -C\nfi\n", true},
		// The ( zsh reads as syntax (a subshell, an array assignment) and a
		// closing ) do begin a token, so a # there is a comment.
		{"(true)# autoload -Uz compinit && compinit\n", false},
		{"local -a a=(# autoload -Uz compinit && compinit\n", false},
		{"x=$(print a) # autoload -Uz compinit && compinit\n", false},
		// A CR beyond a quote mark is still in the word: "compinit"\r is the
		// command zsh names compinit^M.
		{"autoload -Uz compinit && \"compinit\"\r\n", false},
		{". \"$ZSH/oh-my-zsh.sh\"\r\n", false},
		{"autoload -Uz compinit && \"compinit\" # x\r\n", true},
		// autoload -Uz compinit\r marks a function named compinit^M, so the
		// intact call on the next line finds none, unless compinit itself was
		// autoloaded first.
		{"autoload -Uz compinit\r\ncompinit -u\r\n", false},
		{"autoload -Uz compinit\r\nautoload -Uz compinit\ncompinit -u\r\n", true},
		{"compinit\n", true},
		{"source <(fzf --zsh); autoload -Uz compinit && compinit\n", true},
		// #582 review: a command substitution holds commands, so a # that
		// begins a word in $( ), <( ) or ` ` starts a comment; a # right after
		// the closing ) is still the surrounding word's byte.
		{"value=$(print -r -- hello # compinit\n)\n", false},
		{"cat <(print a # compinit\n)\nautoload -Uz compinit && compinit\n", true},
		{"x=`print a # compinit`\n", false},
		{"print -r -- $(print a)#b; autoload -Uz compinit && compinit\n", true},
		// An array assignment's words take comments too, while (#i) inside one
		// stays a pattern; a # after its closing ) begins a token.
		{"fpath=(x # autoload -Uz compinit && compinit\n)\n", false},
		{"fpath=(/x $fpath)# autoload -Uz compinit && compinit\n", false},
		// builtin and noglob autoload define compinit as plain autoload does,
		// so a malformed autoload beside them does not hide the intact one.
		{"builtin autoload -Uz compinit\ncompinit -u\n", true},
		{"autoload -Uz compinit\r\nbuiltin autoload -Uz compinit # intact autoload\ncompinit -u\n", true},
		{"autoload -Uz compinit\r\nnoglob autoload -Uz compinit\ncompinit -u\n", true},
		// #582 review: the scan reads the whole file as zsh does, not one line
		// at a time, so a substitution, quote or case opened on one line is
		// still open on the next, and a # right after its closing ) or quote is
		// a word byte.
		{"x=$(print a\n)#; autoload -Uz compinit && compinit\n", true},
		{"x=`print a\n`#; autoload -Uz compinit && compinit\n", true},
		{"x=$(case a in a) print hi;; esac)#; autoload -Uz compinit && compinit\n", true},
		{"print 'a\nb;# x'; autoload -Uz compinit && compinit\n", true},
		{"print \"a\nb\";# autoload -Uz compinit && compinit\n", false},
		{"print -r -- $'it\\'s;# x'; autoload -Uz compinit && compinit\n", true},
		{"print -r -- ${x:-a;#b}; autoload -Uz compinit && compinit\n", true},
		{"print -r -- x\\\n#; autoload -Uz compinit && compinit\n", true},
		{"autoload -Uz \\\n  compinit\n", false},
		// Text zsh does not run (a string, a word's own ( ), a here-document
		// body) keeps the old line reading: a # at a text line's start or after
		// a blank still cuts it, and the code after the text reads on.
		{"print -r -- 'a\n# autoload -Uz compinit && compinit\nb'\n", false},
		{"print -r -- x(a # compinit)(N)\n", false},
		{"print -r -- x(a # b)(N); autoload -Uz compinit && compinit\n", true},
		// #582 review: a quote inside a backquote body hides its #, and an empty
		// quoted here-document delimiter (<<'') still opens a body.
		{"x=`print -r -- 'x;# y'; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print -r -- \"a # b\"; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print a # autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{": <<''\ndon't\n\n:;# autoload -Uz compinit && compinit\n", false},
		{": <<\"\"\ndon't\n\nautoload -Uz compinit && compinit\n", true},
		{"cat <<EOF >/dev/null\ndon't\nEOF\n# autoload -Uz compinit && compinit\n", false},
		{"cat <<EOF >/dev/null\ndon't\nEOF\nautoload -Uz compinit && compinit\n", true},
		// #582 review: zsh finds a backquote's end without reading quotes, takes
		// the backslash off \` \\ \$ (and \" inside double quotes), then runs the
		// body as commands, so the body is lexed as an rc of its own: $'it\'s'
		// closes after the escaped quote and the ;# after it is a comment.
		{"x=`print -r -- $'it\\'s';# autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{"x=\"`print -r -- $'it\\'s';# autoload -Uz compinit && compinit`\"; print -r -- $x\n", false},
		{"x=`print -r -- \\$'it\\\\'s';# autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{"x=`print -r -- 'a\\`b';# autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{"x=`print -r -- \\\"a;# b\\\"; autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{"x=\"`print -r -- \\\"a;# b\\\"; autoload -Uz compinit && compinit`\"; print -r -- $x\n", true},
		{"x=`print -r -- a\\\\;# b; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print -r -- \\`print a;# b\\`; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print -r -- a\\ #;autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print -r -- a\\\n#; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`print -r -- ${x:-a;#b}; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=${y:-`print -r -- a\\ #;autoload -Uz compinit && compinit`}; print -r -- $x\n", true},
		{"x=\"`print -r -- a\\\\\\ #;autoload -Uz compinit && compinit`\"; print -r -- $x\n", false},
		// A body zsh cannot parse runs nothing: once \\ is unescaped, $'a\' never
		// closes. A word's own ( ) left open is not a parse error.
		{"x=`print -r -- $'a\\\\';# autoload -Uz compinit && compinit`; print -r -- $x\n", false},
		{"x=`print -r -- $'a\\\\\\\\'; autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		{"x=`<<<'x'(;autoload -Uz compinit && compinit`; print -r -- $x\n", true},
		// #582 review: a body that only looked unclosed to the lexer is not cut.
		// case WORD { ... } closes at }, and esac or } ends either case form.
		{"x=`case x { x) autoload -Uz compinit && compinit ;; }`; print -r -- \"$x\"\n", true},
		{"x=`case x { x) { print -r -- a; } }; autoload -Uz compinit && compinit`; print -r -- \"$x\"\n", true},
		{"case x { y) : ;; };# autoload -Uz compinit && compinit\n", false},
		{"case a in }; autoload -Uz compinit && compinit\n", true},
		// A here-document delimiter is quote-removed, never expanded, and its
		// body runs nothing but the $( ) and ` ` an unquoted one expands.
		{"x=`cat <<$'EOF'\ndon't\nEOF\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n", true},
		{"x=`cat <<$x\ndon't\n$x\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n", true},
		{"cat <<$\"EOF\" >/dev/null\ndon't\nEOF\nautoload -Uz compinit && compinit\n", false},
		{"cat <<$'E\\x46' >/dev/null\nit's\nEF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<'EOF' >/dev/null\nautoload -Uz compinit && compinit\nEOF\n", false},
		{"cat <<EOF >/dev/null\nautoload -Uz compinit && compinit\nEOF\n", false},
		{"cat <<EOF\n$(autoload -Uz compinit && compinit)\nEOF\n", true},
		{"cat <<\\EOF\n$(autoload -Uz compinit && compinit)\nEOF\n", false},
		// #582 review: a here-document delimiter is quote-removed as zsh removes
		// quotes: $'...' decodes \u, \U, \C-, \M- and drops the backslash of an
		// escape it does not know; "..." keeps a backslash only before what it
		// does not quote. The body ends where zsh ends it, so what follows runs.
		{"cat <<$'\\u0045OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\U00000045OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\u0045OF' >/dev/null\nautoload -Uz compinit && compinit\nEOF\n", false},
		{"x=`cat <<$'\\\\u0045OF'\nhello\nEOF\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n", true},
		{"cat <<$'E\\qF' >/dev/null\nhello\nEqF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\C-aEOF' >/dev/null\nhello\n\x01EOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\M-aEOF' >/dev/null\nhello\n\xe1EOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\x45OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<$'\\105OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"E\\$F\" >/dev/null\nhello\nE$F\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"E\\\"F\" >/dev/null\nhello\nE\"F\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"E\\qF\" >/dev/null\nhello\nE\\qF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<E\\OF >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<E\\OF >/dev/null\nautoload -Uz compinit && compinit\nEOF\n", false},
		// #582 review: a "..." delimiter is read as zsh's lexer reads a quoted
		// string. A $( ) inside keeps its text raw, backslashes and all, and its own
		// quotes do not close the word; only a backslash-newline right after $( goes.
		{"cat <<\"$(print \\$foo)\" >/dev/null\nhello\n$(print \\$foo)\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$(print \\$foo)\" >/dev/null\nhello\n$(print $foo)\nautoload -Uz compinit && compinit\n$(print \\$foo)\n", false},
		{"cat <<\"$(print \")\")\" >/dev/null\nhello\n$(print \")\")\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$(print '\"')\" >/dev/null\nhello\n$(print '\"')\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$(\\\nprint a)\" >/dev/null\nhello\n$(print a)\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$(print a\\\nb)\" >/dev/null\nhello\n$(print ab)\nautoload -Uz compinit && compinit\n", false},
		{"cat <<\"$((print a); (print b))\" >/dev/null\nhello\n$((print a); (print b))\nautoload -Uz compinit && compinit\n", true},
		// ${ } takes the backslash off \} and holds nested quotes; a ` ` holds "
		// as a plain byte; $(( )) and $[ ] unescape as a quoted string does.
		{"cat <<\"${x:-\\}}\" >/dev/null\nhello\n${x:-}}\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"${x:-\\}}\" >/dev/null\nhello\n${x:-\\}}\nautoload -Uz compinit && compinit\n", false},
		{"cat <<\"${x:-\"a\"}\" >/dev/null\nhello\n${x:-a}\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"`print \"a\"`\" >/dev/null\nhello\n`print \"a\"`\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"`print \\$a`\" >/dev/null\nhello\n`print $a`\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"`print \\$a`\" >/dev/null\nhello\n`print \\$a`\nautoload -Uz compinit && compinit\n", false},
		{"cat <<\"$((1+\\$x))\" >/dev/null\nhello\n$((1+$x))\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$((1+\\$x))\" >/dev/null\nhello\n$((1+\\$x))\nautoload -Uz compinit && compinit\n", false},
		{"cat <<\"$(( (1) ))\" >/dev/null\nhello\n$(( (1) ))\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"$[1+\\$x]\" >/dev/null\nhello\n$[1+$x]\nautoload -Uz compinit && compinit\n", true},
		// zsh only matches the delimiter against lines and never runs it.
		{"cat <<compinit >/dev/null\nhello\ncompinit\n", false},
		{"cat <<\"E\nautoload -Uz compinit && compinit\nF\" >/dev/null\n", false},
		// A quote in the delimiter that never closes is a parse error: nothing
		// from that command on runs.
		{"cat <<\"${x\" >/dev/null\nautoload -Uz compinit && compinit\n${x\n", false},
		{"cat <<\"`print a\" >/dev/null\nautoload -Uz compinit && compinit\n", false},
		{"cat <<'abc >/dev/null\nautoload -Uz compinit && compinit\n", false},
		{"cat <<$'abc >/dev/null\nautoload -Uz compinit && compinit\n", false},
		{"cat <<\"abc >/dev/null\nautoload -Uz compinit && compinit\n", false},
		// Ordinary quoted delimiters keep their verdicts.
		{"cat <<\"EOF\" >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n", true},
		{"cat <<\"EOF\" >/dev/null\nautoload -Uz compinit && compinit\nEOF\n", false},
		{"autoload -Uz compinit && compinit\ncat <<\"EOF\" >/dev/null\nhello\nEOF\n", true},
	} {
		if got := CompinitRuns([]byte(c.rc)); got != c.want {
			t.Errorf("CompinitRuns(%q) = %v, want %v", c.rc, got, c.want)
		}
	}
}

// TestCompinitDetectionMatchesZshOnWordStartsAndCRs records the verdicts above
// from zsh itself (#582): the parser is the one owner of the rule, so a second
// reading of these shapes — the installer's old zsh spelling — is not what
// pins them. A compinit function and an Oh My Zsh loader on a temp fpath print
// a marker when the rc reaches them, so "ran" is what zsh did, not what the
// file mentions. A quoted string that only mentions compinit (print -r --
// "run compinit yourself") is answered by mention, so it is pinned in the
// table above, not here; a quoted # on a line that also runs compinit
// executes, so zsh decides it here. Every filename glob below carries (N) or
// (#qN): zsh stops a sourced file at "no matches found", which would read as
// compinit not running.
func TestCompinitDetectionMatchesZshOnWordStartsAndCRs(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not on PATH: no shell to record the verdict from")
	}
	dir := t.TempDir()
	fnDir := filepath.Join(dir, "functions")
	omzDir := filepath.Join(dir, "oh-my-zsh")
	for _, d := range []string{fnDir, omzDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	const marker = "__ccmorph_compinit_ran__"
	for _, f := range []string{filepath.Join(fnDir, "compinit"), filepath.Join(omzDir, "oh-my-zsh.sh")} {
		if err := os.WriteFile(f, []byte("print -r -- "+marker+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rcs := []string{
		"setopt correct;# autoload -Uz compinit && compinit\n",
		"true&# autoload -Uz compinit && compinit\n",
		"true|# autoload -Uz compinit && compinit\n",
		"fpath=(# autoload -Uz compinit && compinit\n",
		"autoload -Uz compinit && compinit\r\n",
		"# a comment\r\nautoload -Uz compinit && compinit\r\n",
		"autoload -Uz compinit\r\n",
		"autoload -Uz compinit\n",
		"autoload -Uz compinit; compinit\n",
		"source $ZSH/oh-my-zsh.sh\r\n",
		"print \rcompinit\n",
		"autoload -Uz compinit && compinit\r # start completions\n",
		"autoload -Uz compinit && compinit # start completions\r\n",
		"autoload -Uz compinit && compinit -u\r\n",
		"autoload -Uz compinit && compinit;\r\n",
		"autoload -Uz compinit && compinit \r\n",
		"source $ZSH/oh-my-zsh.sh # load\r\n",
		// #555 shapes that zsh executes as written.
		"autoload -Uz compinit && compinit\n",
		"autoload -Uz compinit && compinit",
		"source $ZSH/oh-my-zsh.sh",
		"# autoload -Uz compinit && compinit\n",
		"  # source $ZSH/oh-my-zsh.sh\n",
		"fpath=(/x $fpath) # compinit comes later\n",
		// Escaped separators and blanks are word bytes: no comment starts.
		"print -r -- x\\;# ; autoload -Uz compinit && compinit\n",
		"print -r -- x\\&# ; autoload -Uz compinit && compinit\n",
		"print -r -- x\\|# ; autoload -Uz compinit && compinit\n",
		"print -r -- x\\ # ; autoload -Uz compinit && compinit\n",
		"print -r -- x\\\\;# autoload -Uz compinit && compinit\n",
		// extendedglob's (#...) inside a word or an argument stays code.
		"setopt extendedglob; [[ x = (#i)X ]] && autoload -Uz compinit && compinit\n",
		"setopt extendedglob; [[ ax = a(#i)X ]] && autoload -Uz compinit && compinit\n",
		"setopt extendedglob; [[ x = (a|(#i)X) ]] && autoload -Uz compinit && compinit\n",
		"setopt extendedglob; [[ xx = (x)# ]] && autoload -Uz compinit && compinit\n",
		"setopt extendedglob; print -r -- (#i)nomatch(N); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; print -r -- x=(#i)nomatch(N); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; print -r -- then (#i)nomatch(N); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; local -a a=(x) ; print -r -- b=(#i)nomatch(N); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; files=(/tmp(#qN)); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; files=( (#i)/TMP(N) ); autoload -Uz compinit && compinit\n",
		"setopt extendedglob; case x in (#i)X) autoload -Uz compinit && compinit;; esac\n",
		"[[ -n /tmp(#qN.mh+24) ]]; autoload -Uz compinit && compinit\n",
		"autoload -Uz compinit\nif [[ -n /tmp(#qN.mh+24) ]]; then\n  compinit\nelse\n  compinit -C\nfi\n",
		"autoload -Uz compinit; [[ -n /tmp(#qN.mh+24) ]] && compinit -C || compinit\n",
		"print -r -- a#b; autoload -Uz compinit && compinit\n",
		"print -r -- ${HOME}#x; autoload -Uz compinit && compinit\n",
		"print -r -- $(print a)#b; autoload -Uz compinit && compinit\n",
		// Token-level ( and ) do begin a token: the # after them is a comment.
		"(true)# autoload -Uz compinit && compinit\n",
		"fpath=(/x $fpath)# autoload -Uz compinit && compinit\n",
		"fpath+=(# autoload -Uz compinit && compinit\n)\n",
		"a[1]=(# autoload -Uz compinit && compinit\n)\n",
		"local -a a=(# autoload -Uz compinit && compinit\n)\n",
		"typeset -a a=(# autoload -Uz compinit && compinit\n)\n",
		"declare -a a=(# autoload -Uz compinit && compinit\n)\n",
		"export a=(# autoload -Uz compinit && compinit\n)\n",
		"FOO=1 fpath=(# autoload -Uz compinit && compinit\n)\n",
		"(# autoload -Uz compinit && compinit\n)\n",
		"if true; then (# autoload -Uz compinit && compinit\n:); fi\n",
		"while false; do (# autoload -Uz compinit && compinit\n:); done; :\n",
		"! (# autoload -Uz compinit && compinit\n:)\n",
		"{ (# autoload -Uz compinit && compinit\n:) }\n",
		"x=$(print a; print b) # autoload -Uz compinit && compinit\n",
		"x=$(print a)# autoload -Uz compinit && compinit\n",
		"true&&# autoload -Uz compinit && compinit\n",
		"true||# autoload -Uz compinit && compinit\n",
		// A quoted # on a line that runs compinit: executable, so zsh decides.
		"print -r -- 'x;# y'; autoload -Uz compinit && compinit\n",
		"print -r -- \"a # b\" && autoload -Uz compinit && compinit\n",
		"print -r -- \"\";# autoload -Uz compinit && compinit\n",
		// A CR beside quote marks or a backslash is still in the word.
		"autoload -Uz compinit && \"compinit\"\r\n",
		"autoload -Uz compinit && 'compinit'\r\n",
		"autoload -Uz compinit && compinit\"\"\r\n",
		"autoload -Uz compinit && compinit\\\r\n",
		"autoload -Uz compinit && \r\"compinit\"\n",
		"autoload -Uz compinit && \"compinit\" \r\n",
		"autoload -Uz compinit && \"compinit\"; \r\n",
		"autoload -Uz compinit && \"compinit\" # x\r\n",
		"autoload -Uz compinit && \\compinit\n",
		". \"$ZSH/oh-my-zsh.sh\"\r\n",
		"source \"$ZSH/oh-my-zsh.sh\"\n",
		// autoload -Uz compinit\r marks compinit^M, not compinit.
		"autoload -Uz compinit\r\ncompinit\r\n",
		"autoload -Uz compinit\r\ncompinit -u\r\n",
		"autoload -Uz compinit\r\nautoload -Uz compinit\ncompinit -u\r\n",
		"autoload -Uz compinit;compinit\r\n",
		"autoload -Uz compinit; compinit -u\r\n",
		"autoload -Uz compinit compaudit\r\ncompinit -u\r\n",
		// A command substitution holds commands: a # at a word start in it is
		// a comment, and a # right after its closing ) is a word byte.
		"value=$(print -r -- hello # compinit\n)\n",
		"cat <(print a # compinit\n)\nautoload -Uz compinit && compinit\n",
		"x=`print a # compinit`\n",
		"print -r -- $(print a)#b; autoload -Uz compinit && compinit\n",
		// An array assignment's words take comments; a # after its ) is a token.
		"fpath=(x # autoload -Uz compinit && compinit\n)\n",
		"fpath=(/x $fpath)# autoload -Uz compinit && compinit\n",
		// builtin and noglob autoload define compinit as plain autoload does.
		"builtin autoload -Uz compinit\ncompinit -u\n",
		"autoload -Uz compinit\r\nbuiltin autoload -Uz compinit # intact autoload\ncompinit -u\n",
		"autoload -Uz compinit\r\nnoglob autoload -Uz compinit\ncompinit -u\n",
		// #582 review: the scan reads the whole file as zsh does, so what a line
		// opens is still open on the next. A command substitution's ) on a later
		// line returns to its word, and a # after it is that word's byte.
		"x=$(print a\n)#; autoload -Uz compinit && compinit\n",
		"x=$(print a\n)# autoload -Uz compinit && compinit\n",
		"x=$(\n  print a # )\n)#; autoload -Uz compinit && compinit\n",
		"x=$(print ')'\n)#; autoload -Uz compinit && compinit\n",
		"x=$(print a; print b\n)#; autoload -Uz compinit && compinit\n",
		"x=$(\n(print a)\n)#; autoload -Uz compinit && compinit\n",
		"x=\"$(print a\n)\"#; autoload -Uz compinit && compinit\n",
		"x=`print a\n`#; autoload -Uz compinit && compinit\n",
		"print -r -- <(print a\n)#x >/dev/null; autoload -Uz compinit && compinit\n",
		"x=$(print a\n)\n# autoload -Uz compinit && compinit\n",
		"x=$(print a\n  # compinit\n)\n",
		// A case pattern's ) closes the pattern, not the substitution around it.
		"x=$(case a in a) print hi;; esac)#; autoload -Uz compinit && compinit\n",
		"x=$(case a in (a) print hi;; esac)#; autoload -Uz compinit && compinit\n",
		"x=$(case a in (a|b)) print hi;; esac)#; autoload -Uz compinit && compinit\n",
		"x=$(case a in\n  # c )\n  a) print hi;;\nesac)#; autoload -Uz compinit && compinit\n",
		"x=$(case a in a) print hi\nesac)#; autoload -Uz compinit && compinit\n",
		"case a in\n  a) autoload -Uz compinit && compinit ;;\nesac\n",
		"case a in a)# autoload -Uz compinit && compinit\n;; esac\n",
		"case a in\n  # a) autoload -Uz compinit && compinit ;;\n  a) : ;;\nesac\n",
		// A quoted string spans lines: its ; or ) is text, and the line after
		// it closes reads on as code.
		"print 'a\nb;# x'; autoload -Uz compinit && compinit\n",
		"print \"a\nb)# x\"; autoload -Uz compinit && compinit\n",
		"print 'a\nb';# autoload -Uz compinit && compinit\n",
		"print \"a\nb\";# autoload -Uz compinit && compinit\n",
		"print -r -- 'a\n# autoload -Uz compinit && compinit\nb'\n",
		"print -r -- $'it\\'s;# x'; autoload -Uz compinit && compinit\n",
		"print -r -- $'a\\'\nb';# autoload -Uz compinit && compinit\n",
		// ${...}, $(( )) and a word's own ( ) are text: their ; and # do not cut.
		"print -r -- ${x:-a;#b}; autoload -Uz compinit && compinit\n",
		"print -r -- ${x:-a # b}; autoload -Uz compinit && compinit\n",
		"print -r -- ${x:-a # compinit}\n",
		"print -r -- \"${x:-'}\";# autoload -Uz compinit && compinit\n",
		"print -r -- ${x:-'}'};# autoload -Uz compinit && compinit\n",
		"print -r -- $(( 16#ff )) $(( ##a )); autoload -Uz compinit && compinit\n",
		"(( x = 1 ))# autoload -Uz compinit && compinit\n",
		"for (( i = 0; i < 1; i++ )); do autoload -Uz compinit && compinit; done\n",
		"print -r -- x(a # b)(N); autoload -Uz compinit && compinit\n",
		"print -r -- (a|# b)(N); autoload -Uz compinit && compinit\n",
		"print -r -- x(a # compinit)(N)\n",
		"x=`print -r -- 'x;# y'; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- \"a # b\"; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print a # autoload -Uz compinit && compinit`; print -r -- $x\n",
		": <<''\ndon't\n\n:;# autoload -Uz compinit && compinit\n",
		": <<\"\"\ndon't\n\nautoload -Uz compinit && compinit\n",
		// A here-document's body opens nothing, and its ) is not code.
		"cat <<EOF >/dev/null\ndon't\nEOF\n# autoload -Uz compinit && compinit\n",
		"cat <<EOF >/dev/null\ndon't\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<-EOF >/dev/null\n\tit's\n\tEOF\nautoload -Uz compinit && compinit\n",
		"cat <<\"E F\" >/dev/null\nit's\nE F\nautoload -Uz compinit && compinit\n",
		"x=$(cat <<EOF\n)\nEOF\n)#; autoload -Uz compinit && compinit\n",
		"print -r -- <<<'x' >/dev/null;# autoload -Uz compinit && compinit\n",
		// A backslash-newline joins two lines into one.
		"print -r -- x\\\n#; autoload -Uz compinit && compinit\n",
		"print -r -- x \\\n# autoload -Uz compinit && compinit\n",
		"autoload -Uz \\\n  compinit\n",
		"# a comment \\\nautoload -Uz compinit && compinit\n",
		// A function body's subshell is in command position.
		"f() { (# autoload -Uz compinit && compinit\n:) }; f\n",
		// #582 review: a backquote body is unescaped as zsh unescapes it, then
		// lexed as an rc of its own.
		"x=`print -r -- $'it\\'s';# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=\"`print -r -- $'it\\'s';# autoload -Uz compinit && compinit`\"; print -r -- $x\n",
		"x=`print -r -- \\$'it\\\\'s';# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- 'a\\`b';# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- \\\"a;# b\\\"; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=\"`print -r -- \\\"a;# b\\\"; autoload -Uz compinit && compinit`\"; print -r -- $x\n",
		"x=`print -r -- a\\\\;# b; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- \\`print a;# b\\`; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- \\`print a\\`;# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- a\\ #;autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- a\\\n#; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- ${x:-a;#b}; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=${y:-`print -r -- a\\ #;autoload -Uz compinit && compinit`}; print -r -- $x\n",
		"x=\"`print -r -- a\\\\\\ #;autoload -Uz compinit && compinit`\"; print -r -- $x\n",
		"x=`case a in a) print hi;; esac;# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- 'a\nb;# c'; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- \"a\nb\";# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`cat <<EOF\n;# \nEOF\n`; autoload -Uz compinit && compinit\n",
		"x=$(print -r -- `print a;# )`; autoload -Uz compinit && compinit\n); print -r -- $x\n",
		"x=${y:-`print a;# }`}; autoload -Uz compinit && compinit\n",
		"case a in `print a;# )`) autoload -Uz compinit && compinit;; esac\n",
		// A body zsh cannot parse runs nothing; a word's ( ) left open parses.
		"x=`print -r -- $'a\\\\';# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- $'a\\\\\\\\';# autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`print -r -- $'a\\\\\\\\'; autoload -Uz compinit && compinit`; print -r -- $x\n",
		"x=`<<<'x'(;autoload -Uz compinit && compinit`; print -r -- $x\n",
		// Realistic backquote lines keep their verdicts.
		"eval `dircolors -b 2>/dev/null`\nautoload -Uz compinit;# compinit\n",
		"export GPG_TTY=`tty` # for gpg\nautoload -Uz compinit && compinit\n",
		"[[ `uname` == Darwin ]] && export CLICOLOR=1 # mac\n# autoload -Uz compinit && compinit\n",
		// #582 review: case WORD { ... } is zsh's other case form, and esac or }
		// ends either form, so a body holding one closes and runs.
		"x=`case x { x) autoload -Uz compinit && compinit ;; }`; print -r -- \"$x\"\n",
		"x=`case x { x) { print -r -- a; } }; autoload -Uz compinit && compinit`; print -r -- \"$x\"\n",
		"x=`case x {x) autoload -Uz compinit && compinit;; }`; print -r -- \"$x\"\n",
		"case x { y) : ;; };# autoload -Uz compinit && compinit\n",
		"case x { x) print -r -- a }; autoload -Uz compinit && compinit\n",
		"case x { x) print -r -- a };# autoload -Uz compinit && compinit\n",
		"case a in }; autoload -Uz compinit && compinit\n",
		"case a { a) : ;; esac;# autoload -Uz compinit && compinit\n",
		// A here-document delimiter is quote-removed, never expanded: $'EOF'
		// is EOF, $"EOF" is $EOF and $x is $x. A quoted delimiter's body is data;
		// an unquoted one runs only the $( ) and ` ` it expands.
		"x=`cat <<$'EOF'\ndon't\nEOF\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n",
		"x=`cat <<$x\ndon't\n$x\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n",
		"cat <<$\"EOF\" >/dev/null\ndon't\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<$x >/dev/null\ndon't\n$x\nautoload -Uz compinit && compinit\n",
		"cat <<$'E\\x46' >/dev/null\nit's\nEF\nautoload -Uz compinit && compinit\n",
		"cat <<'EOF' >/dev/null\nautoload -Uz compinit && compinit\nEOF\n",
		"cat <<EOF >/dev/null\nautoload -Uz compinit && compinit\nEOF\n",
		"cat <<EOF\n$(autoload -Uz compinit && compinit)\nEOF\n",
		"cat <<EOF\n`autoload -Uz compinit && compinit`\nEOF\n",
		"cat <<\\EOF\n$(autoload -Uz compinit && compinit)\nEOF\n",
		"cat <<\"EOF\"\n`autoload -Uz compinit && compinit`\nEOF\n",
		"cat <<EOF\n$(print -r -- a # autoload -Uz compinit && compinit\n)\nEOF\n",
		"<<$E\n;#compinit\n",
		// #582 review: here-document delimiters decoded as zsh decodes them.
		"cat <<$'\\u0045OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\U00000045OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\u0045OF' >/dev/null\nautoload -Uz compinit && compinit\nEOF\n",
		"x=`cat <<$'\\\\u0045OF'\nhello\nEOF\nautoload -Uz compinit && compinit`; print -r -- \"$x\"\n",
		"cat <<$'E\\qF' >/dev/null\nhello\nEqF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\C-aEOF' >/dev/null\nhello\n\x01EOF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\M-aEOF' >/dev/null\nhello\n\xe1EOF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\x45OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<$'\\105OF' >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<\"E\\$F\" >/dev/null\nhello\nE$F\nautoload -Uz compinit && compinit\n",
		"cat <<\"E\\\"F\" >/dev/null\nhello\nE\"F\nautoload -Uz compinit && compinit\n",
		"cat <<\"E\\qF\" >/dev/null\nhello\nE\\qF\nautoload -Uz compinit && compinit\n",
		"cat <<E\\OF >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<E\\OF >/dev/null\nautoload -Uz compinit && compinit\nEOF\n",
		// #582 review: a "..." delimiter is read as zsh's lexer reads a quoted
		// string. A $( ) inside keeps its text raw, backslashes and all, and its own
		// quotes do not close the word; only a backslash-newline right after $( goes.
		"cat <<\"$(print \\$foo)\" >/dev/null\nhello\n$(print \\$foo)\nautoload -Uz compinit && compinit\n",
		"cat <<\"$(print \\$foo)\" >/dev/null\nhello\n$(print $foo)\nautoload -Uz compinit && compinit\n$(print \\$foo)\n",
		"cat <<\"$(print \")\")\" >/dev/null\nhello\n$(print \")\")\nautoload -Uz compinit && compinit\n",
		"cat <<\"$(print '\"')\" >/dev/null\nhello\n$(print '\"')\nautoload -Uz compinit && compinit\n",
		"cat <<\"$(\\\nprint a)\" >/dev/null\nhello\n$(print a)\nautoload -Uz compinit && compinit\n",
		"cat <<\"$(print a\\\nb)\" >/dev/null\nhello\n$(print ab)\nautoload -Uz compinit && compinit\n",
		"cat <<\"$((print a); (print b))\" >/dev/null\nhello\n$((print a); (print b))\nautoload -Uz compinit && compinit\n",
		// ${ } takes the backslash off \} and holds nested quotes; a ` ` holds "
		// as a plain byte; $(( )) and $[ ] unescape as a quoted string does.
		"cat <<\"${x:-\\}}\" >/dev/null\nhello\n${x:-}}\nautoload -Uz compinit && compinit\n",
		"cat <<\"${x:-\\}}\" >/dev/null\nhello\n${x:-\\}}\nautoload -Uz compinit && compinit\n",
		"cat <<\"${x:-\"a\"}\" >/dev/null\nhello\n${x:-a}\nautoload -Uz compinit && compinit\n",
		"cat <<\"`print \"a\"`\" >/dev/null\nhello\n`print \"a\"`\nautoload -Uz compinit && compinit\n",
		"cat <<\"`print \\$a`\" >/dev/null\nhello\n`print $a`\nautoload -Uz compinit && compinit\n",
		"cat <<\"`print \\$a`\" >/dev/null\nhello\n`print \\$a`\nautoload -Uz compinit && compinit\n",
		"cat <<\"$((1+\\$x))\" >/dev/null\nhello\n$((1+$x))\nautoload -Uz compinit && compinit\n",
		"cat <<\"$((1+\\$x))\" >/dev/null\nhello\n$((1+\\$x))\nautoload -Uz compinit && compinit\n",
		"cat <<\"$(( (1) ))\" >/dev/null\nhello\n$(( (1) ))\nautoload -Uz compinit && compinit\n",
		"cat <<\"$[1+\\$x]\" >/dev/null\nhello\n$[1+$x]\nautoload -Uz compinit && compinit\n",
		// zsh only matches the delimiter against lines and never runs it.
		"cat <<compinit >/dev/null\nhello\ncompinit\n",
		"cat <<\"E\nautoload -Uz compinit && compinit\nF\" >/dev/null\n",
		// A quote in the delimiter that never closes is a parse error: nothing
		// from that command on runs.
		"cat <<\"${x\" >/dev/null\nautoload -Uz compinit && compinit\n${x\n",
		"cat <<\"`print a\" >/dev/null\nautoload -Uz compinit && compinit\n",
		"cat <<'abc >/dev/null\nautoload -Uz compinit && compinit\n",
		"cat <<$'abc >/dev/null\nautoload -Uz compinit && compinit\n",
		"cat <<\"abc >/dev/null\nautoload -Uz compinit && compinit\n",
		// Ordinary quoted delimiters keep their verdicts.
		"cat <<\"EOF\" >/dev/null\nhello\nEOF\nautoload -Uz compinit && compinit\n",
		"cat <<\"EOF\" >/dev/null\nautoload -Uz compinit && compinit\nEOF\n",
		"autoload -Uz compinit && compinit\ncat <<\"EOF\" >/dev/null\nhello\nEOF\n",
	}
	// Each rc runs in its own zsh at once, a CPU's worth at a time: one after
	// another they took most of the package's minute.
	ran := make([]bool, len(rcs))
	slots := make(chan struct{}, runtime.NumCPU())
	var wg sync.WaitGroup
	for i, rc := range rcs {
		rcFile := filepath.Join(dir, fmt.Sprintf("rc%d", i))
		if err := os.WriteFile(rcFile, []byte(rc), 0o644); err != nil {
			t.Fatal(err)
		}
		// -f: the operator's own startup files take no part. The temp
		// functions dir is first on fpath, so `autoload -Uz compinit` finds
		// the marker function, and ZSH names the loader the rc sources. The
		// environment is the test's own, not the caller's: a fixture such as
		// ${y:-`...`} would otherwise run or skip its backquote by whether the
		// caller happened to set y. zsh writes each here-document's body to a
		// file named from TMPPREFIX (default /tmp/zsh), so that points into the
		// test's temp dir too: a sandbox that forbids /tmp does not turn a
		// here-document fixture into a failure that reads as compinit not
		// running.
		cmd := exec.Command(zsh, "-f", "-c",
			`fpath=("$1" $fpath); ZSH=$2; source "$3"`, "compinit-oracle", fnDir, omzDir, rcFile)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "TMPDIR=" + dir,
			"TMPPREFIX=" + filepath.Join(dir, "zsh"), "LC_ALL=C"}
		cmd.Dir = dir
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			out, _ := cmd.Output()
			ran[i] = strings.Contains(string(out), marker)
		}()
	}
	wg.Wait()
	for i, rc := range rcs {
		if got := CompinitRuns([]byte(rc)); got != ran[i] {
			t.Errorf("CompinitRuns(%q) = %v, zsh ran compinit = %v", rc, got, ran[i])
		}
	}
}

// TestCompinitRunsVerbAnswersByExitStatus pins the contract install.sh reads:
// the verb's exit status alone is the verdict, so the hint needs no output
// parsing, and an rc that cannot be read answers not-running rather than
// failing the install.
func TestCompinitRunsVerbAnswersByExitStatus(t *testing.T) {
	dir := t.TempDir()
	runs := filepath.Join(dir, "runs")
	if err := os.WriteFile(runs, []byte("autoload -Uz compinit && compinit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idle := filepath.Join(dir, "idle")
	if err := os.WriteFile(idle, []byte("# autoload -Uz compinit && compinit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Main([]string{"compinit-runs", runs}, nil, nil, nil); got != 0 {
		t.Errorf("compinit-runs on an rc that runs compinit = %d, want 0", got)
	}
	if got := Main([]string{"compinit-runs", idle}, nil, nil, nil); got != 1 {
		t.Errorf("compinit-runs on a commented-out rc = %d, want 1", got)
	}
	if got := Main([]string{"compinit-runs", filepath.Join(dir, "absent")}, nil, nil, nil); got != 1 {
		t.Errorf("compinit-runs on an unreadable rc = %d, want 1", got)
	}
	if got := Main([]string{"compinit-runs"}, nil, nil, nil); got != 2 {
		t.Errorf("compinit-runs with no operand = %d, want 2", got)
	}
}

// #638: the fpath question is one rule. doctor asks FpathBound in-process and
// install.sh asks `util fpath-bound`; these are the shapes both answers cover.
func TestFpathBoundIsDoctorRule(t *testing.T) {
	dir := t.TempDir()
	site := filepath.Join(dir, "site-functions")
	withFile := filepath.Join(dir, "other")
	bare := filepath.Join(dir, "bare")
	for _, d := range []string{site, withFile, bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(withFile, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		rc    string
		fpath string
		want  bool
	}{
		{"rc names the directory", "fpath=(" + site + " $fpath)\n", "", true},
		{"rc names ccmorph's site-functions", "fpath=($HOME/.config/ccmorph/share/zsh/site-functions $fpath)\n", "", true},
		{"live fpath holds the directory", "autoload -Uz compinit\n", bare + string(os.PathListSeparator) + site, true},
		{"live fpath holds a directory with _ccmorph", "autoload -Uz compinit\n", withFile, true},
		{"rc names neither and fpath is elsewhere", "autoload -Uz compinit\n", bare, false},
		{"no rc and no fpath", "", "", false},
	} {
		if got := FpathBound([]byte(c.rc), site, c.fpath); got != c.want {
			t.Errorf("%s: FpathBound = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestFpathBoundVerbAnswersByExitStatus pins the contract install.sh reads: the
// status alone is the verdict, an absent rc is empty as doctor reads it, and
// the live FPATH in the environment counts.
func TestFpathBoundVerbAnswersByExitStatus(t *testing.T) {
	t.Setenv("FPATH", "")
	dir := t.TempDir()
	site := filepath.Join(dir, "site-functions")
	if err := os.MkdirAll(site, 0o755); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, "rc")
	if err := os.WriteFile(rc, []byte("fpath=("+site+" $fpath)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Main([]string{"fpath-bound", rc, site}, nil, nil, nil); got != 0 {
		t.Errorf("fpath-bound on an rc naming the dir = %d, want 0", got)
	}
	t.Setenv("FPATH", site)
	if got := Main([]string{"fpath-bound", filepath.Join(dir, "absent"), site}, nil, nil, nil); got != 0 {
		t.Errorf("fpath-bound with the dir on FPATH = %d, want 0", got)
	}
	t.Setenv("FPATH", "")
	if got := Main([]string{"fpath-bound", filepath.Join(dir, "absent"), site}, nil, nil, nil); got != 1 {
		t.Errorf("fpath-bound with no rc and empty FPATH = %d, want 1", got)
	}
	if got := Main([]string{"fpath-bound", rc}, nil, nil, nil); got != 2 {
		t.Errorf("fpath-bound with one operand = %d, want 2", got)
	}
}
