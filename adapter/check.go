package adapter

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/netroute"
	"github.com/akleizor/ccmorph/ordjson"
)

// CheckResult is one `ccmorph check` answer; its JSON is the --json contract.
type CheckResult struct {
	OK     bool   `json:"ok"`
	Route  string `json:"route"`
	Status int    `json:"status"`
	MS     int64  `json:"ms"`
	Note   string `json:"note"`
}

// Routes is every route a check may try, in dialectOf's order: the one needing
// no translation first.
var Routes = []string{"anthropic", "responses", "chat"}

// CachedRoute is the route the classification cache settled for model on base,
// "" when the cache holds no usable verdict for it.
func CachedRoute(base, profileDirs, model string) string {
	upstream := strings.TrimRight(base, "/")
	for _, r := range cachedRows(upstream, cachePath(profileDirs, upstream)) {
		if r.Model != model {
			continue
		}
		switch r.Dialect {
		case "anthropic", "responses", "chat":
			return r.Dialect
		case "chat-noreason":
			return "chat" // a check sends no reasoning field anyway
		}
	}
	return ""
}

// Check asks model the smallest real question on each route in turn: one "hi",
// one output token (16 on Responses, its floor), no tools. It stops at the
// first answer, at a refusal that holds for every route (402, 429, 5xx), or at
// no answer at all, so each route is asked at most once. oauth sends a Claude
// subscription's headers. proxy is the profile's CCMORPH_UPSTREAM_PROXY. The
// note never carries secret.
func Check(base, secret, model, proxy string, oauth bool, routes []string) CheckResult {
	probeVia = netroute.Parse(proxy)
	var tried []CheckResult
	for _, route := range routes {
		r := checkRoute(base, secret, model, route, oauth)
		if secret != "" { // scrubbed before the cut, so no prefix of it survives
			r.Note = strings.ReplaceAll(r.Note, secret, "[redacted]")
		}
		r.Note = shortNote(r.Note)
		if r.OK || r.Status == 0 || refusedMessage(r.Status) {
			return r
		}
		tried = append(tried, r)
	}
	// 404 and 405 only say the route is absent: the last route that exists
	// speaks for the model.
	for i := len(tried) - 1; i >= 0; i-- {
		if s := tried[i].Status; s != 404 && s != 405 {
			return tried[i]
		}
	}
	if len(tried) == 0 {
		// An exported Check asked of no route has nothing to ask: say so, never
		// index an empty slice (#636).
		return CheckResult{Note: "no route to check"}
	}
	return tried[len(tried)-1]
}

func checkRoute(base, secret, model, route string, oauth bool) CheckResult {
	path, hdr := "/v1/messages", anthropicHeaders(secret)
	body := map[string]any{"model": model, "max_tokens": 1, "messages": hi()}
	switch route {
	case "responses":
		path, hdr = "/v1/responses", bearer(secret)
		body = map[string]any{"model": model, "max_output_tokens": 16, "input": hi()}
	case "chat":
		path, hdr = "/v1/chat/completions", bearer(secret)
	}
	if oauth {
		// What Claude Code itself sends with a subscription's token.
		hdr = bearer(secret)
		hdr["anthropic-version"], hdr["anthropic-beta"] = ctxwindow.AnthropicVersion, "oauth-2025-04-20"
		body["system"] = "You are Claude Code, Anthropic's official CLI for Claude."
	}
	r := CheckResult{Route: route}
	start := time.Now()
	resp, err := do(probeClient(), "POST", ctxwindow.APIURL(base, path), body, hdr, probeTimeout)
	if err != nil {
		// The URL can carry a base's userinfo: name only the cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		r.MS, r.Note = time.Since(start).Milliseconds(), err.Error()
		return r
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	r.MS, r.Status, r.OK = time.Since(start).Milliseconds(), resp.StatusCode, resp.StatusCode < 300
	if !r.OK {
		r.Note = refusalText(raw)
	}
	return r
}

var innerMessage = regexp.MustCompile(`"message"\s*:\s*("(?:[^"\\]|\\.)*")`)

// shortNote is an upstream's reason in one short line. A gateway that relays
// its upstream's JSON error carries it inside its own message: the inner
// message is the reason.
func shortNote(s string) string {
	if m := innerMessage.FindStringSubmatch(s); m != nil {
		var inner string
		if json.Unmarshal([]byte(m[1]), &inner) == nil && inner != "" {
			s = inner
		}
	}
	return ordjson.TruncRunes(strings.Join(strings.Fields(s), " "), 150)
}
