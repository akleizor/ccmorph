package util

import "testing"

// TestClaudeCredentialServiceGolden pins the keychain service-name rule to one value;
// tests/rc_hosts_test.py test_keychain_service_matches_the_go_golden pins the
// add-on's Python spelling to the same one, so the two cannot drift (#636).
func TestClaudeCredentialServiceGolden(t *testing.T) {
	if got := claudeCredentialService("/Users/a/.config/ccmorph/profiles/work"); got != "Claude Code-credentials-20e9b7cb" {
		t.Fatalf("claudeCredentialService = %q", got)
	}
}
