package agentproxy

import (
	"bytes"
	"encoding/json"
	"github.com/akleizor/ccmorph/util"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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
		h.Set("Authorization", "Bearer "+t.Key)
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
// (rc_route.normalize_body holds the reasons, case by case).
var turnFields = regexp.MustCompile(`\s*\b(?:cch|cc_prev_req|cc_prompt_id|cc_turn_origin|cc_prompt_index|cc_turn_index)=[^;\s]*;?`)

const billingHeader = "x-anthropic-billing-header:"

// rewriteBody points the body at the served model and, for an endpoint that
// is not first party, drops what only first party accepts. It returns the body
// unchanged when nothing changed or it is not a JSON object.
func rewriteBody(body []byte, pick string, t Target) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var p map[string]any
	if dec.Decode(&p) != nil || p == nil {
		return body
	}
	changed := false
	asked, _ := p["model"].(string)
	if m := servedModel(pick, asked, t.Tiers); m != "" && m != asked {
		p["model"] = m
		changed = true
	}
	if !t.FirstParty() {
		if th, ok := p["thinking"].(map[string]any); ok && th["display"] == "updates" {
			th["display"] = "omitted"
			changed = true
		}
		// A first-party session also offers Anthropic's server tools (web
		// search, the advisor): typed, with no input_schema. Another endpoint
		// refuses the whole request for one of them, and no gateway session
		// ever sends them, so they stay behind.
		if tools, ok := p["tools"].([]any); ok {
			kept := tools[:0:0]
			for _, t := range tools {
				if m, ok := t.(map[string]any); ok && m["input_schema"] == nil && m["type"] != nil && m["type"] != "custom" {
					continue
				}
				kept = append(kept, t)
			}
			if len(kept) != len(tools) {
				p["tools"] = kept
				changed = true
			}
		}
		for _, f := range []string{"system", "messages", "tools"} {
			changed = dropCacheScope(p[f]) || changed
		}
		if sys, ok := p["system"].([]any); ok {
			for _, b := range sys {
				if m, ok := b.(map[string]any); ok {
					if s, ok := m["text"].(string); ok && strings.HasPrefix(s, billingHeader) {
						if stable := turnFields.ReplaceAllString(s, ""); stable != s {
							m["text"] = stable
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return body
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if enc.Encode(p) != nil {
		return body
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}

func dropCacheScope(node any) bool {
	changed := false
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			changed = dropCacheScope(item) || changed
		}
	case map[string]any:
		if cc, ok := n["cache_control"].(map[string]any); ok {
			if _, has := cc["scope"]; has {
				delete(cc, "scope")
				changed = true
			}
		}
		if c, ok := n["content"].([]any); ok {
			changed = dropCacheScope(c) || changed
		}
	}
	return changed
}
