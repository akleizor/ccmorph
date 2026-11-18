package util

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
)

// ReadSigninKeychain reads the specified Claude Code keychain item.
func ReadSigninKeychain(service, account string) ([]byte, error) {
	return exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w").Output()
}

// ReadSigninCredentials reads Claude Code's credential storage for a profile.
func ReadSigninCredentials(dir string, readKeychain func(string, string) ([]byte, error)) ([]byte, error) {
	if runtime.GOOS == "darwin" {
		account := ""
		for _, k := range []string{"LOGNAME", "USER", "LNAME", "USERNAME"} {
			if account = os.Getenv(k); account != "" {
				break
			}
		}
		if account == "" {
			if u, err := user.Current(); err == nil {
				account = u.Username
			}
		}
		sum := sha256.Sum256([]byte(dir))
		return readKeychain("Claude Code-credentials-"+hex.EncodeToString(sum[:])[:8], account)
	}
	return os.ReadFile(filepath.Join(dir, ".credentials.json"))
}
