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
