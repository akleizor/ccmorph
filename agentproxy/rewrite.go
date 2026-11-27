package agentproxy

import (
	"github.com/akleizor/ccmorph/ordjson"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/util"
)

// Target is the endpoint and credential a picked profile serves on.
type Target struct {
	Base *url.URL // scheme and host the request goes to; the path is the request's own
	// Key is an API key, sent as x-api-key and as a bearer token (a gateway
	// may read either). OAuth is a Claude subscription token instead.
	Key, OAuth string
	// Tiers is the profile's {alias: model}, for a pick that names an alias.
	Tiers map[string]string
}

// FirstParty is whether the target is Anthropic's own API, which takes the
// session's request as it is: only another endpoint needs it made portable.
func (t Target) FirstParty() bool { return t.Base.Hostname() == anthropicHost }

const anthropicHost = "api.anthropic.com"

var authHeaders = []string{"Authorization", "X-Api-Key", "Cookie", "Proxy-Authorization"}

const oauthBeta = "oauth-2025-04-20"

// setAuth puts the target's credential on h in place of the session's own.
func (t Target) setAuth(h http.Header) {
	for _, k := range authHeaders {
		h.Del(k)
	}
	var betas []string
	for _, v := range h.Values("Anthropic-Beta") {
		for _, b := range strings.Split(v, ",") {
			if b = strings.TrimSpace(b); b != "" && !strings.HasPrefix(b, "oauth-") {
				betas = append(betas, b)
			}
		}
	}
	if t.OAuth != "" {
		betas = append(betas, oauthBeta)
		h.Set("Authorization", "Bearer "+t.OAuth)
	} else {
		h.Set("X-Api-Key", t.Key)
		if !t.FirstParty() { // first party reads a bearer as OAuth; a gateway may read either
			h.Set("Authorization", "Bearer "+t.Key)
		}
	}
	h.Del("Anthropic-Beta")
	if len(betas) > 0 {
		h.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
}

// servedModel is the model the pick serves for a request that asked for asked.
// A pick on an alias (the subscription's opus) is that profile's own model for
// the alias when it pins one, else the request's own model when it is already
// of that family, else the alias as named.
func servedModel(pick string, asked string, tiers map[string]string) string {
	if pick == "" {
		return asked
	}
	if util.IsTier(pick) {
		if m := strings.TrimSuffix(tiers[pick], "[1m]"); m != "" {
			return m
		}
		if strings.HasPrefix(util.TierWord(asked), "claude-"+pick+"-") {
			return asked
		}
	}
	return strings.TrimSuffix(pick, "[1m]")
}

// The session believes it talks to api.anthropic.com, so it sends request
// fields only first party accepts; another endpoint gets their portable form
// (makePortable holds the reasons, case by case).
var turnFields = regexp.MustCompile(`\s*\b(?:cch|cc_prev_req|cc_prompt_id|cc_turn_origin|cc_prompt_index|cc_turn_index)=[^;\s]*;?`)

const billingHeader = "x-anthropic-billing-header:"

// rewriteBody points the body at the served model and, for an endpoint that
// is not first party, drops what only first party accepts. It returns the body
// unchanged when nothing changed or it is not a JSON object.
func rewriteBody(body []byte, pick string, t Target) []byte {
	v, err := ordjson.Parse(body)
	p, ok := v.(*ordjson.Obj)
	if err != nil || !ok {
		return body
	}
	changed := false
	asked, _ := str(p, "model")
	if m := servedModel(pick, asked, t.Tiers); m != "" && m != asked {
		p.Set("model", m)
		changed = true
	}
	if !t.FirstParty() {
		changed = makePortable(p, true) || changed
	}
	return reencode(body, p, changed)
}

// reencode is p as bytes when it changed, else the original body.
func reencode(body []byte, p *ordjson.Obj, changed bool) []byte {
	if !changed {
		return body
	}
	return ordjson.Marshal(p, false)
}

// makePortable drops from p what only first party accepts and reports whether
// it changed anything: the display mode streamed thinking asks for, the cache
// breakpoints' scope, and the per-turn fields of the billing header (they
// rewrite the whole prompt each turn, so no endpoint's cache survives them).
// serverTools also drops Anthropic's server tools (web search, the advisor):
// typed, with no input_schema, which another endpoint refuses the whole request
// for, and no gateway session ever sends them.
func makePortable(p *ordjson.Obj, serverTools bool) bool {
	changed := false
	if th := child(p, "thinking"); th != nil && field(th, "display") == "updates" {
		th.Set("display", "omitted")
		changed = true
	}
	if l := list(p, "tools"); serverTools && l != nil {
		kept := l[:0:0]
		for _, t := range l {
			if m, ok := t.(*ordjson.Obj); ok && field(m, "input_schema") == nil && field(m, "type") != nil && field(m, "type") != "custom" {
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) != len(l) {
			p.Set("tools", kept)
			changed = true
		}
	}
	for _, f := range []string{"system", "messages", "tools"} {
		if v, ok := p.Get(f); ok {
			changed = dropCacheScope(v) || changed
		}
	}
	for _, b := range list(p, "system") {
		if m, ok := b.(*ordjson.Obj); ok {
			if s, ok := str(m, "text"); ok && strings.HasPrefix(s, billingHeader) {
				if stable := turnFields.ReplaceAllString(s, ""); stable != s {
					m.Set("text", stable)
					changed = true
				}
			}
		}
	}
	return changed
}

func dropCacheScope(node any) bool {
	changed := false
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			changed = dropCacheScope(item) || changed
		}
	case *ordjson.Obj:
		if cc := child(n, "cache_control"); cc != nil && del(cc, "scope") {
			changed = true
		}
		if c, ok := field(n, "content").([]any); ok {
			changed = dropCacheScope(c) || changed
		}
	}
	return changed
}
