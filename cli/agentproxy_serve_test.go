package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/akleizor/ccmorph/agentproxy"
	"github.com/akleizor/ccmorph/util"
)

func TestAgentProxyServesProvidersAndAccounts(t *testing.T) {
	for typ, want := range map[string]bool{"token": true, "apikey": true, "proxy": true, "provider": true, "sub": true, "bogus": false, "": false} {
		if got := agentProxyServes(typ); got != want {
			t.Errorf("agentProxyServes(%q) = %v, want %v", typ, got, want)
		}
	}
}

// proxyFixture is a launcher whose state dir holds the named profiles, with a
// stand-in cliproxyapi on PATH and a core whose `util bridge-start` answers a
// fixed handle and counts its calls.
func proxyFixture(t *testing.T, confs map[string]string) (*launcher, *agentHop, string) {
	t.Helper()
	dir, libexec, bin := t.TempDir(), t.TempDir(), t.TempDir()
	for name, body := range confs {
		if err := os.WriteFile(filepath.Join(dir, name+".conf"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	count := filepath.Join(libexec, "starts")
	core := "#!/bin/sh\n[ \"$2\" = bridge-start ] || exit 1\necho x >> '" + count + "'\n" +
		"printf 'PID=%s\\nPORT=4242\\nKEY=k-bridge\\nTMPDIR=%s\\n' $$ '" + libexec + "/ccmorph-bridge-x'\n"
	if err := os.WriteFile(filepath.Join(libexec, "ccmorph-core"), []byte(core), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "cliproxyapi"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	l := &launcher{dir: dir, libexec: libexec, profileDirs: filepath.Join(dir, "profiles")}
	return l, &agentHop{targets: map[string]*target{}}, count
}

// usersHeld: another open file holds the bridge's users lock.
func usersHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

func TestAProviderPickJoinsItsBridgeOnceAndLetsGoWithTheSession(t *testing.T) {
	l, h, count := proxyFixture(t, map[string]string{"gw": "type=provider\nprovider=codex\nmodel=m1\n"})
	if err := os.MkdirAll(filepath.Join(l.dir, "auth", "gw"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolve := h.resolver(l)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tg, err := resolve(agentproxy.Pick{Word: "gw", Model: "m1"})
			if err != nil || tg.Base.String() != "http://127.0.0.1:4242" || tg.Key != "k-bridge" || tg.OAuth != "" {
				t.Errorf("provider target: %+v %v", tg, err)
			}
		}()
	}
	wg.Wait()
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Fatalf("the bridge starts once for concurrent requests: %q", b)
	}
	users := util.BridgeFiles(filepath.Join(l.dir, "auth"), "gw")[2]
	if !usersHeld(users) {
		t.Fatal("the session holds a use of the bridge while it runs")
	}
	ca, err := agentproxy.LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	if h.p, err = agentproxy.Start(ca, agentproxy.Options{Dir: filepath.Join(t.TempDir(), "agents"), Resolve: resolve}); err != nil {
		t.Fatal(err)
	}
	h.run = t.TempDir()
	h.stop()
	if usersHeld(users) {
		t.Fatal("the session's use of the bridge ends with it")
	}
}

func TestAProviderPickWithoutASignInIsRefusedInTheLaunchsWords(t *testing.T) {
	l, h, _ := proxyFixture(t, map[string]string{"gw": "type=provider\nprovider=codex\nmodel=m1\n"})
	_, err := h.resolver(l)(agentproxy.Pick{Word: "gw", Model: "m1"})
	if err == nil || !strings.Contains(err.Error(), "ccmorph login gw") {
		t.Fatalf("no auth dir: %v", err)
	}
}

func TestASubPickReadsTheAccountsTokenAndAnExpiredOneIsRenewedByClaudeCode(t *testing.T) {
	expired := errors.New("sign-in expired; start a session on it to refresh")
	tok, renewed := "", 0
	read := func(string) (string, string, error) {
		if renewed == 0 {
			return "", "Max", expired
		}
		return tok, "Max", nil
	}
	auth := func(ctx context.Context, dir string, args ...string) *exec.Cmd {
		if len(args) != 1 || args[0] != "status" {
			t.Errorf("the renewal asks Claude Code for its status: %v", args)
		}
		renewed++
		tok = "fresh-token"
		return exec.CommandContext(ctx, "true")
	}
	got, err := subToken("/p/alex", read, auth)
	if err != nil || got != "fresh-token" || renewed != 1 {
		t.Fatalf("expired token: %q %v after %d renewals", got, err, renewed)
	}
	renewed = 0
	read = func(string) (string, string, error) { return "live", "Max", nil }
	if got, err := subToken("/p/alex", read, auth); err != nil || got != "live" || renewed != 0 {
		t.Fatalf("a live token is left alone: %q %v", got, err)
	}
	read = func(string) (string, string, error) { return "", "", errors.New("not signed in") }
	if _, err := subToken("/p/alex", read, auth); err == nil || renewed != 0 {
		t.Fatalf("no sign-in is not renewed: %v", err)
	}
}
