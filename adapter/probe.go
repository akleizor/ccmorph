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

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/profile"
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
// transport error (0), a 5xx, or 401/402/403/429 -- properties of the
// credential, the account's balance, or the gateway, which must not settle as a
// cached verdict shared by every profile on the endpoint.
func indeterminate(status int) bool {
	return status == 401 || status == 402 || status == 403 || status == 429 || status == 0 || (status >= 500 && status < 600)
}

// refusedMessage is true when a status is the gateway refusing the request
// itself -- an empty balance (402), a rate limit (429), or its own failure
// (the remaining 5xx) -- rather than saying the model does not speak this
// dialect. Such an answer applies to every dialect, so probing stops at the
// first one (#375). 401/403 stay probeable because the Anthropic path and the
// Bearer paths send different credentials, 0 is a transport failure, not an
// answer, and 501 is excluded: it names an operation the gateway does not
// implement on this path, so the working dialect is still tried.
func refusedMessage(status int) bool {
	return status == 402 || status == 429 || (status >= 500 && status < 600 && status != 501)
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
		// The Anthropic error shape nests the explanation one level down.
		if e, ok := parsed["error"].(map[string]any); ok {
			if s, ok := e["message"].(string); ok && s != "" {
				detail = s
			}
		}
	}
	return resp.StatusCode, strings.ReplaceAll(ordjson.TruncRunes(detail, 600), "\n", " ")
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

// apiURL matches the serving client's URL: only OpenAI bases include the version.
func apiURL(base, path string) string {
	scheme, netloc, p := profile.SplitURL(strings.TrimRight(base, "/"))
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

// refusingStatus is a listing request the endpoint answered without serving
// one. Its message keeps the "HTTP Error %d" shape inventory has always
// reported, so logs and other callers read identically, while a caller can
// tell a gateway that offers no model list from a transport failure (#357).
type refusingStatus struct{ code int }

func (e refusingStatus) Error() string { return fmt.Sprintf("HTTP Error %d", e.code) }

// messageRefusal is the gateway refusing the launch's message itself: not that
// the model speaks no known dialect, but that the account or the gateway
// answered no to the one question a session depends on. Its text is the
// gateway's own explanation, so the ready-file reader sees what the gateway
// said, never an adapter problem (#375).
type messageRefusal struct {
	model  string
	status int
	text   string
}

func (e messageRefusal) Error() string {
	if note := strings.TrimSpace(e.text); note != "" {
		return fmt.Sprintf("the gateway refused the message for model %s (HTTP %d): %s",
			shim.PyRepr(e.model), e.status, ordjson.TruncRunes(note, 150))
	}
	return fmt.Sprintf("the gateway refused the message for model %s (HTTP %d)", shim.PyRepr(e.model), e.status)
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
		return nil, refusingStatus{resp.StatusCode}
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	data, _ := doc["data"].([]any)
	return listedModels(data, true), nil
}

// truthy is Python truthiness for decoded JSON. It is not ordjson.Truthy: this
// package decodes with encoding/json, so numbers are float64 and objects
// map[string]any rather than json.Number and *ordjson.Obj.
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

// listingRow is one row of a /v1/models listing, decoded once for every
// consumer: the id exactly as the row wrote it, the endpoints it declares, and
// the two filters the model-id list applies.
type listingRow struct {
	// id is the row's "id" as written, so a model lookup compares what the
	// gateway wrote: a row whose id is not a string never matches one.
	id any
	// name is the same id as a string, "" when it is not one.
	name string
	// endpoints is the row's supported_endpoints when that is an array; nil
	// when the row declares none.
	endpoints []any
	// chat is false only when the row explicitly denies
	// capabilities.chat_completion.
	chat bool
	// safe is true when name is a non-empty string of safe model-id
	// characters.
	safe bool
}

// endpointPrecedence is the order a model's supported_endpoints are asked for
// its dialect: the one needing no translation first.
var endpointPrecedence = [][2]string{
	{"/v1/messages", "anthropic"},
	{"/v1/responses", "responses"},
	{"/v1/chat/completions", "chat"},
}

// walkListing walks a /v1/models listing's data rows in listing order,
// decoding each row once into what every reader of the listing needs; the
// filters live here, once, so the id list and the endpoint lookup cannot
// drift apart. Only an explicit capabilities.chat_completion denial excludes
// a row from the id list, and only a non-empty string id of safe characters
// is a listable model id. logUnsafe logs the ids the safe filter skips, the
// way inventory always has; quiet readers pass false.
func walkListing(data []any, logUnsafe bool) []listingRow {
	var rows []listingRow
	var unsafe []string
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		r := listingRow{id: m["id"], chat: true}
		// Exclude only an explicit denial; most endpoints publish no capabilities.
		if caps, ok := m["capabilities"].(map[string]any); ok && !truthy(caps["chat_completion"]) {
			r.chat = false
		}
		r.name, _ = m["id"].(string)
		if r.name != "" {
			if safeModelID.MatchString(r.name) {
				r.safe = true
			} else if r.chat {
				unsafe = append(unsafe, r.name)
			}
		}
		r.endpoints, _ = m["supported_endpoints"].([]any)
		rows = append(rows, r)
	}
	if logUnsafe && len(unsafe) > 0 {
		sort.Strings(unsafe)
		reprs := make([]string, len(unsafe))
		for i, u := range unsafe {
			reprs[i] = shim.PyRepr(ordjson.TruncRunes(u, 60))
		}
		logf("skipping %d model id(s) with unsafe characters: %s", len(unsafe), strings.Join(reprs, ", "))
	}
	return rows
}

const noInference = "cannot determine protocol without inference"

// dialectAuto uses an explicit per-model endpoint declaration, never schema
// error guesses. The status is the refusal's own when the endpoint answered —
// a rejected key's 401, so the caller can refuse without inference — and 0
// only when nothing answered (#374).
func dialectAuto(base, key, model string) (string, string, int) {
	resp, err := do(client, "GET", apiURL(base, "/v1/models"), nil, keyHeaders(key), probeTimeout)
	var doc map[string]any
	status := 0
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			err = fmt.Errorf("HTTP Error %d", resp.StatusCode)
			status = resp.StatusCode
		} else {
			err = json.NewDecoder(resp.Body).Decode(&doc)
		}
	}
	if err != nil {
		return "unusable", noInference + ": " + err.Error(), status
	}
	data, _ := doc["data"].([]any)
	if dialect := declaredDialect(data, model); dialect != "" {
		return dialect, "declared supported_endpoints", 200
	}
	return "unusable", noInference + ": no per-model endpoint declaration or proven cache", 0
}

// declaredDialect is dialectAuto's answer read from an already-fetched model
// listing: the dialect a model's own supported_endpoints declares, or "" when
// it declares none. It sends nothing, so doctor can use it read-only.
func declaredDialect(data []any, model string) string {
	for _, r := range walkListing(data, false) {
		if r.id != model {
			continue
		}
		if r.endpoints == nil {
			// The first row naming the model ends the search, declared or not.
			return ""
		}
		for _, p := range endpointPrecedence {
			for _, e := range r.endpoints {
				if e == p[0] {
					return p[1]
				}
			}
		}
	}
	return ""
}

// listedModels is inventory's id list read from an already-fetched listing,
// with the same filters. logUnsafe carries inventory's unsafe-id log; a quiet
// reader of a listing it already holds passes false.
func listedModels(data []any, logUnsafe bool) []string {
	out := []string{}
	for _, r := range walkListing(data, logUnsafe) {
		if r.chat && r.safe {
			out = append(out, r.name)
		}
	}
	sort.Strings(out)
	return out
}

func anthropicHeaders(key string) map[string]string {
	return map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
}

func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

func hi() []any { return []any{map[string]any{"role": "user", "content": "hi"}} }

// dialectOf finds which dialect the model serves, preferring the one needing no
// translation. The status characterizes an "unusable" verdict: the first
// indeterminate status seen, otherwise the last definite one. A status the
// gateway refuses outright (402, 429, 5xx) applies to every dialect, so probing
// stops at the first such answer and its note becomes the verdict's (#375). No
// thinking field on the Anthropic probe: dialect and thinking support are
// separate axes.
func dialectOf(base, key, model string, bounded bool) (string, string, int) {
	var seen []int
	var notes []string
	status, first := post(apiURL(base, "/v1/messages"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolAnthropic, "messages": hi()},
		anthropicHeaders(key))
	seen, notes = append(seen, status), append(notes, first)
	if status == 200 {
		return "anthropic", "", 200
	}
	if refusedMessage(status) {
		return "unusable", ordjson.TruncRunes(first, 150), status
	}

	resp := map[string]any{"model": model, "reasoning": map[string]any{"effort": "low"},
		"tools": toolResponses, "input": hi()}
	if bounded {
		resp["max_output_tokens"] = 16
	}
	status, note := post(apiURL(base, "/v1/responses"), resp, bearer(key))
	seen, notes = append(seen, status), append(notes, note)
	if status == 200 {
		return "responses", "", 200
	}
	if refusedMessage(status) {
		return "unusable", ordjson.TruncRunes(note, 150), status
	}

	status, note = post(apiURL(base, "/v1/chat/completions"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolChat,
			"reasoning_effort": "low", "messages": hi()}, bearer(key))
	seen, notes = append(seen, status), append(notes, note)
	if status == 200 {
		return "chat", "", 200
	}
	if refusedMessage(status) {
		return "unusable", ordjson.TruncRunes(note, 150), status
	}

	// Rejecting reasoning_effort is not a reason to drop a model: the translator
	// can strip it.
	status, noteNR := post(apiURL(base, "/v1/chat/completions"),
		map[string]any{"model": model, "max_tokens": 8, "tools": toolChat, "messages": hi()}, bearer(key))
	seen, notes = append(seen, status), append(notes, noteNR)
	if status == 200 {
		return "chat-noreason", "", 200
	}
	if refusedMessage(status) {
		return "unusable", ordjson.TruncRunes(noteNR, 150), status
	}

	verdict, n := seen[len(seen)-1], note
	if n == "" {
		n = noteNR
	}
	if n == "" {
		n = first
	}
	for i, s := range seen {
		if indeterminate(s) {
			verdict = s
			// An indeterminate answer the gateway actually gave outranks a
			// later definite one's note: it is the truer cause.
			if s != 0 && notes[i] != "" {
				n = notes[i]
			}
			break
		}
	}
	return "unusable", ordjson.TruncRunes(n, 150), verdict
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
// (the same id on two independent responses). Any probe failure answers false,
// and an id that is not a string is a shaky reading too: comparing two decoded
// JSON objects with == panics.
func recyclesToolIDs(base, key, model string) bool {
	var seen [2]string
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
				id, ok := m["id"].(string)
				if !ok {
					return false
				}
				seen[i], found = id, true
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

// answered reports whether a loopback endpoint answers at all — any HTTP
// status, and no credential on the wire. The shim answers a HEAD itself (it
// relays only GET, POST, PUT and DELETE), so a direct-route gateway that
// offers no model list still reads as ready, and the readiness probe never
// travels upstream (#357).
func answered(port int) bool {
	resp, err := do(loopback, "HEAD", "http://127.0.0.1:"+strconv.Itoa(port)+"/", nil, nil, 5*time.Second)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return true
}
