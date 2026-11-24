package util

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/profile"
)

// GH-638: a legacy stem over MaxNameLen can still reach rm through
// profile.RemovableName, and rm reaches KeychainDelete. The gate that refuses
// it is profile's one name rule, so no keychain command may run. The fake
// security is also the proof the observation works: a name the gate accepts
// does reach it.
func TestKeychainDeleteRefusesALongStemWithoutTouchingTheKeychain(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("KeychainDelete only runs on darwin")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "security")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + calls + "\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	long := strings.Repeat("a", profile.MaxNameLen+1)
	KeychainDelete(long)
	if data, err := os.ReadFile(calls); err == nil {
		t.Fatalf("KeychainDelete ran security for a %d-byte name: %s", len(long), data)
	}

	KeychainDelete("delete_probe")
	data, err := os.ReadFile(calls)
	if err != nil || len(data) == 0 {
		t.Fatalf("the fake security was not reached for a valid name (%v); the refusal above proves nothing", err)
	}
}

// Runs against the real login keychain, so only where one exists.
func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if runtime.GOOS != "darwin" || exec.Command("security", "default-keychain").Run() != nil {
		t.Skip("no login keychain")
	}
	dir := t.TempDir()
	name := "ccmtest" + strings.ReplaceAll(filepath.Base(dir), "-", "")
	t.Cleanup(func() { KeychainDelete(name) })
	conf := filepath.Join(dir, name+".conf")
	secret := `sk-"quoted"\back`
	os.WriteFile(conf, []byte("type=apikey\napi_key="+secret+"\n"), 0o600)
	if err := KeychainMigrate(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") || strings.Contains(string(data), "sk-") {
		t.Fatal(string(data))
	}
	if got, err := profile.KeychainGet("keychain:" + name + ".api_key"); err != nil || got != secret {
		t.Fatalf("got %q %v", got, err)
	}
}

// GH-848: the keychain walk scanned the state root with the same unescaped
// join as the router (see router/profiles_test.go for the cause); a root
// holding `[` made the pattern bad and the walk silently migrated nothing.
// The walk runs against a fake security on PATH that records its stdin, so
// the check works wherever the tests run — and every write names the profile
// it stored, which is what proves the walk saw exactly a and b: c.conf links
// to a plaintext conf kept OUTSIDE the scanned root, so a walk that visited
// the link would find a plain secret to store under c's own name, and its
// atomic rewrite would replace the link itself.
func TestKeychainWalkMigratesEveryConfUnderABracketedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state[cd")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := t.TempDir()
	calls := filepath.Join(fake, "calls")
	script := filepath.Join(fake, "security")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >> \""+calls+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))

	linkTarget := filepath.Join(t.TempDir(), "target.conf")
	if err := os.WriteFile(linkTarget, []byte("type=apikey\napi_key=sk-plain-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "bad-name"} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"),
			[]byte("type=apikey\napi_key=sk-plain-"+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(linkTarget, filepath.Join(root, "c.conf")); err != nil {
		t.Fatal(err)
	}

	if err := keychainWalk(root); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		data, err := os.ReadFile(filepath.Join(root, name+".conf"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") {
			t.Fatalf("%s.conf was not migrated under a root holding `[`: %s", name, data)
		}
	}

	// The keychain saw exactly the a and b writes: one store per migrated
	// profile, and no c or bad-name — those confs were never walked.
	stored, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("the fake security was never reached (%v); the walk migrated nothing", err)
	}
	if n := strings.Count(string(stored), "\n"); n != 2 {
		t.Fatalf("the walk issued %d keychain writes, want exactly 2 (a and b):\n%s", n, stored)
	}
	for _, ref := range []string{`-a "a.api_key"`, `-a "b.api_key"`} {
		if !strings.Contains(string(stored), ref) {
			t.Fatalf("the walk never issued %s:\n%s", ref, stored)
		}
	}
	for _, ref := range []string{`-a "c.api_key"`, `-a "bad-name.api_key"`} {
		if strings.Contains(string(stored), ref) {
			t.Fatalf("the walk stored a skipped conf %s:\n%s", ref, stored)
		}
	}

	// The skipped confs kept their bytes untouched: bad-name still plain,
	// and c.conf still a link to its plain target outside the root.
	if data, err := os.ReadFile(filepath.Join(root, "bad-name.conf")); err != nil ||
		!strings.Contains(string(data), "api_key=sk-plain-bad-name") {
		t.Fatalf("bad-name.conf holds no valid profile yet was rewritten: %s (%v)", data, err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "c.conf")); err != nil ||
		fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("c.conf was replaced as a file, so the walk visited the link: %v", err)
	}
	if data, err := os.ReadFile(linkTarget); err != nil ||
		!strings.Contains(string(data), "api_key=sk-plain-c") {
		t.Fatalf("the link's target was rewritten, so the walk visited c.conf: %s (%v)", data, err)
	}
}
