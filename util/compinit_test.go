package util

import (
	"os"
	"path/filepath"
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
	} {
		if got := CompinitRuns([]byte(c.rc)); got != c.want {
			t.Errorf("CompinitRuns(%q) = %v, want %v", c.rc, got, c.want)
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
