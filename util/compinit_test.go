package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	rcFile := filepath.Join(dir, "rc")
	for _, rc := range []string{
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
	} {
		if err := os.WriteFile(rcFile, []byte(rc), 0o644); err != nil {
			t.Fatal(err)
		}
		// -f: the operator's own startup files take no part. The temp
		// functions dir is first on fpath, so `autoload -Uz compinit` finds
		// the marker function, and ZSH names the loader the rc sources.
		out, _ := exec.Command(zsh, "-f", "-c",
			`fpath=("$1" $fpath); ZSH=$2; source "$3"`, "compinit-oracle", fnDir, omzDir, rcFile).Output()
		ran := strings.Contains(string(out), marker)
		if got := CompinitRuns([]byte(rc)); got != ran {
			t.Errorf("CompinitRuns(%q) = %v, zsh ran compinit = %v", rc, got, ran)
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
