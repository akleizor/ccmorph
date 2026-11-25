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

// claudeCredentialService is the login-keychain service under which Claude Code keeps a
// config dir's credentials: a hash of the dir names it. ccmorph-rc-hosts's
// keychain_service_for spells the same rule; TestClaudeCredentialServiceGolden and
// test_keychain_service_matches_the_go_golden pin both to one value (#636).
func claudeCredentialService(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
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
		return readKeychain(claudeCredentialService(dir), account)
	}
	return os.ReadFile(filepath.Join(dir, ".credentials.json"))
}
