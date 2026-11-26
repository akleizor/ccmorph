package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Bash command in the session runs with the env the session had before the
// proxy: curl or Python never meet the proxy or its CA.
func TestBashChildrenGetTheSessionsOwnEnvBack(t *testing.T) {
	dir := t.TempDir()
	theirs := filepath.Join(dir, "theirs.sh")
	os.WriteFile(theirs, []byte("export THEIRS=kept\n"), 0o600)
	t.Setenv("HTTPS_PROXY", "http://corp:3128")
	t.Setenv("CCMORPH_AGENT_PROXY", "/outer/agents")
	h := &agentHop{run: dir}
	path, err := h.bashEnv([]string{"NODE_EXTRA_CA_CERTS=/corp/ca's.pem"}, theirs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", `
export HTTPS_PROXY=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1 NODE_EXTRA_CA_CERTS=/bundle ANTHROPIC_BASE_URL=x CCMORPH_AGENT_PROXY=/d
. "$1"; echo "$THEIRS|$HTTPS_PROXY|${https_proxy-unset}|$NODE_EXTRA_CA_CERTS|${ANTHROPIC_BASE_URL-unset}|${CCMORPH_AGENT_PROXY-unset}"`, "sh", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "kept|http://corp:3128|unset|/corp/ca's.pem|unset|unset" {
		t.Fatalf("Bash env: %s", got)
	}
}
