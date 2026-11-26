package cli

import (
	"crypto/tls"
	"crypto/x509"
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
	"time"

	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/router"
	"github.com/akleizor/ccmorph/util"
)

// agentHop is one session's agent proxy (package agentproxy): started for one
// Claude Code run and stopped when it exits, so what it knows (the session's
// endpoint, the targets it resolved) never outlives that run. A subagent the
// router picked on another profile goes to that profile, as Claude Code's own
// subagent.
type agentHop struct {
	p   *agentproxy.Proxy
	run string // its run dir: picks, the trust bundle, the Bash env file

	mu      sync.Mutex
	targets map[string]*target
	logMu   sync.Mutex
}

// target is one profile and model's resolution, made once: requests for it
// wait on the first, and a failure is kept a while so a broken profile is not
// retried by every request of a busy agent.
type target struct {
	once sync.Once
	t    agentproxy.Target
	err  error
	at   time.Time
}

const failureKept = time.Minute

var anthropicURL = &url.URL{Scheme: "https", Host: "api.anthropic.com"}

// sessionKeys are what the proxy changes in the session's env. Claude Code's
// Bash children get them back as they were (CLAUDE_ENV_FILE), so curl or
// Python in a session never meets the proxy or its CA.
var sessionKeys = []string{"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", "ANTHROPIC_BASE_URL", agentproxy.DirEnv}

// agentProxyEnv starts this run's agent proxy when the router is on and puts
// the session behind it. A session on Anthropic's own endpoint keeps it as
// its base URL and reaches it through the proxy as an HTTPS proxy; a session
// on any other endpoint gets the proxy as its base URL, which sends it on to
// that endpoint. Inside a Remote Control edge the edge's own proxy routes
// agents instead. The run before it, if any, has already ended: its proxy is
// stopped here.
func (l *launcher) agentProxyEnv(env []string) []string {
	l.agents.stop()
	l.agents = nil
	if !router.Enabled() || l.inEdge() {
		return env
	}
	get := func(k string) string {
		if v, ok := envGet(env, k); ok {
			return v
		}
		return os.Getenv(k)
	}
	o := agentproxy.Options{}
	base, _ := envGet(env, "ANTHROPIC_BASE_URL")
	b, native := nativeBase(base)
	if !native {
		u, err := url.Parse(b)
		if err != nil || u.Host == "" {
			return env
		}
		o.Origin = u
	}
	if up := get("HTTPS_PROXY"); up == "" {
		o.Upstream, _ = url.Parse(get("https_proxy"))
	} else {
		o.Upstream, _ = url.Parse(up)
	}
	if o.Upstream != nil && o.Upstream.Host == "" {
		o.Upstream = nil
	}
	extra := get("NODE_EXTRA_CA_CERTS")
	h, bundle, err := l.startAgentHop(o, extra)
	if err != nil {
		l.log("agent proxy not started: " + err.Error())
		return env
	}
	l.agents = h
	restore, err := h.bashEnv(env, get("CLAUDE_ENV_FILE"))
	if err != nil {
		l.log("agent proxy not started: " + err.Error())
		h.stop()
		l.agents = nil
		return env
	}
	addr := "http://127.0.0.1:" + strconv.Itoa(h.p.Port())
	if native {
		env = setEnv(env, "HTTPS_PROXY", addr)
		env = setEnv(env, "https_proxy", addr)
		env = setEnv(env, "NODE_EXTRA_CA_CERTS", bundle)
	} else {
		env = setEnv(env, "ANTHROPIC_BASE_URL", addr)
	}
	env = setEnv(env, "CLAUDE_ENV_FILE", restore)
	return setEnv(env, agentproxy.DirEnv, h.p.Dir)
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

// startAgentHop starts the proxy with o and writes the trust bundle the
// session gets: ccmorph's CA and the one the session already trusted.
func (l *launcher) startAgentHop(o agentproxy.Options, extra string) (*agentHop, string, error) {
	ca, err := agentproxy.LoadCA(filepath.Join(l.dir, "agent-proxy"))
	if err != nil {
		return nil, "", err
	}
	h := &agentHop{run: filepath.Join(l.dir, "run", "agent-proxy."+strconv.Itoa(l.launcherPID)), targets: map[string]*target{}}
	if err := os.MkdirAll(h.run, 0o700); err != nil {
		return nil, "", err
	}
	bundle := append([]byte{}, ca.PEM...)
	if extra != "" {
		if b, err := os.ReadFile(extra); err == nil {
			bundle = append(append(bundle, '\n'), b...)
			if pool, err := x509.SystemCertPool(); err == nil && pool.AppendCertsFromPEM(b) {
				o.TLS = &tls.Config{RootCAs: pool}
			}
		}
	}
	bundlePath := filepath.Join(h.run, "ca-bundle.pem")
	if err := os.WriteFile(bundlePath, bundle, 0o600); err != nil {
		os.RemoveAll(h.run)
		return nil, "", err
	}
	o.Dir = filepath.Join(h.run, "agents")
	o.Resolve = h.resolver(l)
	o.Logf = func(format string, args ...any) {
		h.logMu.Lock()
		defer h.logMu.Unlock()
		l.log(fmt.Sprintf(format, args...))
	}
	if h.p, err = agentproxy.Start(ca, o); err != nil {
		os.RemoveAll(h.run)
		return nil, "", err
	}
	o.Logf("agent proxy listening on 127.0.0.1:%d", h.p.Port())
	return h, bundlePath, nil
}

// bashEnv writes the file Claude Code sources before each Bash command: the
// session's own value of every key the proxy changed, after the person's own
// CLAUDE_ENV_FILE, if any.
func (h *agentHop) bashEnv(env []string, theirs string) (string, error) {
	var b strings.Builder
	if theirs != "" {
		fmt.Fprintf(&b, "[ -r %s ] && . %s\n", shellQuote(theirs), shellQuote(theirs))
	}
	for _, k := range sessionKeys {
		if v, ok := envGet(env, k); ok {
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
		} else if v, ok := os.LookupEnv(k); ok && k != agentproxy.DirEnv {
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
		} else {
			fmt.Fprintf(&b, "unset %s\n", k)
		}
	}
	path := filepath.Join(h.run, "bash-env.sh")
	return path, os.WriteFile(path, []byte(b.String()), 0o600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stop ends the proxy and removes its run dir. The adapters it started end
// with the Claude Code process they were started for.
func (h *agentHop) stop() {
	if h == nil {
		return
	}
	h.p.Close()
	os.RemoveAll(h.run)
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
	resolve := func(pick agentproxy.Pick) (agentproxy.Target, error) {
		typ, env, err := util.ProfileEnv(dir, profiles, pick.Word, func(string) {})
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
			if t.OAuth, _ = envGet(env, "CLAUDE_CODE_OAUTH_TOKEN"); t.OAuth == "" {
				return agentproxy.Target{}, errors.New("no OAuth token")
			}
			return t, nil
		}
		t.Key, _ = envGet(env, "ANTHROPIC_API_KEY")
		base, _ := envGet(env, "ANTHROPIC_BASE_URL")
		b, native := nativeBase(base)
		if t.Base, err = url.Parse(b); err != nil || t.Base.Host == "" {
			return agentproxy.Target{}, fmt.Errorf("profile %s has no usable base URL", pick.Word)
		}
		if native {
			return t, nil
		}
		pp := parent()
		if pp == 0 {
			return agentproxy.Target{}, errors.New("no Claude Code process")
		}
		port, err := startAdapter(core, pick.Word, pick.Model, pp, logPath)
		if err != nil {
			return agentproxy.Target{}, err
		}
		if port != "" { // else the endpoint serves this model as it is
			t.Base = &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
		}
		return t, nil
	}
	return func(pick agentproxy.Pick) (agentproxy.Target, error) {
		// A gateway's adapter is started for one model (its translator is the
		// model's), so a target is one profile and model.
		id := pick.Word + "\x00" + pick.Model
		h.mu.Lock()
		e := h.targets[id]
		if e == nil || (e.err != nil && time.Since(e.at) > failureKept) {
			e = &target{}
			h.targets[id] = e
		}
		h.mu.Unlock()
		e.once.Do(func() { e.t, e.err = resolve(pick); e.at = time.Now() })
		return e.t, e.err
	}
}

// startAdapter is `util adapter-start` for one profile and model: its port,
// "" when the profile serves that model natively.
func startAdapter(core, name, model string, parent int, logPath string) (string, error) {
	out, err := exec.Command(core, "util", "adapter-start", core, name, model, strconv.Itoa(parent), logPath).Output()
	if err != nil {
		return "", fmt.Errorf("adapter for %s did not start", name)
	}
	pid, port, native := 0, "", false
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
		if pid > 0 { // the one this call just started, which nothing needs
			syscall.Kill(pid, syscall.SIGTERM)
		}
		return "", nil
	}
	if port == "" {
		return "", fmt.Errorf("adapter for %s named no port", name)
	}
	return port, nil
}
