// Package quota is `ccmorph quota`: one view of every profile's plan windows
// and spend. Claude account usage is read from claude.ai's usage endpoint (the
// one Claude Code's /usage reads) with the profile's own sign-in; Codex
// subscriptions from Codex's own usage endpoint with the cliproxyapi sign-in;
// gateways and API keys from the receipts their turns already wrote. A gateway's
// own endpoint is asked whether it answers, so a dead gateway's receipts are
// never printed as a month that was free (#726). A GET to a
// usage endpoint: no inference, nothing billed. A sign-in whose access token has
// expired is reported, never refreshed here: refreshing would rotate the token
// under the Claude Code sessions that hold it.
package quota

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/util"
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
// (arg is the config dir), "token" (extra is the token), "codex" or
// "antigravity" (arg is the cliproxyapi auth dir) or "gateway" (arg is the base URL, extra is
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
				// The endpoint is asked whether it answers at all, so a dead
				// gateway's receipts stop answering as a healthy zero (#726).
				e.Checked = endpointChecked(client, f[2], f[4])
				e.Balance, e.Error = balance(client, f[2], f[4], e.Error)
			}
			entries[i] = e
		}()
	}
	wg.Wait()
	// A spend entry that carries a reason and no spend is a store the run could
	// not read whole (#545): the view says so, and the run fails rather than
	// answer a healthy zero. Other reasons keep the run's exit — a refused
	// sign-in or an unreachable endpoint is one profile's state, and the rest of
	// the view still read.
	unread := false
	for _, e := range entries {
		// A spend that skipped receipts it could not read is still a store
		// not read whole: it carries the rest's total, and the run fails (#654).
		gap := e.Spend == nil || e.Spend.UnreadableReceipts > 0
		if gap && e.Error != "" && (e.Kind == "gateway" || e.Kind == "budget") {
			unread = true
		}
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
		if unread {
			return 1
		}
		return 0
	}
	renderText(stdout, entries, color)
	if unread {
		return 1
	}
	return 0
}

// read dispatches one stdin line. A gateway line's spend is read from the
// receipts alone; the endpoint is asked only beside that, in Main, and only
// whether it answers (endpointChecked) or what a balance costs (balance.go).
func read(client *http.Client, name, kind, arg, extra string) Entry {
	switch kind {
	case "sub":
		token, plan, err := SubscriptionToken(arg)
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
	case "antigravity":
		return readAntigravity(client, name, arg)
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

// probeWait is the wait a gateway gets for one answer, the 5 seconds doctor's
// route check gives it (util.ProbeListing's own bound), so quota never hangs
// longer over a dead endpoint than the command that diagnoses it.
const probeWait = 5 * time.Second

// endpointChecked asks the gateway's own endpoint whether it answers, the way
// doctor's route check does: the model list a launch reads first, sent with the
// profile's own key, and read here for nothing but whether an answer came —
// any answer, even a refusal or a redirect, proves the endpoint is there, and
// only silence says it is not. A free GET, no inference, nothing billed. nil
// says the line has no gateway to ask: no endpoint, no key to offer, or
// Anthropic's own base, where the receipts are the whole answer and doctor's
// key check is the place a native key gets checked (#726).
//
// The probe answers once and goes nowhere else: a redirect is refused, so the
// key is carried to exactly the one host the profile's base URL names. Go
// strips Authorization when a redirect crosses hosts, but not a custom header
// like x-api-key, and following a redirect would hand the profile's credential
// to whoever the endpoint names. doctor's curl probe sends its request exactly
// once too (no -L), so the two probes ask the same single question.
func endpointChecked(client *http.Client, base, key string) *bool {
	if key == "" || base == "" || util.NativeBase(base) {
		return nil
	}
	if u, err := url.Parse(base); err != nil || u.Host == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ctxwindow.APIURL(base, "/v1/models?limit=1"), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Authorization", "Bearer "+key)
	probe := *client
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, _, err = fetch(&probe, req)
	checked := err == nil
	return &checked
}
