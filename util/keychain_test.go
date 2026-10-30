package util

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Runs against the real login keychain, so only where one exists.
func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if runtime.GOOS != "darwin" || exec.Command("security", "default-keychain").Run() != nil {
		t.Skip("no login keychain")
	}
	dir := t.TempDir()
	name := "ccmtest" + strings.ReplaceAll(filepath.Base(dir), "-", "")
	t.Cleanup(func() { keychainDelete([]string{name}, nil, nil, nil) })
	conf := filepath.Join(dir, name+".conf")
	secret := `sk-"quoted"\back`
	os.WriteFile(conf, []byte("type=apikey\napi_key="+secret+"\n"), 0o600)
	if keychainMigrate([]string{dir}, nil, nil, os.Stderr) != 0 {
		t.Fatal("migrate failed")
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") || strings.Contains(string(data), "sk-") {
		t.Fatal(string(data))
	}
	var out bytes.Buffer
	if keychainGet([]string{"keychain:" + name + ".api_key"}, nil, &out, os.Stderr) != 0 || out.String() != secret {
		t.Fatalf("got %q", out.String())
	}
}
