package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/shim"
)

const (
	probeTimeout = 60 * time.Second // a dialect silent for a minute is not going to answer
	ccMaxTokens  = 32000            // what Claude Code sends
)

var (
	outCap    = regexp.MustCompile(`(?i)supports at most (\d+) completion tokens`)
	versionRE = regexp.MustCompile(`(?i)/v[0-9]+$`)
	// Every id rendered into the config is a YAML scalar and a cliproxyapi model
	// name; ids outside this set are skipped, not escaped-and-hoped.
	safeModelID = regexp.MustCompile(`^[A-Za-z0-9._:\[\]()@/-]+$`)
)

// A single tool shape per dialect: Responses takes a flat function tool, Chat
// Completions nests it under "function".
var (
	emptyObj      = func() map[string]any { return map[string]any{"type": "object", "properties": map[string]any{}} }
	toolAnthropic = []any{map[string]any{"name": "probe", "description": "probe", "input_schema": emptyObj()}}
	toolResponses = []any{map[string]any{"type": "function", "name": "probe", "description": "probe", "parameters": emptyObj()}}
	toolChat      = []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "probe", "description": "probe", "parameters": emptyObj()}}}
	toolProbe = []any{map[string]any{"name": "probe_tool", "description": "probe",
		"input_schema": map[string]any{"type": "object",
			"properties": map[string]any{"x": map[string]any{"type": "string"}},
			"required":   []any{"x"}}}}
)

// client never carries the profile's credential to another origin (CheckRedirect
// refuses it, which reads as a transport error and so as indeterminate).
// loopback never uses a proxy, whatever http_proxy says: a proxy cannot reach
// this machine's 127.0.0.1.
var (
	client   = &http.Client{CheckRedirect: shim.CheckRedirect, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}}
	loopback = &http.Client{CheckRedirect: shim.CheckRedirect, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
)

// Swappable in unit tests, as the Python tests patched the module.
var (
	post          = postImpl
	dialectAutoFn = dialectAuto
	dialectOfFn   = dialectOf
	inventoryFn   = inventory
	outputCapFn   = outputCap
	reasonsFn     = reasonsByDefault
	recyclesFn    = recyclesToolIDs
)

func do(c *http.Client, method, u string, body any, headers map[string]string, timeout time.Duration) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		cancel()
		return nil, err
	}
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{resp.Body, cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

// indeterminate is true when a probe's status says nothing about the model: a
// transport error (0), a 5xx, or 401/403/429 -- properties of the credential or
// the gateway, which must not settle as a cached verdict shared by every
// profile on the endpoint.
func indeterminate(status int) bool {
	return status == 401 || status == 403 || status == 429 || status == 0 || (status >= 500 && status < 600)
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

func postImpl(u string, body map[string]any, headers map[string]string) (int, string) {
	resp, err := do(client, "POST", u, body, headers, probeTimeout)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 300 {
		if err != nil {
			return 0, err.Error()
		}
		return 200, ""
	}
	detail := strings.ToValidUTF8(string(raw), "�")
	var parsed map[string]any
	if json.Unmarshal([]byte(detail), &parsed) == nil {
		for _, k := range []string{"detailMsg", "error_message"} {
			if s, ok := parsed[k].(string); ok && s != "" {
				detail = s
				break
			}
		}
	}
	return resp.StatusCode, strings.ReplaceAll(truncRunes(detail, 600), "\n", " ")
}

// postJSON returns the parsed response object, or nil on any failure.
func postJSON(u string, body map[string]any, headers map[string]string) map[string]any {
	resp, err := do(client, "POST", u, body, headers, probeTimeout)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil
	}
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil {
		return nil
	}
	return out
}

// splitURL is urlsplit's scheme, netloc and path for "scheme://netloc/path".
func splitURL(raw string) (scheme, netloc, path string) {
	rest := raw
	if i := strings.Index(raw, "://"); i >= 0 {
		scheme, rest = strings.ToLower(raw[:i]), raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			netloc, rest = rest[:j], rest[j:]
		} else {
			netloc, rest = rest, ""
		}
	}
	if j := strings.IndexAny(rest, "?#"); j >= 0 {
		rest = rest[:j]
	}
	return scheme, netloc, rest
}

// apiURL matches the serving client's URL: only OpenAI bases include the version.
func apiURL(base, path string) string {
	scheme, netloc, p := splitURL(strings.TrimRight(base, "/"))
	prefix := strings.TrimRight(p, "/")
	// Claude Code and cliproxyapi's Anthropic client append /v1/messages verbatim.
	if path != "/v1/messages" && versionRE.MatchString(prefix) {
		path = strings.TrimPrefix(path, "/v1")
	}
	switch {
	case netloc != "":
		return scheme + "://" + netloc + prefix + path
	case scheme != "":
		return scheme + ":" + prefix + path
	}
	return prefix + path
}

func keyHeaders(key string) map[string]string {
	return map[string]string{"x-api-key": key, "Authorization": "Bearer " + key}
}

// inventory lists the usable chat model ids. No anthropic-version header:
// proxies rewrite non-claude ids into claude-<junk>-<reversed-id> when they see it.
func inventory(base, key string, c *http.Client) ([]string, error) {
	if c == nil {
		c = client
	}
	resp, err := do(c, "GET", apiURL(base, "/v1/models"), nil, keyHeaders(key), 60*time.Second)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP Error %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	data, _ := doc["data"].([]any)
	out, unsafe := []string{}, []string{}
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Exclude only an explicit denial; most endpoints publish no capabilities.
		if caps, ok := m["capabilities"].(map[string]any); ok && !truthy(caps["chat_completion"]) {
			continue
		}
		id, ok := m["id"].(string)
		if !ok || id == "" {
			continue
		}
		if !safeModelID.MatchString(id) {
			unsafe = append(unsafe, id)
			continue
		}
		out = append(out, id)
	}
	if len(unsafe) > 0 {
		sort.Strings(unsafe)
		reprs := make([]string, len(unsafe))
		for i, u := range unsafe {
			reprs[i] = shim.PyRepr(truncRunes(u, 60))
		}
		logf("skipping %d model id(s) with unsafe characters: %s", len(unsafe), strings.Join(reprs, ", "))
	}
	sort.Strings(out)
	return out, nil
}

// truthy is Python truthiness for decoded JSON.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	case []any:
		return len(t) > 0
	case map[string]any:
		return len(t) > 0
	}
	return true
}

const noInference = "cannot determine protocol without inference"

// dialectAuto uses an explicit per-model endpoint declaration, never schema
// error guesses.
func dialectAuto(base, key, model string) (string, string, int) {
	resp, err := do(client, "GET", apiURL(base, "/v1/models"), nil, keyHeaders(key), probeTimeout)
	var doc map[string]any
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			err = fmt.Errorf("HTTP Error %d", resp.StatusCode)
		} else {
			err = json.NewDecoder(resp.Body).Decode(&doc)
		}
	}
	if err != nil {
		return "unusable", noInference + ": " + err.Error(), 0
	}
	rows, _ := doc["data"].([]any)
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok || row["id"] != model {
			continue
		}
		endpoints, ok := row["supported_endpoints"].([]any)
		if !ok {
			break
		}
		for _, p := range [][2]string{{"/v1/messages", "anthropic"}, {"/v1/responses", "responses"}, {"/v1/chat/completions", "chat"}} {
			for _, e := range endpoints {
				if e == p[0] {
					return p[1], "declared supported_endpoints", 200
				}
			}
		}
	}
	return "unusable", noInference + ": no per-model endpoint declaration or proven cache", 0
}

func anthropicHeaders(key string) map[string]string {
	return map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
}

func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

func hi() []any { return []any{map[string]any{"role": "user", "content": "hi"}} }

// dialectOf finds which dialect the model serves, preferring the one needing no
// translation. The status characterizes an "unusable" verdict: the first
// indeterminate status seen, otherwise the last definite one. No thinking field
// on the Anthropic probe: dialect and thinking support are separate axes.
func dialectOf(base, key, model string, bounded bool) (string, string, int) {
	var seen []int
	status, first := post(apiURL(base, "/v1/messages"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolAnthropic, "messages": hi()},
		anthropicHeaders(key))
	seen = append(seen, status)
	if status == 200 {
		return "anthropic", "", 200
	}

	resp := map[string]any{"model": model, "reasoning": map[string]any{"effort": "low"},
		"tools": toolResponses, "input": hi()}
	if bounded {
		resp["max_output_tokens"] = 16
	}
	status, _ = post(apiURL(base, "/v1/responses"), resp, bearer(key))
	seen = append(seen, status)
	if status == 200 {
		return "responses", "", 200
	}

	status, note := post(apiURL(base, "/v1/chat/completions"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolChat,
			"reasoning_effort": "low", "messages": hi()}, bearer(key))
	seen = append(seen, status)
	if status == 200 {
		return "chat", "", 200
	}

	// Rejecting reasoning_effort is not a reason to drop a model: the translator
	// can strip it.
	status, noteNR := post(apiURL(base, "/v1/chat/completions"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolChat, "messages": hi()}, bearer(key))
	seen = append(seen, status)
	if status == 200 {
		return "chat-noreason", "", 200
	}

	verdict := seen[len(seen)-1]
	for _, s := range seen {
		if indeterminate(s) {
			verdict = s
			break
		}
	}
	n := note
	if n == "" {
		n = noteNR
	}
	if n == "" {
		n = first
	}
	return "unusable", truncRunes(n, 150), verdict
}

// outputCap is the largest max_tokens accepted, when the model states one in its
// own error. max_tokens MUST stay ccMaxTokens (the question is whether the model
// rejects what Claude Code will really send); a stop sequence bounds the
// generation of a model that accepts it, since a metered gateway bills those tokens.
func outputCap(base, key, model, dialect string) *int {
	if dialect == "responses" || dialect == "unusable" {
		return nil // /responses uses max_output_tokens, which gateways reject
	}
	path, hdr, stopKey := "/v1/chat/completions", bearer(key), "stop"
	if dialect == "anthropic" {
		path, hdr, stopKey = "/v1/messages", anthropicHeaders(key), "stop_sequences"
	}
	body := map[string]any{"model": model, "max_tokens": ccMaxTokens, "messages": hi(),
		stopKey: []any{"\n\n"}}
	status, note := post(apiURL(base, path), body, hdr)
	if status == 200 {
		return nil
	}
	m := outCap.FindStringSubmatch(note)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	return &n
}

// reasonsByDefault is true only when unrequested thinking can also be disabled
// safely (kimi-k3 pathology, 2026-08-16): always-thinking models reject the
// control and must retain native behavior. Any probe failure answers false.
func reasonsByDefault(base, key, model string) bool {
	headers := anthropicHeaders(key)
	request := map[string]any{"model": model, "max_tokens": 512, "messages": hi()}
	body := postJSON(apiURL(base, "/v1/messages"), request, headers)
	if body == nil {
		return false
	}
	reasons := false
	if content, ok := body["content"].([]any); ok {
		for _, b := range content {
			if m, ok := b.(map[string]any); ok && m["type"] == "thinking" {
				reasons = true
			}
		}
	}
	if !reasons {
		return false
	}
	request["thinking"] = map[string]any{"type": "disabled"}
	return postJSON(apiURL(base, "/v1/messages"), request, headers) != nil
}

// recyclesToolIDs is true when the upstream mints deterministic tool_use ids
// (the same id on two independent responses). Any probe failure answers false.
func recyclesToolIDs(base, key, model string) bool {
	var seen [2]any
	for i := range seen {
		body := postJSON(apiURL(base, "/v1/messages"),
			map[string]any{"model": model, "max_tokens": 64, "tools": toolProbe,
				"messages": []any{map[string]any{"role": "user", "content": "Call probe_tool with x=1. No prose."}}},
			anthropicHeaders(key))
		if body == nil {
			return false
		}
		content, _ := body["content"].([]any)
		found := false
		for _, b := range content {
			if m, ok := b.(map[string]any); ok && m["type"] == "tool_use" {
				seen[i], found = m["id"], true
				break
			}
		}
		if !found {
			return false
		}
	}
	return seen[0] == seen[1]
}

// serving reports whether a loopback endpoint answers /v1/models with 200.
func serving(port int, key string) bool {
	resp, err := do(loopback, "GET", "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil,
		map[string]string{"x-api-key": key}, 5*time.Second)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}
