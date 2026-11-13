// Package quota is `ccmorph quota`: one view of every profile's plan windows
// and spend. Claude account usage is read from claude.ai's usage endpoint (the
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
	"strings"
	"sync"
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
// cliproxyapi auth dir) or "gateway" (arg is the base URL, extra is
// monthly_budget_usd; either may be empty, and a fifth field carries an API key).
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
	var lines [][]string
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) < 3 || f[0] == "" {
			continue
		}
		for len(f) < 5 {
			f = append(f, "")
		}
		lines = append(lines, f)
	}
	// Each profile's read waits on its own endpoint or on the receipts, never on
	// another's, so they run at once and the call takes as long as the slowest.
	entries := make([]Entry, len(lines))
	var wg sync.WaitGroup
	for i, f := range lines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := read(client, f[0], f[1], f[2], f[3])
			if f[1] == "gateway" {
				e.Balance, e.Error = balance(client, f[2], f[4], e.Error)
			}
			entries[i] = e
		}()
	}
	wg.Wait()
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
			// The same command cred and login name: a missing sign-in has a
			// working fix, and quota is where a person first sees the state.
			msg := err.Error()
			if msg == "not signed in" {
				msg = "not signed in — sign in again: ccmorph login " + name
			}
			return Entry{Profile: name, Kind: "claude", Plan: plan, Windows: []Window{}, Error: msg}
		}
		return readClaude(client, name, token, plan, false)
	case "token":
		if extra == "" {
			return Entry{Profile: name, Kind: "claude", Windows: []Window{}, Error: "not signed in"}
		}
		// The token's read is still sent, but its row does not follow the
		// endpoint's answer: anything but a usage reply is tokenNoPlanUsage (#270).
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

// usageVerdict is the status ladder both usage readers walk: the row for a
// read that did not come back as usage — unreachable, refused sign-in,
// rate-limited, or an error status — with the cached reading standing in
// where one exists (stale). ok is true only when the reply is usage the
// caller goes on to parse; e carries the profile, kind and plan the row keeps
// beside the verdict, so a reader whose plan comes from the reply passes an
// entry without one.
func usageVerdict(e Entry, status int, err error) (Entry, bool) {
	switch {
	case err != nil:
		return stale(e.Profile, e.Kind, e.Plan, "unreachable", "not read yet: usage endpoint unreachable"), false
	case status == 401 || status == 403:
		e.Error = "sign-in refused; start a session on it to sign in"
		return e, false
	case status == 429:
		return stale(e.Profile, e.Kind, e.Plan, "rate-limited", "not read yet: usage endpoint is rate-limiting"), false
	case status != 200:
		if status >= 500 {
			return stale(e.Profile, e.Kind, e.Plan, "endpoint error", fmt.Sprintf("not read yet: usage endpoint answered %d", status)), false
		}
		e.Error = fmt.Sprintf("not read yet: usage endpoint answered %d", status)
		return e, false
	}
	return e, true
}
