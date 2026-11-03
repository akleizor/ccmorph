package util

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A private view that cannot be built must say so and never print the hop's
// cache notice: the session it announces is not starting (#226).
func TestPrivateViewFailureSkipsTheNotice(t *testing.T) {
	real := filepath.Join(t.TempDir(), "demo")
	os.Mkdir(real, 0o300) // listable by no one: the view cannot link its entries
	t.Cleanup(func() { os.Chmod(real, 0o700) })
	t.Setenv("CLAUDE_CONFIG_DIR", real)
	t.Setenv("TMPDIR", t.TempDir())
	said := false
	var errw strings.Builder
	if rc := privateSession("/bin/true", nil, "", func() { said = true }, &errw); rc != 1 || said {
		t.Fatalf("rc %d, notice printed %v", rc, said)
	}
	if !strings.Contains(errw.String(), "private view") {
		t.Errorf("stderr %q", errw.String())
	}
}

// The launch env comes from a 0600 file that is removed on read, never from the
// helper's argument vector (#112).
func TestSessionEnvComesFromAPrivateFileAndIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(path, []byte("A=1\x00B=x\ny\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSessionEnv(path)
	if err != nil || strings.Join(got, "|") != "A=1|B=x\ny" {
		t.Fatalf("%q, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("env file left behind: %v", err)
	}
	loose := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(loose, []byte("A=1\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The write is masked by the runner's umask (077 lands it 0600, and the
	// refusal below never happens), so the fixture pins its mode itself (#142).
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readSessionEnv(loose); err == nil {
		t.Fatal("a world-readable env file was accepted")
	}
}

// A private session still starts when TMPDIR cannot be written: the view falls
// back to the run/ root the launch env file already uses (#112), handed over by
// the launcher or, from an older one, derived from the state dir, and is removed
// when claude exits (#140).
func TestPrivateViewStartsUnderTheStateRunDirWhenTmpdirCannotBeWritten(t *testing.T) {
	for _, handed := range []bool{true, false} {
		name := "key-from-launcher"
		if !handed {
			name = "older-launcher-no-key"
		}
		t.Run(name, func(t *testing.T) {
			real := filepath.Join(t.TempDir(), "demo")
			os.Mkdir(real, 0o700)
			ro, state, scratch := t.TempDir(), t.TempDir(), t.TempDir()
			os.Chmod(ro, 0o555)
			t.Cleanup(func() { os.Chmod(ro, 0o755) })
			t.Setenv("TMPDIR", ro)
			t.Setenv("CLAUDE_CONFIG_DIR", real)
			runDir := ""
			if handed {
				runDir = filepath.Join(state, "run")
			} else {
				t.Setenv("CCMORPH_STATE_DIR", state)
			}
			out := filepath.Join(scratch, "view")
			script := filepath.Join(scratch, "claude")
			os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$CLAUDE_CONFIG_DIR\" > '"+out+"'; exit 7\n"), 0o755)
			if rc := privateSession(script, nil, runDir, func() {}, &strings.Builder{}); rc != 7 {
				t.Fatalf("rc %d, want claude's own 7", rc)
			}
			b, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(b); !strings.HasPrefix(got, filepath.Join(state, "run", "ccmorph-private.")) {
				t.Errorf("view %q, want under %s", got, filepath.Join(state, "run"))
			}
			if left, _ := filepath.Glob(filepath.Join(state, "run", "ccmorph-private.*")); len(left) != 0 {
				t.Errorf("view left behind: %q", left)
			}
		})
	}
}

func TestPrivateViewLinksConfigButNotSessionState(t *testing.T) {
	real := filepath.Join(t.TempDir(), "demo")
	for _, n := range []string{"settings.json", "projects", ".claude.json"} {
		os.MkdirAll(filepath.Join(real, n), 0o700)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", real)
	t.Setenv("TMPDIR", t.TempDir())
	out := filepath.Join(t.TempDir(), "seen")
	script := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(script, []byte("#!/bin/sh\nls -A \"$CLAUDE_CONFIG_DIR\" > '"+out+"'; echo \"$CLAUDE_SECURESTORAGE_CONFIG_DIR\" >> '"+out+"'; exit 7\n"), 0o755)
	if rc := privateSession(script, nil, "", func() {}, &strings.Builder{}); rc != 7 {
		t.Fatalf("rc %d, want claude's own 7", rc)
	}
	b, _ := os.ReadFile(out)
	if got := string(b); got != ".claude.json\nsettings.json\n"+real+"\n" {
		t.Errorf("view saw %q", got)
	}
}
