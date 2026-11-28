package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/quota"
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

	mu       sync.Mutex
	targets  map[string]*target
	bridges  []*util.SharedBridge  // provider bridges this session holds a use of
	auth     []string              // each one's auth dir, for its release
	adapters []*util.SharedAdapter // gateway adapters this session holds a use of
	pids     map[string]int        // each target's adapter pid: a dead one evicts the cached target
	logMu    sync.Mutex
}

// target is one profile and model's resolution, made once: requests for it
// wait on the first, and a failure is kept a while so a broken profile is not
// retried by every request of a busy agent.
type target struct {
	once sync.Once
	t    agentproxy.Target
	err  error
	at   time.Time
	sub  bool // a subscription token, which its own sessions refresh in place
}

// adapterGone: the adapter a cached target names has exited, so the target is
// resolved again (a new adapter) instead of failing every later request.
func adapterGone(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == syscall.ESRCH }

const failureKept = time.Minute

// subTokenKept is how long a subscription token read stands: the account's
// own Claude Code sessions refresh the access token in the keychain, so a
// long agent run reads it again rather than send an expired one.
const subTokenKept = 5 * time.Minute

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
	l.swapAgents(nil).stop()
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
	restore, err := h.bashEnv(env, get("CLAUDE_ENV_FILE"))
	if err != nil {
		l.log("agent proxy not started: " + err.Error())
		l.swapAgents(nil).stop()
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
	// The proxy cannot serve a tether `thread: continue` request on a routed agent,
	// so the session turns it off as the edge path does (edgeEnv).
	env = setEnv(env, "CLAUDE_CODE_TETHER_LIVE", "0")
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
	// Registered before the proxy starts, so a TERM mid-start still removes the run dir.
	l.swapAgents(h)
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
		l.swapAgents(nil)
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
	p, err := agentproxy.Start(ca, o)
	if err != nil {
		l.swapAgents(nil)
		os.RemoveAll(h.run)
		return nil, "", err
	}
	h.mu.Lock()
	h.p = p
	h.mu.Unlock()
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
		} else if v, ok := os.LookupEnv(k); ok && k != agentproxy.DirEnv && !slices.Contains(profile.BackendEnv, k) {
			// A backend key absent from the launch env is one `util claude-session`
			// cleared before exec: the session never had the launcher's value.
			fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(v))
		} else {
			fmt.Fprintf(&b, "unset %s\n", k)
		}
	}
	path := filepath.Join(h.run, "bash-env.sh")
	return path, os.WriteFile(path, []byte(b.String()), 0o600)
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// stop ends the proxy and removes its run dir, and lets go of the provider
// bridges and gateway adapters it joined: the last user out stops one.
func (h *agentHop) stop() {
	if h == nil {
		return
	}
	h.mu.Lock()
	p := h.p
	bridges, auth := h.bridges, h.auth
	adapters := h.adapters
	h.bridges, h.auth, h.adapters = nil, nil, nil
	h.mu.Unlock()
	if p != nil {
		p.Close()
	}
	for _, a := range adapters {
		a.Release()
	}
	for i, b := range bridges {
		b.Release(auth[i])
	}
	os.RemoveAll(h.run)
}

// errNotServed: a profile whose sign-in the proxy cannot carry yet.
var errNotServed = errors.New("not served by the agent proxy")

// agentProxyServes is whether the agent proxy can serve a profile of this type:
// one the hook may leave a pick for. It reads only the launcher's own copy of
// the profile, never the session's env.
func agentProxyServes(typ string) bool {
	return typ == "token" || typ == "apikey" || typ == "proxy" || typ == "provider" || typ == "sub"
}

// resolver is where a pick's profile is served. Only the launcher's state is
// read, never a method that changes it: this runs on the proxy's goroutines.
func (h *agentHop) resolver(l *launcher) func(agentproxy.Pick) (agentproxy.Target, error) {
	dir, profiles, core, logPath := l.dir, l.profileDirs, filepath.Join(l.libexec, "ccmorph-core"), l.logOrNull()
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
		switch typ {
		case "sub":
			tok, err := subToken(filepath.Join(profiles, pick.Word), quota.SubscriptionToken, l.claudeAuthCtx)
			if err != nil {
				return agentproxy.Target{}, err
			}
			t.OAuth = tok
			return t, nil
		case "provider":
			// The profile's bridge, joined or started as its own launch does,
			// and held for this session until it ends.
			b, err := acquireBridge(dir, filepath.Dir(core), logPath, pick.Word, io.Discard)
			if err != nil {
				return agentproxy.Target{}, err
			}
			auth := filepath.Join(dir, "auth", pick.Word)
			h.mu.Lock()
			h.bridges, h.auth = append(h.bridges, b), append(h.auth, auth)
			h.mu.Unlock()
			t.Base, t.Key = &url.URL{Scheme: "http", Host: "127.0.0.1:" + b.Port}, b.Key
			return t, nil
		case "token":
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
		// The adapter is the one every session of this profile and model shares,
		// held for this session until it ends. It is a subagent's route, not the
		// profile this session runs on, so its hold must never answer `ccmorph hop`
		// for the session (#1022).
		sa, ok := util.AcquireAdapterForSubagent(core, filepath.Join(dir, "auth"), pick.Word, pick.Model, filepath.Join(dir, pick.Word+".conf"), io.Discard)
		if !ok {
			return agentproxy.Target{}, fmt.Errorf("adapter for %s did not start", pick.Word)
		}
		port := sa.Port
		if sa.Native {
			port = ""
		} else if port == "" {
			sa.Release()
			return agentproxy.Target{}, fmt.Errorf("adapter for %s named no port", pick.Word)
		} else {
			h.mu.Lock()
			h.adapters = append(h.adapters, sa)
			if h.pids == nil {
				h.pids = map[string]int{}
			}
			h.pids[pick.Word+"\x00"+pick.Model] = sa.PID
			h.mu.Unlock()
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
		if e == nil || (e.err != nil && time.Since(e.at) > failureKept) || (e.sub && time.Since(e.at) > subTokenKept) || adapterGone(h.pids[id]) {
			delete(h.pids, id)
			e = &target{}
			h.targets[id] = e
		}
		h.mu.Unlock()
		e.once.Do(func() {
			t, err := resolve(pick)
			h.mu.Lock()
			e.t, e.err, e.at, e.sub = t, err, time.Now(), t.OAuth != ""
			h.mu.Unlock()
		})
		h.mu.Lock()
		t, err := e.t, e.err
		h.mu.Unlock()
		return t, err
	}
}

// subToken is the access token of a Claude account's sign-in, as its own launch
// would find it: read from the keychain item Claude Code keeps for the config
// dir. An expired one is refreshed by Claude Code itself (`claude auth status`
// renews the sign-in as any launch does, under its own lock), never by a token
// call of ccmorph's: a second refresh would spend the single-use refresh token
// under the sessions that hold it.
func subToken(dir string, read func(string) (string, string, error), auth func(context.Context, string, ...string) *exec.Cmd) (string, error) {
	tok, _, err := read(dir)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		return tok, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := auth(ctx, dir, "status")
	cmd.WaitDelay = time.Second
	cmd.Run()
	tok, _, err = read(dir)
	return tok, err
}
