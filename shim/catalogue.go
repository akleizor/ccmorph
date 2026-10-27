package shim

import (
	"context"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- model identity -----------------------------------------------------------
// Claude Code and the Claude apps speak Anthropic model ids; that is the only
// vocabulary this shim shows them. Two gateway habits break it: answering under
// an internal name (which the phone then displays), and a catalogue that moves
// (a new Opus appears, an old one is retired). Requests keep the asked id when
// the gateway serves it and otherwise take the newest served model of the SAME
// family whose version is not older than the asked one (never another family,
// never a downgrade); responses carry the Anthropic id that was sent.

const (
	catalogueTTL      = 600 * time.Second // a moving catalogue is re-read at most this often...
	catalogueMissTTL  = 60 * time.Second  // ...or this often while an asked id is missing
	catalogueDeadline = 20 * time.Second  // total wait for connect, headers and body together
	catalogueMaxBytes = 1024 * 1024
)

var claudeID = regexp.MustCompile(`^claude-([a-z]+)-([0-9]+(?:-[0-9]{1,2})?)(?:-[0-9]{8})?$`)

// claudeVersion is (family, version tuple) for an Anthropic id.
func claudeVersion(model string) (string, []int, bool) {
	m := claudeID.FindStringSubmatch(model)
	if m == nil {
		return "", nil, false
	}
	var parts []int
	for _, p := range strings.Split(m[2], "-") {
		n, _ := strconv.Atoi(p)
		parts = append(parts, n)
	}
	// A trailing -0 is spelling (claude-opus-5-0 is claude-opus-5), not a newer version.
	for len(parts) > 1 && parts[len(parts)-1] == 0 {
		parts = parts[:len(parts)-1]
	}
	return m[1], parts, true
}

// resolveModel is the id to send for model, given the gateway's served ids.
func resolveModel(model string, served map[string]bool) string {
	if len(served) == 0 || served[model] {
		return model
	}
	family, wanted, ok := claudeVersion(model)
	if !ok {
		return model
	}
	// The asked version under another spelling (a dated id) beats a newer one;
	// an older version is never substituted: the gateway's own error is better.
	best, bestExact, bestVersion, found := "", false, []int(nil), false
	for candidate := range served {
		f, v, ok := claudeVersion(candidate)
		if !ok || f != family || slices.Compare(v, wanted) < 0 {
			continue
		}
		exact := slices.Equal(v, wanted)
		better := !found
		if found {
			switch {
			case exact != bestExact:
				better = exact
			case slices.Compare(v, bestVersion) != 0:
				better = slices.Compare(v, bestVersion) > 0
			default:
				better = candidate > best
			}
		}
		if better {
			best, bestExact, bestVersion, found = candidate, exact, v, true
		}
	}
	if !found {
		return model
	}
	return best
}

const (
	pathMessages = "/v1/messages"
	pathCount    = "/v1/messages/count_tokens"
	pathResp     = "/v1/responses"
	pathChat     = "/v1/chat/completions"
)

// Catalogue is the gateway's served model ids, read from its own /v1/models.
type Catalogue struct {
	upstream, modelsURL string
	client              *http.Client

	mu        sync.Mutex
	ids       map[string]bool
	endpoints map[string][]string // nil value: the gateway declared none
	readAt    time.Time
}

func newCatalogue(upstream, modelsURL string, client *http.Client) *Catalogue {
	upstream = strings.TrimRight(upstream, "/")
	if modelsURL == "" {
		modelsURL = upstream + "/v1/models"
	}
	return &Catalogue{upstream: upstream, modelsURL: modelsURL, client: client, ids: map[string]bool{}, endpoints: map[string][]string{}}
}

func hasAny(list []string, paths ...string) bool {
	for _, p := range paths {
		if slices.Contains(list, p) {
			return true
		}
	}
	return false
}

// read fetches the catalogue within the deadline: a late, oversized, truncated
// or unparseable answer is no answer ("send as asked").
func (c *Catalogue) read(key string) (map[string][]string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), catalogueDeadline)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", c.modelsURL, nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", "")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, catalogueMaxBytes+1))
	if err != nil || len(body) > catalogueMaxBytes {
		return nil, false
	}
	var top any
	if err := jsonv2.Unmarshal(body, &top, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil, false
	}
	obj, ok := top.(map[string]any)
	if !ok {
		return nil, false
	}
	var rows []any
	switch data := obj["data"].(type) {
	case []any:
		rows = data
	case map[string]any, string:
	default:
		if _, present := obj["data"]; present {
			return nil, false // not iterable
		}
	}
	declarations := map[string][]string{}
	countTokens := map[string]bool{}
	for _, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			continue
		}
		id, ok := row["id"].(string)
		if !ok {
			continue
		}
		var endpoints []string
		list, isList := row["supported_endpoints"].([]any)
		if isList {
			endpoints = []string{}
			for _, e := range list {
				if s, ok := e.(string); ok {
					endpoints = append(endpoints, s)
				}
			}
			if slices.Contains(endpoints, pathCount) {
				countTokens[id] = true
			}
		}
		// Match dialect_auto: malformed declarations stop the scan; lists
		// without a serving endpoint allow a later duplicate.
		if previous, seen := declarations[id]; seen {
			if previous == nil || hasAny(previous, pathMessages, pathResp, pathChat) {
				continue
			}
		}
		declarations[id] = endpoints
	}
	// Token counting is independent of the canonical message dialect.
	for model := range countTokens {
		if e := declarations[model]; e != nil && !slices.Contains(e, pathCount) {
			declarations[model] = append(e, pathCount)
		}
	}
	return declarations, true
}

// served is the set of ids the gateway serves, refreshed when stale.
func (c *Catalogue) served(key, model string) map[string]bool {
	if c.upstream == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	age := time.Duration(1<<62 - 1)
	if !c.readAt.IsZero() {
		age = time.Since(c.readAt)
	}
	if age > catalogueTTL || (!c.ids[model] && age > catalogueMissTTL) {
		c.readAt = time.Now()
		if decl, ok := c.read(key); ok && time.Since(c.readAt) <= catalogueDeadline {
			c.endpoints, c.ids = decl, map[string]bool{}
			for id := range decl {
				c.ids[id] = true
			}
		} else {
			c.ids, c.endpoints = map[string]bool{}, map[string][]string{}
		}
	}
	return c.ids
}

func (c *Catalogue) declared(model string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoints[model]
}

// --- model routes ----------------------------------------------------------------

// routeSpec is CCMORPH_MODEL_ROUTE, handed over by the adapter.
type routeSpec struct {
	Adapter   string            `json:"adapter"`
	Profile   string            `json:"profile"`
	Config    string            `json:"config"`
	Port      any               `json:"port"`
	Identity  string            `json:"identity"`
	Models    []string          `json:"models"`
	ModelMap  map[string]string `json:"model_map"`
	Upstream  string            `json:"upstream"`
	ModelsURL string            `json:"models_url"`
	Key       string            `json:"key_sha256"`
	Timeout   any               `json:"timeout"`
}

// ModelRoutes prepares a selected model before sending its first request, never
// replays it. A spec without "config" fronts the gateway directly (no
// translator). Declared non-Messages mapped targets are rejected before
// incompatible forwarding.
type ModelRoutes struct {
	command   []string
	known     map[string]bool
	modelMap  map[string]string
	catalogue *Catalogue
	keySHA256 string
	timeout   time.Duration
	lock      chan struct{}
	usage     *UsageSidecar
	knownMu   sync.Mutex
}

func newModelRoutes(raw string, client *http.Client) (*ModelRoutes, error) {
	var spec routeSpec
	if err := jsonv2.Unmarshal([]byte(raw), &spec, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil, fmt.Errorf("CCMORPH_MODEL_ROUTE: %w", err)
	}
	if spec.Key == "" {
		return nil, fmt.Errorf("CCMORPH_MODEL_ROUTE: key_sha256 missing")
	}
	var seconds float64
	switch t := spec.Timeout.(type) {
	case float64:
		seconds = t
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err != nil {
			return nil, fmt.Errorf("CCMORPH_MODEL_ROUTE: bad timeout %q", t)
		}
		seconds = f
	default:
		return nil, fmt.Errorf("CCMORPH_MODEL_ROUTE: timeout missing")
	}
	r := &ModelRoutes{
		known: map[string]bool{}, modelMap: spec.ModelMap, keySHA256: spec.Key,
		timeout:   time.Duration((seconds + 5) * float64(time.Second)),
		catalogue: newCatalogue(spec.Upstream, spec.ModelsURL, client),
		lock:      make(chan struct{}, 1),
	}
	for _, m := range spec.Models {
		r.known[m] = true
	}
	if spec.Config != "" {
		port := ""
		switch p := spec.Port.(type) {
		case float64:
			port = strconv.FormatFloat(p, 'f', -1, 64)
		case string:
			port = p
		}
		r.command = adapterCommand(spec.Adapter)
		r.command = append(r.command, spec.Profile, "--ensure-config", spec.Config,
			"--ensure-port", port, "--ensure-identity", spec.Identity)
	}
	// The adapter always names the profile; without one there is no route to
	// attribute a row to, so the sidecar stays off and the shim writes nothing.
	if spec.Profile != "" {
		r.usage = newSidecar(spec.Profile, spec.Identity)
	}
	return r, nil
}

// adapterCommand runs the adapter the way its shebang would; an adapter that is
// not executable is a script for python3.
func adapterCommand(adapter string) []string {
	if st, err := os.Stat(adapter); err == nil && st.Mode()&0o111 != 0 {
		return []string{adapter}
	}
	return []string{"python3", adapter}
}

// mappingTarget is the explicit map target for a request's wire id.
func (r *ModelRoutes) mappingTarget(model string) (string, bool) {
	t, ok := r.modelMap[strings.TrimSuffix(model, "[1m]")]
	return t, ok
}

// ensure prepares model; a non-empty result is the refusal message.
func (r *ModelRoutes) ensure(body []byte, key string, requested *string, path string) string {
	d := parseDoc(body)
	if d == nil {
		return ""
	}
	model, ok := d.str("model")
	if !ok {
		return ""
	}
	model = strings.TrimSuffix(model, "[1m]")
	if r.command == nil {
		// Same rule as resolve: one key, the wire id. A suffixed key decides
		// nothing, so this request must not count as mapped either.
		base := ""
		if requested != nil {
			base = strings.TrimSuffix(*requested, "[1m]")
		}
		if _, mapped := r.modelMap[base]; requested != nil && mapped {
			r.catalogue.served(key, model)
			endpoints := r.catalogue.declared(model)
			if endpoints != nil && !slices.Contains(endpoints, pathMessages) && !slices.Contains(endpoints, path) &&
				hasAny(endpoints, pathResp, pathChat) {
				return fmt.Sprintf("Mapped model %s requires translation: the gateway declares "+
					"a non-Messages endpoint only. This native edge cannot prepare "+
					"its adapter; use a translation-capable route for this map.", pyRepr(model))
			}
		}
		return ""
	}
	r.knownMu.Lock()
	known := r.known[model]
	r.knownMu.Unlock()
	if known {
		return ""
	}
	select {
	case r.lock <- struct{}{}:
	case <-time.After(r.timeout):
		return "Another model is being prepared; retry this request."
	}
	defer func() { <-r.lock }()
	r.knownMu.Lock()
	known = r.known[model]
	r.knownMu.Unlock()
	if known {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	args := append(slices.Clone(r.command[1:]), "--model", model)
	err := exec.CommandContext(ctx, r.command[0], args...).Run()
	if ctx.Err() != nil || (err != nil && !isExit(err)) {
		return fmt.Sprintf("Could not prepare model %s within the compatibility deadline.", pyRepr(model))
	}
	if err != nil {
		return fmt.Sprintf("Model %s could not be prepared; check gateway availability and adapter compatibility.", pyRepr(model))
	}
	r.knownMu.Lock()
	r.known[model] = true
	r.knownMu.Unlock()
	return ""
}

func isExit(err error) bool {
	_, ok := err.(*exec.ExitError)
	return ok
}

// pyRepr is repr() of a str: single quotes unless it holds one and no double.
func pyRepr(s string) string {
	q := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		q = '"'
	}
	var b strings.Builder
	b.WriteByte(q)
	for _, r := range s {
		switch {
		case r == rune(q) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(q)
	return b.String()
}
