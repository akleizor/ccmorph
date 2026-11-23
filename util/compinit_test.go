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
