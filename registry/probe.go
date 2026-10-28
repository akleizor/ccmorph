package registry

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/pycompat"
)

var declared = []*regexp.Regexp{
	regexp.MustCompile(`(?i)maximum context length is (\d[\d,]*)`),
	regexp.MustCompile(`(?i)too long:\s*\d[\d,]*\s*tokens?\s*>\s*(\d[\d,]*)\s*maximum`),
	regexp.MustCompile(`(?i)>\s*(\d[\d,]*)\s*maximum`),
	regexp.MustCompile(`(?i)max(?:imum)?[ _-]?(?:input|context)[ _-]?(?:tokens|length)\D{0,12}(\d[\d,]*)`),
	regexp.MustCompile(`(?i)(?:limit|maximum) of (\d[\d,]*) tokens`),
	regexp.MustCompile(`(?i)should be \[?\d+,\s*(\d[\d,]*)\]?`),
}

// "Wrong endpoint for this model", not "payload too large": try the other shape.
var shapeMismatch = regexp.MustCompile(`(?i)does not support the required API|MESSAGES_API|internal_feature_not_supported`)

var contextOverflow = regexp.MustCompile(`(?i)prompt.{0,20}too long|context.{0,30}(?:exceed|too long)|maximum context length`)

type shape struct {
	name, path string
	headers    func(key string) [][2]string
}

var shapes = []shape{
	{"anthropic", "/v1/messages", func(k string) [][2]string {
		return [][2]string{{"x-api-key", k}, {"anthropic-version", "2023-06-01"}}
	}},
	{"openai", "/v1/chat/completions", func(k string) [][2]string {
		return [][2]string{{"Authorization", "Bearer " + k}}
	}},
}

// matchDeclared finds a stated ceiling in an error body.
func matchDeclared(detail string) (int64, bool) {
	for _, pat := range declared {
		if m := pat.FindStringSubmatch(detail); m != nil {
			n, err := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
			if err != nil {
				continue // shortcut: a ceiling past int64 is not a real one
			}
			return n, true
		}
	}
	return 0, false
}

// postSized is the single place a sized prompt is built and sent.
func postSized(ctx context.Context, base, key, model, blob, path string, hdr func(string) [][2]string, timeout time.Duration, op Opener) (*obj, error) {
	body := pycompat.NewObj()
	body.Set("model", model)
	body.Set("max_tokens", pycompat.Int(8))
	msg := pycompat.NewObj()
	msg.Set("role", "user")
	msg.Set("content", blob)
	body.Set("messages", []any{msg})
	headers := append([][2]string{{"content-type", "application/json"}}, hdr(key)...)
	resp, err := pick(op).Open(ctx, Request{Method: "POST", URL: APIURL(base, path), Header: headers,
		Body: []byte(pycompat.Marshal(body, pycompat.Format{ASCII: true}))}, timeout)
	if err != nil {
		return nil, err
	}
	v, perr := pycompat.Parse(resp)
	if perr != nil {
		return nil, fmt.Errorf("JSONDecodeError: %s", perr.Error())
	}
	doc := asObj(v)
	if doc == nil {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyType(v))
	}
	return doc, nil
}

// postSizedFn is a seam for tests that fake the request builder.
var postSizedFn = postSized

// tokens is `usage.input_tokens or usage.prompt_tokens`, 0 when neither is a count.
func tokens(doc *obj) int64 {
	used := asObj(getv(doc, "usage"))
	if used == nil {
		return 0
	}
	if n, ok := anyInt(getv(used, "input_tokens")); ok && n != 0 {
		return n
	}
	n, _ := anyInt(getv(used, "prompt_tokens"))
	return n
}

func stopReason(doc *obj) string { s, _ := str(getv(doc, "stop_reason")); return s }

// reading is (reported_input_tokens, stop_reason) from one accepted request.
type reading struct {
	reported int64
	stop     string
}

func reread(ctx context.Context, base, key, model, blob, path string, hdr func(string) [][2]string, timeout time.Duration, op Opener) *reading {
	doc, err := postSizedFn(ctx, base, key, model, blob, path, hdr, timeout, op)
	if err != nil {
		return nil
	}
	return &reading{tokens(doc), stopReason(doc)}
}

const (
	// BisectTolerance: 2% of the window is well inside the headroom Claude Code
	// already leaves, so narrowing further buys nothing a session can feel.
	bisectTolerance = 0.02
	// BisectMaxAccepts is THE cost control. A refused over-limit request is free
	// (rejected before inference); an accepted one is billed for its whole payload.
	// One accept lands 6-28% low, two within a few percent, and understating is
	// safe here because the registry value only lowers the compaction trigger.
	BisectMaxAccepts = 1
	// Probes lean high in the remaining range: a probe above the ceiling is
	// refused and therefore free.
	bisectBias = 0.75
	// BisectMaxSteps bounds wall clock and a flapping endpoint, not spend.
	bisectMaxSteps = 32
)

// Sender asks the gateway to take one prefix of the oversized payload and
// answers "refused", "accepted", "timeout" or another outcome naming a failure.
type Sender func(blob string) (kind string, reported int64)

// BisectCeiling finds a ceiling a gateway enforces but will not state, from
// refusals alone. It returns (value, note, accepts); value is 0 when none was
// found. The value is a size the provider ACTUALLY accepted, never the
// midpoint of a range never sent, so it can only understate.
func BisectCeiling(send Sender, blob string, floor int, tolerance float64, maxAccepts, maxSteps int) (int64, string, int) {
	high := len(blob) / 4
	lo := 0    // largest size known to be ACCEPTED
	hi := high // smallest size known to be REFUSED
	type found struct {
		size     int
		reported int64
	}
	var best *found
	accepts, steps := 0, 0

	converged := func() bool {
		limit := int(float64(hi) * tolerance)
		if floor > limit {
			limit = floor
		}
		return lo != 0 && hi-lo <= limit
	}
	ask := func(size int) string {
		steps++
		kind, reported := send(blob[:size*4])
		switch kind {
		case "refused":
			if size < hi {
				hi = size
			}
		case "accepted":
			accepts++
			if size > lo {
				lo, best = size, &found{size, reported}
			}
		}
		return kind
	}

	// Phase 1: free descent. Every refusal here costs nothing.
	size := high / 2
	for steps < maxSteps && size >= floor {
		outcome := ask(size)
		if outcome == "refused" {
			size /= 2
			continue
		}
		if outcome == "accepted" {
			break
		}
		// A timeout must never be read as a ceiling: it says the REQUEST did not
		// finish, not that the prompt was too long.
		if outcome == "timeout" {
			return 0, fmt.Sprintf("search timed out after %d step(s); nothing was recorded. "+
				"Retry with a larger --probe-timeout and --deadline if needed; requests may be billed", steps), accepts
		}
		return 0, fmt.Sprintf("search abandoned after %d step(s): %s", steps, outcome), accepts
	}
	if best == nil {
		return 0, fmt.Sprintf("all %d requests were refused down to the estimated %s-token floor; no limit identified", steps, commas(int64(floor))), accepts
	}

	// Phase 2: narrow, biased toward free refusals.
	for steps < maxSteps && !converged() && accepts < maxAccepts {
		probe := lo + int(float64(hi-lo)*bisectBias)
		if probe <= lo || probe >= hi {
			break
		}
		// A timeout is not information: it narrowed nothing, so stop and keep
		// only what was actually probed.
		if k := ask(probe); k != "refused" && k != "accepted" {
			break
		}
	}
	gap := ""
	if !converged() {
		gap = fmt.Sprintf("; search stopped within the estimated %s–%s range, so this understates the unconfirmed ceiling", commas(int64(lo)), commas(int64(hi)))
	}
	if best.reported > 0 {
		return best.reported, fmt.Sprintf("%d search steps, %d accepted request(s), potentially billed; reported %s input tokens%s", steps, accepts, commas(best.reported), gap), accepts
	}
	return int64(best.size), fmt.Sprintf("%d search steps, %d accepted request(s), potentially billed; no token count reported, using a ~4 chars/token estimate%s", steps, accepts, gap), accepts
}

// ClassifyAccepted says what a 200 to an over-limit request means. Three
// outcomes arrive as 200: a refusal in a success envelope, a prompt that simply
// fitted, and one quietly trimmed. half is a second reading at half the size;
// if halving the input does not change what comes back, that plateau is the limit.
func ClassifyAccepted(full reading, half *reading) (kind string, value int64, note string) {
	if full.stop == "model_context_window_exceeded" {
		return "exceeded", 0, "stop_reason reports context overflow without a numeric limit"
	}
	if half == nil {
		return "inconclusive", 0, "oversized request accepted; confirmation failed, so the limit is unknown"
	}
	if full.reported == 0 || half.reported == 0 {
		return "inconclusive", 0, "no usable token counts reported"
	}
	// Halving the input should roughly halve the count; close to unchanged means
	// the model stopped reading at the same place both times.
	if float64(half.reported) > float64(full.reported)*0.9 {
		return "truncated", full.reported, fmt.Sprintf("token-count plateau near %s for full and half inputs; inferred limit", commas(full.reported))
	}
	return "intact", 0, fmt.Sprintf("accepted at least %s input tokens; upper limit not identified", commas(full.reported))
}

// ProbeResult is probe()'s answer: kind is prefixed with the shape that answered.
type ProbeResult struct {
	Kind  string
	Value int64 // 0 when none
	Note  string
}

// The six-byte JSON escapes a gateway wraps an upstream body with: a gateway
// escapes ">" so the comparison never sees a literal one (Python replaced
// "\u003e" and friends before matching).
var unicodeEscapes = strings.NewReplacer(`\u003e`, ">", `\u003c`, "<", `\u0026`, "&")

// Probe sends one oversized prompt on each API shape until one answers. A value
// read on either shape may be recorded: an adapter=true profile puts a
// translator in front of its endpoint.
func Probe(ctx context.Context, base, key, model, blob string, timeout time.Duration, op Opener, bisect int) ProbeResult {
	for _, sh := range shapes {
		sh := sh
		send := func(b string) (string, int64) {
			doc, err := postSizedFn(ctx, base, key, model, b, sh.path, sh.headers, timeout, op)
			var he *HTTPError
			switch {
			case errors.As(err, &he):
				detail := DecodeHexBodies(decodeReplace(he.Body))
				if (he.Code == 400 || he.Code == 413 || he.Code == 422) && contextOverflow.MatchString(detail) {
					return "refused", 0
				}
				return fmt.Sprintf("HTTP %d: not a context-overflow refusal", he.Code), 0
			case err != nil:
				// A timeout is NOT a refusal: it says the request did not finish.
				if IsTimeout(err) {
					return "timeout", 0
				}
				return errText(err), 0
			}
			if stopReason(doc) == "model_context_window_exceeded" {
				return "refused", 0
			}
			return "accepted", tokens(doc)
		}

		doc, err := postSizedFn(ctx, base, key, model, blob, sh.path, sh.headers, timeout, op)
		if err == nil {
			full := reading{tokens(doc), stopReason(doc)}
			var half *reading
			if full.stop != "model_context_window_exceeded" {
				// This second request can also incur input charges.
				half = reread(ctx, base, key, model, blob[:len(blob)/2], sh.path, sh.headers, timeout, op)
			}
			kind, value, note := ClassifyAccepted(full, half)
			return ProbeResult{sh.name + ":" + kind, value, note}
		}
		var he *HTTPError
		if !errors.As(err, &he) {
			// `repr(exc)` is what the code caught, not what happened: name the event
			// in words, and for a timeout say the probe is deliberately oversized.
			if IsTimeout(err) {
				return ProbeResult{sh.name + ":error", 0, "over-limit probe timed out; no further requests sent. " +
					"Retry with a larger --probe-timeout and --deadline if needed; the timed-out request may be billed"}
			}
			return ProbeResult{sh.name + ":error", 0, errText(err)}
		}
		detail := decodeReplace(he.Body)
		if parsed := parseDict(he.Body); parsed != nil {
			// Gateways wrap the provider's words: tencent puts the ceiling in
			// error.message and unicode-escapes the ">" in it.
			if s := firstText(parsed); s != "" {
				detail = s
			}
		}
		if shapeMismatch.MatchString(detail) || he.Code == 404 {
			continue
		}
		// A gateway that wraps the upstream body as text never json-parses, so
		// the escaped comparison inside it is normalized before the patterns run.
		detail = DecodeHexBodies(unicodeEscapes.Replace(detail))
		if n, ok := matchDeclared(detail); ok {
			return ProbeResult{sh.name + ":declared", n, ""}
		}
		// The refusal proves a ceiling exists BELOW what was sent; this gateway
		// just did not say where. That is what the search is for.
		if bisect > 0 && (he.Code == 400 || he.Code == 413 || he.Code == 422) && contextOverflow.MatchString(detail) {
			val, why, _ := BisectCeiling(send, blob, 1_000, bisectTolerance, bisect, bisectMaxSteps)
			if val > 0 {
				return ProbeResult{sh.name + ":bisected", val, why}
			}
			return ProbeResult{sh.name + ":opaque", 0, strings.Join(strings.Fields(truncRunes(detail, 200)), " ") + " — " + why}
		}
		return ProbeResult{sh.name + ":opaque", 0, strings.Join(strings.Fields(truncRunes(detail, 300)), " ")}
	}
	return ProbeResult{"unsupported", 0, "rejected on both shapes"}
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}

// firstText is `detailMsg or error_message or error.message`, the first truthy string.
func firstText(parsed *obj) string {
	for _, k := range []string{"detailMsg", "error_message"} {
		if s, ok := str(getv(parsed, k)); ok && s != "" {
			return s
		}
	}
	if e := asObj(getv(parsed, "error")); e != nil {
		if s, ok := str(getv(e, "message")); ok && s != "" {
			return s
		}
	}
	return ""
}

// errText is "<ClassName>: <message>" for a failure the probe did not expect.
func errText(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, ": "); i > 0 && !strings.ContainsAny(msg[:i], " <") && strings.HasSuffix(msg[:i], "Error") {
		return msg // already "ValueError: ..." style
	}
	var ue *URLError
	if errors.As(err, &ue) {
		return "URLError: " + ue.Error()
	}
	return msg
}
