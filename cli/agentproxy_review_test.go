package cli

import (
	"net/url"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/router"
	"github.com/akleizor/ccmorph/util"
)

// The proxy turns the tether off for a routed session as the edge path does,
// and its run dir goes with the proxy.
func TestAProxiedSessionTurnsTetherLiveOffAndItsRunDirGoesWithIt(t *testing.T) {
	state := t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", state)
	t.Setenv("CCMORPH_EDGE_ROUTE_ENV", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NODE_EXTRA_CA_CERTS", "")
	if err := router.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	l := &launcher{dir: state, launcherPID: os.Getpid()}
	env := l.agentProxyEnv([]string{"CLAUDE_CODE_OAUTH_TOKEN=x"})
	h := l.swapAgents(nil)
	if h == nil {
		t.Fatalf("the proxy did not start: %v", env)
	}
	defer h.stop()
	if v, _ := envGet(env, "CLAUDE_CODE_TETHER_LIVE"); v != "0" {
		t.Fatalf("CLAUDE_CODE_TETHER_LIVE = %q, want 0", v)
	}
	if v, _ := envGet(env, "HTTPS_PROXY"); v == "" {
		t.Fatalf("a native session is behind the proxy: %v", env)
	}
	h.stop()
	if _, err := os.Stat(h.run); !os.IsNotExist(err) {
		t.Fatalf("run dir stays: %v", err)
	}
	if err := router.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if got := l.agentProxyEnv([]string{"A=b"}); len(got) != 1 {
		t.Fatalf("router off changes env: %v", got)
	}
}

// A signal that stops the helpers while the proxy is still starting finds the
// run dir: it is registered as soon as it exists.
func TestTheRunDirIsRegisteredBeforeTheProxyStarts(t *testing.T) {
	state := t.TempDir()
	l := &launcher{dir: state, launcherPID: 4242}
	h, _, err := l.startAgentHop(agentproxy.Options{}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.stop()
	if got := l.swapAgents(nil); got != h {
		t.Fatalf("registered hop = %v, want %v", got, h)
	}
	l.swapAgents(h)
	l.stopHelpers()
	if _, err := os.Stat(h.run); !os.IsNotExist(err) {
		t.Fatalf("stopHelpers leaves the run dir: %v", err)
	}
}

// Concurrent requests for one pick, failing and resolved again, share the
// cache without a data race (run under -race).
func TestConcurrentResolvesShareTheCacheWithoutARace(t *testing.T) {
	l, h, _ := proxyFixture(t, map[string]string{"gw": "type=provider\nprovider=codex\nmodel=m1\n"})
	resolve := h.resolver(l)
	for range 20 {
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resolve(agentproxy.Pick{Word: "gw", Model: "m1"}) // no sign-in: a refusal, kept
			}()
		}
		wg.Wait()
		h.mu.Lock()
		for _, e := range h.targets {
			e.at = time.Now().Add(-2 * failureKept) // expired: the next round resolves it again
		}
		h.mu.Unlock()
	}
}

// A cached target whose adapter has exited is resolved again, not served to
// fail every later request.
func TestATargetWhoseAdapterDiedIsResolvedAgain(t *testing.T) {
	l, h, _ := proxyFixture(t, map[string]string{"gw": "type=provider\nprovider=codex\nmodel=m1\n"})
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	id := "gw\x00m1"
	stale := &target{t: agentproxy.Target{Base: &url.URL{Scheme: "http", Host: "127.0.0.1:1"}}, sa: &util.SharedAdapter{PID: dead.Process.Pid}}
	stale.once.Do(func() {})
	h.targets[id] = stale
	tg, err := h.resolver(l)(agentproxy.Pick{Word: "gw", Model: "m1"})
	if err == nil || tg.Base != nil {
		t.Fatalf("the dead adapter's target was served: %+v %v", tg, err)
	}
}
