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
	// Pin every key bashEnv may read from this process's env, so the test
	// runs the branches it means to on any machine, including one inside a
	// live ccmorph session (#973).
	t.Setenv("HTTPS_PROXY", "http://corp:3128")
	t.Setenv("CCMORPH_AGENT_PROXY", "/outer/agents")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.person.example")
	t.Setenv("CLAUDE_CODE_TETHER_LIVE", "1")
	// The person never set https_proxy: t.Setenv first so cleanup puts back
	// whatever the machine had, then unset it so the absent branch is the
	// one under test rather than whatever the operator exported.
	t.Setenv("https_proxy", "http://corp:3128")
	os.Unsetenv("https_proxy")
	h := &agentHop{run: dir}
	path, err := h.bashEnv([]string{"NODE_EXTRA_CA_CERTS=/corp/ca's.pem"}, theirs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", "-c", `
export HTTPS_PROXY=http://127.0.0.1:1 https_proxy=http://127.0.0.1:1 NODE_EXTRA_CA_CERTS=/bundle ANTHROPIC_BASE_URL=x CCMORPH_AGENT_PROXY=/d CLAUDE_CODE_TETHER_LIVE=0
. "$1"; echo "$THEIRS|$HTTPS_PROXY|${https_proxy-unset}|$NODE_EXTRA_CA_CERTS|${ANTHROPIC_BASE_URL-unset}|${CCMORPH_AGENT_PROXY-unset}|${CLAUDE_CODE_TETHER_LIVE-unset}"`, "sh", path).Output()
	if err != nil {
		t.Fatal(err)
	}
	// One field per line bashEnv writes, naming the branch it pins: THEIRS
	// comes from sourcing the person's own env file; HTTPS_PROXY and
	// comes from os.LookupEnv (restore-when-present); ANTHROPIC_BASE_URL is a
	// backend key claude-session clears, so it is unset though the process has it;
	// https_proxy is dropped because neither the session env nor the
	// process env has it (unset-when-absent); NODE_EXTRA_CA_CERTS comes
	// from the passed session env list; CCMORPH_AGENT_PROXY is the dir key,
	// which bashEnv never restores even though the process env has it;
	// CLAUDE_CODE_TETHER_LIVE gets back the person's own value.
	if got := strings.TrimSpace(string(out)); got != "kept|http://corp:3128|unset|/corp/ca's.pem|unset|unset|1" {
		t.Fatalf("Bash env: %s", got)
	}
}
