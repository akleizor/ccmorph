package router

import (
	"context"
	"encoding/json"
	"github.com/akleizor/ccmorph/agentproxy"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// A Remote Control edge session sends its inference through the edge's proxy,
// which can serve any profile it holds. So a subagent picked on another profile
// still runs as Claude Code's own subagent: the hook leaves the pick for the
// proxy, and the proxy sends that one agent's requests to the picked profile
// (agentproxy.Claim holds the other half of this protocol).

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
		rc = filepath.Join(paths.Home(), ".local", "bin", "ccmorph-rc") // where the add-on's install.sh links it
	}
	cmd := exec.CommandContext(ctx, rc, profile, "--as", account, "--switch", "--rehand")
	// The deadline asks ccmorph-rc to stop (it finishes a handoff it is publishing
	// on TERM) and kills it only after rcWaitDelay, inside the hook's headroom.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = rcWaitDelay
	return cmd.Run()
}

// rcWaitDelay is how long ccmorph-rc may finish after TERM. edgeWord sends TERM
// this long before the hook's deadline, so the wait fits inside hookBudget.
const rcWaitDelay = 3 * time.Second

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
	ctx, cancel := context.WithDeadline(context.Background(), deadline.Add(-rcWaitDelay))
	defer cancel()
	if rcHold(ctx, profile, account) != nil {
		return "", false
	}
	return profile, true
}

// promptKey names a pending pick: agentproxy.PromptKey.
func promptKey(prompt string) string { return agentproxy.PromptKey(prompt) }

func edgeAgents() string { return filepath.Join(rcBridge(), "agents") }

// leavePick leaves the pick for the agent whose prompt this is, in its session's
// folder, where the proxy claims it on that agent's first request.
func leavePick(session, prompt, word, model string) error {
	return agentproxy.LeavePick(edgeAgents(), session, prompt, agentproxy.Pick{Word: word, Model: model})
}

// proxyServes is whether the launcher's agent proxy can carry profile's sign-in:
// a subscription sign-in or token, an API key, a gateway or a provider
// (cli.agentProxyServes keeps the same list).
func proxyServes(profile string) bool {
	path, err := ctxwindow.ProfilePath(profile)
	if err != nil {
		return false
	}
	_, f, err := ctxwindow.ReadConf(path)
	if err != nil {
		return false
	}
	switch f["type"] {
	case "token", "apikey", "proxy", "provider", "sub":
		return true
	}
	return false
}
