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
// file mentions. Shapes the parser deliberately answers by mention rather than
// execution (a quoted string, say) are not in this table.
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
