package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// completionRow runs doctorCompletion for an install rooted at root and
// returns what it printed.
func completionRow(t *testing.T, l *launcher) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	l.doctorCompletion()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// completionHint runs the whole completion hint the way doctor's report
// prints it — the fpath half, then the compinit half from the same reading —
// and returns what it printed.
func completionHint(t *testing.T, l *launcher) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	l.doctorCompinit(l.doctorCompletion())
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func completionInstall(t *testing.T, root string) *launcher {
	t.Helper()
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &launcher{libDir: filepath.Join(root, "lib")}
}

// secureForCompinit drops group and world write from the install's directories.
// compinit refuses to run over a group-writable fpath directory (it prompts on
// a terminal and aborts without one), and MkdirAll under a umask of 002 — CI's —
// makes exactly that; a new zsh would then complete nothing, which the row
// must say, but these cases need the startup they describe to be the one run.
func secureForCompinit(t *testing.T, root string) {
	t.Helper()
	for _, d := range []string{root, filepath.Join(root, "share"), filepath.Join(root, "share", "zsh"),
		filepath.Join(root, "share", "zsh", "site-functions")} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDoctorCompletionSaysOnlyWhatANewZshStillMisses holds every state of a
// person's zsh startup the completion row has to answer for (#363): the row is
// doctor's word that Tab completes in a new zsh, and zsh starts no completion
// system by itself, so a startup file that only loads the directory onto fpath
// must not quiet it — a new shell that has a completion bound to the ccmorph
// command must, and the row may name only the half the startup files miss.
// The expected lines are spelled here the way a person types them, never built
// with doctor's own shellWord, so a quoting bug cannot hide behind shared code.
func TestDoctorCompletionSaysOnlyWhatANewZshStillMisses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	secureForCompinit(t, root)
	// A cold compinit under a loaded CI box can pass the production 5s; a probe
	// that times out would print a row where these cases expect silence.
	deadline := completionProbeDeadline
	completionProbeDeadline = 60 * time.Second
	t.Cleanup(func() { completionProbeDeadline = deadline })
	// The t.TempDir paths carry no character zsh would quote, so doctor must
	// print them bare — the assertion checks it without reusing shellWord.
	fpathLine := "fpath=(" + dir + " $fpath)"
	compinitLine := "autoload -Uz compinit && compinit"
	// The quiet cases run compinit -u, as tests/completion_test.py does: GitHub's
	// Ubuntu image runs `chmod -R 777 /usr/share`, so compaudit rejects zsh's own
	// fpath there and a plain compinit aborts in a shell with no terminal — no new
	// zsh on that host completes anything, and the row rightly stays.
	compinitAnyHost := compinitLine + " -u"
	// addCompinit is the whole row for a home whose fpath already holds the
	// directory: the line that starts completions and nothing about fpath.
	addCompinit := "INFO completion: nothing in ~/.zshrc runs compinit — add:\n" +
		"INFO   autoload -Uz compinit && compinit\n"

	brew := t.TempDir() // Homebrew's own site-functions, where brew links the file
	if err := os.WriteFile(filepath.Join(brew, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// compinit also audits the directory and its parent (see secureForCompinit).
	for _, p := range []string{filepath.Dir(brew), brew, filepath.Join(brew, "_ccmorph")} {
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		id      string
		rc      string // the person's .zshrc; "" leaves none
		env     [][2]string
		quiet   bool     // a new zsh completes ccmorph: the row must be gone
		want    []string // lines the row must carry
		notWant []string // lines it must not repeat
		exact   string   // the whole row, where only one wording is right
	}{
		{
			// A brand-new home: zsh runs neither an fpath line nor compinit.
			id:   "empty-home-gh363",
			want: []string{fpathLine, compinitLine},
		},
		{
			// The issue's report: the row used to vanish once the fpath line
			// was added, though Tab still completed nothing.
			id:      "fpath-line-only-gh363",
			rc:      fpathLine + "\n",
			want:    []string{compinitLine},
			notWant: []string{fpathLine},
			exact:   addCompinit,
		},
		{
			id:    "fpath-and-compinit",
			rc:    fpathLine + "\n" + compinitAnyHost + "\n",
			quiet: true,
		},
		{
			// compinit alone: the directory is still not on fpath.
			id:   "compinit-only",
			rc:   compinitLine + "\n",
			want: []string{fpathLine},
		},
		{
			// The fpath line runs after compinit, so compinit scans an fpath
			// without the directory; the row says where the line belongs.
			id:      "fpath-below-compinit-gh363",
			rc:      compinitLine + "\n" + fpathLine + "\n",
			want:    []string{fpathLine, "above compinit"},
			notWant: []string{compinitLine},
		},
		{
			// FPATH in this process's environment completes nothing in a new
			// terminal; the missing half is the line that starts completions.
			// The value keeps the colon-separated shape main exercised: the
			// install directory is the second entry, so a scan that stops at
			// one element finds nothing.
			id:      "fpath-from-env-only-gh363",
			env:     [][2]string{{"FPATH", "/usr/share/zsh:" + dir}},
			want:    []string{compinitLine},
			notWant: []string{fpathLine},
			exact:   addCompinit,
		},
		{
			// Homebrew: the file is linked into brew's site-functions, which
			// brew shellenv puts on fpath; nothing runs compinit. The brew
			// directory is the second entry of a colon-separated FPATH.
			id:      "brew-fpath-no-compinit-gh363",
			env:     [][2]string{{"FPATH", "/usr/local/share/zsh/site-functions:" + brew}},
			want:    []string{compinitLine},
			notWant: []string{fpathLine},
			exact:   addCompinit,
		},
		{
			// The Homebrew person whose rc runs compinit: a new zsh completes.
			id:    "brew-rc-compinit",
			rc:    "fpath=(" + brew + " $fpath)\n" + compinitAnyHost + "\n",
			quiet: true,
		},
		{
			id:    "fish-login-shell",
			env:   [][2]string{{"SHELL", "/usr/bin/fish"}},
			rc:    fpathLine + "\n",
			quiet: true,
		},
		{
			// No shell to ask ($SHELL runs nothing): a probe that cannot
			// answer is never a working verdict, so the row still says what
			// the startup files miss — here, where the fpath line belongs.
			id:      "no-shell-both-halves",
			env:     [][2]string{{"SHELL", "/no/such/shell"}},
			rc:      fpathLine + "\n" + compinitLine + "\n",
			want:    []string{fpathLine, "above compinit"},
			notWant: []string{compinitLine},
		},
		{
			// The same fallback must not repeat the issue's bug: an fpath
			// line alone is not a working completion.
			id:      "no-shell-fpath-only-gh363",
			env:     [][2]string{{"SHELL", "/no/such/shell"}},
			rc:      fpathLine + "\n",
			want:    []string{compinitLine},
			notWant: []string{fpathLine},
			exact:   addCompinit,
		},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("ZDOTDIR", "")
			t.Setenv("SHELL", "/bin/zsh")
			t.Setenv("FPATH", "")
			for _, kv := range tc.env {
				t.Setenv(kv[0], kv[1])
			}
			if tc.rc != "" {
				if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(tc.rc), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got := completionHint(t, l)
			if tc.quiet {
				if got != "" {
					t.Fatalf("doctor row = %q, want silence: a new zsh completes ccmorph", got)
				}
				return
			}
			if got == "" {
				t.Fatal("doctor row is empty, want it to say what a new zsh still misses")
			}
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("doctor row = %q, want it to carry %q", got, want)
				}
			}
			for _, banned := range tc.notWant {
				if strings.Contains(got, banned) {
					t.Errorf("doctor row = %q, want it to not repeat %q", got, banned)
				}
			}
			if tc.exact != "" && got != tc.exact {
				t.Errorf("doctor row = %q, want exactly %q: the directory is already on fpath, so the row names only the compinit line", got, tc.exact)
			}
		})
	}
}

// TestDoctorCompletionQuotesADirectoryWithSpaces keeps the one place quoting
// shows: a directory with a space must reach the row as one shell word.
func TestDoctorCompletionQuotesADirectoryWithSpaces(t *testing.T) {
	root := filepath.Join(t.TempDir(), "my libexec")
	l := completionInstall(t, root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("FPATH", "")
	got := completionRow(t, l)
	want := "fpath=('" + filepath.Join(root, "share", "zsh", "site-functions") + "' $fpath)"
	if !strings.Contains(got, want) {
		t.Fatalf("doctor row = %q, want one quoted element %q", got, want)
	}
}

// TestDoctorCompletionSpeaksWhenTheProbeCannotAnswer is the landing gate's
// repro: a .zshrc whose fpath line and a comment naming compinit satisfy the
// static read while the shell it would start never answers — here a sleep that
// outlasts a deadline the test shortens, standing in for the gate's hung zsh.
// Silence there would read as "Tab completion works"; the row must still speak.
func TestDoctorCompletionSpeaksWhenTheProbeCannotAnswer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	home := t.TempDir()
	rc := "fpath=(" + dir + " $fpath)\n# compinit has not been enabled yet\nsleep 6\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("FPATH", "")
	deadline := completionProbeDeadline
	completionProbeDeadline = 300 * time.Millisecond
	t.Cleanup(func() { completionProbeDeadline = deadline })
	got := completionHint(t, l)
	if got == "" {
		t.Fatal("doctor row is empty, want it to speak: a probe that cannot answer is not a working completion")
	}
	if want := "fpath=(" + dir + " $fpath)"; !strings.Contains(got, want) {
		t.Errorf("doctor row = %q, want it to carry %q", got, want)
	}
}

// TestDoctorCompletionHintFitsInTwoShortLines holds the issue's width: an
// ordinary install — a site-functions path as long as /Users/newuser's — on an
// 80-column terminal. Whatever a person's startup files miss, the whole hint is
// two short lines, and no line may wrap.
func TestDoctorCompletionHintFitsInTwoShortLines(t *testing.T) {
	t.Chdir(t.TempDir()) // a short base, so the install path is the real variable
	// 31 c's plus /share/zsh/site-functions: the 56-character directory of an
	// ordinary /Users/newuser install.
	root := strings.Repeat("c", 31)
	l := completionInstall(t, root)
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	fpathLine := "fpath=(" + dir + " $fpath)"
	compinitLine := "autoload -Uz compinit && compinit"
	for _, rc := range []string{
		"",                                     // nothing set up: both halves missing
		fpathLine + "\n",                       // the directory, no compinit
		compinitLine + "\n",                    // compinit, no directory
		compinitLine + "\n" + fpathLine + "\n", // the fpath line below compinit
	} {
		home := t.TempDir()
		if rc != "" {
			if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("HOME", home)
		t.Setenv("ZDOTDIR", "")
		t.Setenv("SHELL", "/bin/zsh")
		t.Setenv("FPATH", "")
		got := completionHint(t, l)
		var rows []string
		if got != "" {
			rows = strings.Split(strings.TrimRight(got, "\n"), "\n")
		}
		if len(rows) > 2 {
			t.Errorf("rc %q: doctor hint is %d rows, want at most two:\n%s", rc, len(rows), got)
		}
		for _, row := range rows {
			if n := utf8.RuneCountInString(row); n > 80 {
				t.Errorf("rc %q: doctor row is %d columns, want 80 or fewer: %q", rc, n, row)
			}
		}
	}
}

// TestDoctorCompletionPrintsTheLineForAnInstallZshDoesNotSearch is main's
// plain-path check, kept verbatim.
func TestDoctorCompletionPrintsTheLineForAnInstallZshDoesNotSearch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("FPATH", "")
	got := completionRow(t, l)
	want := "fpath=(" + filepath.Join(root, "share", "zsh", "site-functions") + " $fpath)"
	if !strings.Contains(got, want) {
		t.Fatalf("doctor row = %q, want it to carry %q", got, want)
	}
}

// TestDoctorCompletionStaysSilentWhenZshAlreadyFindsIt is main's silence guard,
// kept verbatim: once zsh reaches the file the fpath half prints nothing.
func TestDoctorCompletionStaysSilentWhenZshAlreadyFindsIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "libexec")
	l := completionInstall(t, root)
	dir := filepath.Join(root, "share", "zsh", "site-functions")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")

	t.Setenv("FPATH", "/usr/share/zsh:"+dir)
	if got := completionRow(t, l); got != "" {
		t.Errorf("dir on FPATH: want no row, got %q", got)
	}

	// Homebrew's own site-functions, where brew links the file.
	brew := t.TempDir()
	os.WriteFile(filepath.Join(brew, "_ccmorph"), []byte("#compdef ccmorph\n"), 0o644)
	t.Setenv("FPATH", brew)
	if got := completionRow(t, l); got != "" {
		t.Errorf("_ccmorph elsewhere on FPATH: want no row, got %q", got)
	}

	t.Setenv("FPATH", "")
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("fpath=("+dir+" $fpath)\n"), 0o644)
	if got := completionRow(t, l); got != "" {
		t.Errorf(".zshrc names the dir: want no row, got %q", got)
	}
	os.Remove(filepath.Join(home, ".zshrc"))

	t.Setenv("SHELL", "/usr/bin/fish")
	if got := completionRow(t, l); got != "" {
		t.Errorf("fish login shell: want no row, got %q", got)
	}
}
