package cli

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/router"
	"github.com/akleizor/ccmorph/util"
)

// agentHop is the launcher's agent proxy (package agentproxy): it lives as long
// as this launcher, so every session the launcher runs can send a subagent the
// router picked on another profile to that profile, as Claude Code's own
// subagent. What it starts to serve a pick (a gateway's adapter) it stops.
type agentHop struct {
	p      *agentproxy.Proxy
	bundle string // the CA file the session trusts: ccmorph's CA and any it already trusted

	mu       sync.Mutex
	targets  map[string]agentproxy.Target
	adapters []int
	logMu    sync.Mutex
}

var anthropicURL = &url.URL{Scheme: "https", Host: "api.anthropic.com"}

// agentProxyEnv puts the session behind the launcher's agent proxy when the
// router is on. A session on Anthropic's own endpoint keeps it as its base URL
// and reaches it through the proxy as an HTTPS proxy; a session on any other
// endpoint gets the proxy as its base URL, which sends it on to that endpoint.
// Inside a Remote Control edge the edge's own proxy routes agents instead.
func (l *launcher) agentProxyEnv(env []string) []string {
	if !router.Enabled() || l.inEdge() {
		return env
	}
	if l.agents == nil {
		h, err := l.startAgentHop()
		if err != nil {
			l.log("agent proxy not started: " + err.Error())
			return env
		}
		l.agents = h
	}
	port := strconv.Itoa(l.agents.p.Port())
	base, _ := envGet(env, "ANTHROPIC_BASE_URL")
	l.agents.p.Origin = nil
	if b, native := nativeBase(base); !native {
		u, err := url.Parse(b)
		if err != nil || u.Host == "" {
			return env
		}
		l.agents.p.Origin = u
		env = setEnv(env, "ANTHROPIC_BASE_URL", "http://127.0.0.1:"+port)
	} else {
		proxy := "http://127.0.0.1:" + port
		env = setEnv(env, "HTTPS_PROXY", proxy)
		env = setEnv(env, "https_proxy", proxy)
		env = setEnv(env, "NODE_EXTRA_CA_CERTS", l.agents.bundle)
	}
	return setEnv(env, agentproxy.DirEnv, l.agents.p.Dir)
}

func setEnv(env []string, key, val string) []string {
	out := env[:0:0]
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return append(out, key+"="+val)
}

func (l *launcher) startAgentHop() (*agentHop, error) {
	ca, err := agentproxy.LoadCA(filepath.Join(l.dir, "agent-proxy"))
	if err != nil {
		return nil, err
	}
	run := filepath.Join(l.dir, "run", "agent-proxy."+strconv.Itoa(l.launcherPID))
	h := &agentHop{targets: map[string]agentproxy.Target{}}
	bundle := append([]byte{}, ca.PEM...)
	if extra := os.Getenv("NODE_EXTRA_CA_CERTS"); extra != "" {
		if b, err := os.ReadFile(extra); err == nil {
			bundle = append(append(bundle, '\n'), b...)
		}
	}
	if err := os.MkdirAll(run, 0o700); err != nil {
		return nil, err
	}
	h.bundle = filepath.Join(run, "ca-bundle.pem")
	if err := os.WriteFile(h.bundle, bundle, 0o600); err != nil {
		return nil, err
	}
	logf := func(format string, args ...any) {
		h.logMu.Lock()
		defer h.logMu.Unlock()
		l.log(fmt.Sprintf(format, args...))
	}
	h.p, err = agentproxy.Start(filepath.Join(run, "agents"), ca, h.resolver(l), logf)
	if err != nil {
		os.RemoveAll(run)
		return nil, err
	}
	logf("agent proxy listening on 127.0.0.1:%d", h.p.Port())
	return h, nil
}

// stop ends the proxy and every adapter it started, and removes its run dir.
func (h *agentHop) stop() {
	if h == nil {
		return
	}
	h.p.Close()
	h.mu.Lock()
	for _, pid := range h.adapters {
		syscall.Kill(pid, syscall.SIGTERM)
	}
	h.adapters = nil
	h.mu.Unlock()
	os.RemoveAll(filepath.Dir(h.p.Dir))
}

// errNotServed: a profile whose sign-in the proxy cannot carry yet.
var errNotServed = errors.New("not served by the agent proxy")

// agentProxyServes is whether the agent proxy can serve a profile of this type:
// one the hook may leave a pick for. It reads only the launcher's own copy of
// the profile, never the session's env.
func agentProxyServes(typ string) bool { return typ == "token" || typ == "apikey" || typ == "proxy" }

// resolver is where a pick's profile is served. Only the launcher's state is
// read, never a method that changes it: this runs on the proxy's goroutines.
func (h *agentHop) resolver(l *launcher) func(agentproxy.Pick) (agentproxy.Target, error) {
	dir, profiles, core, logPath := l.dir, l.profileDirs, filepath.Join(l.libexec, "ccmorph-core"), l.logOrNull()
	// An agent's adapter lives as long as the Claude Code process that asked
	// for it, never named by the launcher's pid: that is how `ccmorph hop`
	// finds the session's own gateway (util.SessionAdapter).
	parent := func() int {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.child != nil {
			return l.child.Pid
		}
		return 0
	}
	return func(pick agentproxy.Pick) (agentproxy.Target, error) {
		word := pick.Word
		h.mu.Lock()
		defer h.mu.Unlock()
		// A gateway's adapter is started for one model (its translator is the
		// model's), so a target is one profile and model.
		id := word + "\x00" + pick.Model
		if t, ok := h.targets[id]; ok {
			return t, nil
		}
		typ, env, err := util.ProfileEnv(dir, profiles, word, func(string) {})
		if err != nil {
			return agentproxy.Target{}, err
		}
		if !agentProxyServes(typ) {
			return agentproxy.Target{}, errNotServed
		}
		t := agentproxy.Target{Base: anthropicURL, Tiers: map[string]string{}}
		for _, tier := range util.TierWords {
			if v, ok := envGet(env, util.TierEnvKey(tier)); ok && v != "" {
				t.Tiers[tier] = v
			}
		}
		if typ == "token" {
			t.OAuth, _ = envGet(env, "CLAUDE_CODE_OAUTH_TOKEN")
			if t.OAuth == "" {
				return agentproxy.Target{}, errors.New("no OAuth token")
			}
		} else {
			t.Key, _ = envGet(env, "ANTHROPIC_API_KEY")
			base, _ := envGet(env, "ANTHROPIC_BASE_URL")
			if _, native := nativeBase(base); !native {
				pp := parent()
				if pp == 0 {
					return agentproxy.Target{}, errors.New("no Claude Code process")
				}
				port, pid, err := startAdapter(core, word, pick.Model, pp, logPath)
				if err != nil {
					return agentproxy.Target{}, err
				}
				if pid > 0 {
					h.adapters = append(h.adapters, pid)
				}
				if port != "" {
					t.Base = &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
				}
			}
		}
		h.targets[id] = t
		return t, nil
	}
}

// startAdapter is `util adapter-start` for one profile and model: its port,
// "" when the profile serves that model natively.
func startAdapter(core, name, model string, parent int, logPath string) (port string, pid int, err error) {
	out, err := exec.Command(core, "util", "adapter-start", core, name, model, strconv.Itoa(parent), logPath).Output()
	if err != nil {
		return "", 0, fmt.Errorf("adapter for %s did not start", name)
	}
	native := false
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch {
		case k == "PID":
			pid, _ = strconv.Atoi(v)
		case k == "PORT":
			port = v
		case line == "NATIVE=1":
			native = true
		}
	}
	if native {
		if pid > 0 {
			syscall.Kill(pid, syscall.SIGTERM)
		}
		return "", 0, nil
	}
	if port == "" {
		return "", pid, fmt.Errorf("adapter for %s named no port", name)
	}
	return port, pid, nil
}
