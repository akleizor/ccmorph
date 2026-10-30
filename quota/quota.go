// Package quota is `ccmorph quota`: one view of every profile's plan windows
// and spend. Claude subscriptions are read from claude.ai's usage endpoint (the
// one Claude Code's /usage reads) with the profile's own sign-in; Codex
// subscriptions from Codex's own usage endpoint with the cliproxyapi sign-in;
// gateways and API keys from the receipts their turns already wrote. A GET to a
// usage endpoint: no inference, nothing billed. A sign-in whose access token has
// expired is reported, never refreshed here: refreshing would rotate the token
// under the Claude Code sessions that hold it.
package quota

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usageURL and transport are swapped by tests. The transport keeps the tests
// in-process: quota never needs a listening port to be verified.
var (
	usageURL  = "https://api.anthropic.com/api/oauth/usage"
	transport http.RoundTripper
	now       = time.Now
)

// Main reads one profile per stdin line,
// "name<TAB>kind<TAB>arg<TAB>extra", and prints one entry each. kind is "sub"
// (arg is the config dir), "token" (extra is the token), "codex" (arg is the
// cliproxyapi auth dir) or "gateway" (extra is monthly_budget_usd, may be empty).
func Main(argv []string, stdin io.Reader, stdout, stderr io.Writer) int {
	jsonOut, color := false, false
	for _, a := range argv {
		switch a {
		case "--json":
			jsonOut = true
		case "--color":
			color = true
		default:
			fmt.Fprintln(stderr, "usage: ccmorph-core quota [--json] [--color] < profiles")
			return 2
		}
	}
	client := httpClient()
	sc := bufio.NewScanner(stdin)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	entries := []Entry{}
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		for len(f) < 4 {
			f = append(f, "")
		}
		entries = append(entries, read(client, f[0], f[1], f[2], f[3]))
	}
	if len(entries) == 0 {
		if jsonOut {
			fmt.Fprintln(stdout, `{"profiles":[]}`)
			return 0
		}
		fmt.Fprintln(stdout, "No profiles.")
		return 0
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(struct {
			Profiles []Entry `json:"profiles"`
		}{entries}); err != nil {
			fmt.Fprintln(stderr, "ccmorph-core quota:", err)
			return 1
		}
		return 0
	}
	renderText(stdout, entries, color)
	return 0
}

// read dispatches one stdin line. A gateway line never reaches the network.
func read(client *http.Client, name, kind, arg, extra string) Entry {
	switch kind {
	case "sub":
		token, plan, err := subscriptionToken(arg)
		if err != nil {
			return Entry{Profile: name, Kind: "claude", Plan: plan, Windows: []Window{}, Error: err.Error()}
		}
		return readClaude(client, name, token, plan, false)
	case "token":
		if extra == "" {
			return Entry{Profile: name, Kind: "claude", Windows: []Window{}, Error: "not signed in"}
		}
		return readClaude(client, name, extra, "", true)
	case "codex":
		return readCodex(client, name, arg)
	default:
		return spendEntry(name, extra)
	}
}

// fetch performs one GET and returns its body and status.
func fetch(client *http.Client, req *http.Request) ([]byte, int, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, nil
}

// stateDir is where receipts and the quota cache live, the same root
// shim/sidecar.go uses.
func stateDir() string {
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return filepath.Join(root, "ccmorph")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccmorph")
}
