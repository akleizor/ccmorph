package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// A Remote Control edge session sends its inference through the edge's proxy,
// which can serve any profile it holds. So a subagent picked on another profile
// still runs as Claude Code's own subagent: the hook leaves the pick for the
// proxy, and the proxy sends that one agent's requests to the picked profile
// (rc_route.claim_agent holds the other half of this protocol).

func rcBridge() string { return filepath.Join(paths.ProfileRoot(), "profiles", ".rc-bridge") }

// edgePort is this session's edge proxy port, when it is an edge session.
func edgePort() (int, bool) {
	if os.Getenv("CCMORPH_EDGE_ROUTE_ENV") == "" {
		return 0, false
	}
	return util.LoopbackProxyPort()
}

// edgeRoute is the route whose edge record holds this proxy port; "" when none does.
func edgeRoute(bridge string, port int) string {
	records, _ := filepath.Glob(filepath.Join(paths.GlobQuote(bridge), "edges", "*.json"))
	for _, f := range records {
		var rec struct{ Port int }
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &rec) == nil && rec.Port == port {
			return strings.TrimSuffix(filepath.Base(f), ".json")
		}
	}
	return ""
}

// rcHold makes the edge's proxy hold profile without changing any switch word
// (`ccmorph-rc <profile> --switch --rehand`): a no-op when it already holds it.
// A var so a test can stand in for the launcher.
var rcHold = func(ctx context.Context, profile, account string) error {
	rc, err := exec.LookPath("ccmorph-rc")
	if err != nil {
		home, _ := os.UserHomeDir()
		rc = filepath.Join(home, ".local", "bin", "ccmorph-rc") // where the add-on's install.sh links it
	}
	return exec.CommandContext(ctx, rc, profile, "--as", account, "--switch", "--rehand").Run()
}

// edgeWord is the switch word under which this edge session's proxy serves
// profile ("account", "route" or the profile's name), once the proxy holds it;
// ok is false outside an edge session or when the proxy cannot serve it.
func edgeWord(profile string, deadline time.Time) (string, bool) {
	port, ok := edgePort()
	if !ok {
		return "", false
	}
	account := filepath.Base(os.Getenv("CLAUDE_CONFIG_DIR"))
	switch profile {
	case account:
		return "account", true
	case edgeRoute(rcBridge(), port):
		return "route", true
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if rcHold(ctx, profile, account) != nil {
		return "", false
	}
	return profile, true
}

// promptKey names a pending pick: rc_route.prompt_key.
func promptKey(prompt string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(prompt)))
	return hex.EncodeToString(sum[:])
}

// leavePick leaves the pick for the agent whose prompt this is, where the proxy
// claims it on that agent's first request.
func leavePick(prompt, word, model string) error {
	dir := filepath.Join(rcBridge(), "agents", "pending")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"word": word, "model": model})
	tmp, err := os.CreateTemp(dir, ".pick.*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(b)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(dir, promptKey(prompt)+".json"))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// dropAgent removes a stopped agent's binding (rc_route.agent_target).
func dropAgent(agent string) {
	if agent != "" && !strings.ContainsAny(agent, `/\.`) {
		os.Remove(filepath.Join(rcBridge(), "agents", agent+".json"))
	}
}
