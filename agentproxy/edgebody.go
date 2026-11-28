package agentproxy

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util/modelid"
)

// What the Remote Control edge does to a request body: which model it names
// under a target's tiers, the fields only first party accepts made portable,
// and the event uploads the phone downloads lightened.

// A pinned tier (`tier.fable=gpt-6-astra` in rc-hosts) reaches the session as
// the tier's env key (modelid.TierEnvKey), which only covers the bare alias. The phone's
// model picker sends the full id (claude-fable-5-1), so the proxy pins the
// whole family.
var claudeFamily = regexp.MustCompile(`^claude-(` + strings.Join(modelid.TierWords, "|") + `)-\d`)

// pinnedModel is the model a pinned tier sends model to, "" when none does.
func pinnedModel(model string, tiers map[string]string) string {
	key := model
	if m := claudeFamily.FindStringSubmatch(model); m != nil {
		key = m[1]
	}
	// `[1m]` is Claude Code's own context marker, stripped before a request is
	// sent; a profile's tier carries it (claude-opus-5-5[1m]) and the backend
	// 404s on it.
	return modelid.TrimWindowMarker(tiers[key])
}

// routeAlias is the alias the route's own tiers map to model, "" when none.
func routeAlias(model string, routeTiers map[string]string) string {
	for _, a := range modelid.TierWords {
		if routeTiers[a] == model {
			return a
		}
	}
	for a, m := range routeTiers {
		if m == model {
			return a
		}
	}
	return ""
}

// modelRule is how a body's model is read: the target's tiers, the route's own
// (the edge's, from its start), whether this target is that route, and whether
// a real Claude id the phone picked is pinned by family too.
type modelRule struct {
	tiers, routeTiers map[string]string
	isRoute           bool
	family            bool
}

// served is the model a request for model is served as, "" when it stays.
//
// A session's env pins the route's out-of-family tiers on the bare alias, so
// under another target its fable arrives as the route's model (gpt-6-astra):
// that id is read back to its alias first, then served as the target's own
// model for it. family=false stops there: a real Claude id the phone picked
// stays as picked.
func (r modelRule) served(model string) string {
	if len(r.routeTiers) > 0 && !r.isRoute {
		if alias := routeAlias(model, r.routeTiers); alias != "" {
			if own := pinnedModel(alias, r.tiers); own != "" {
				return own
			}
		}
	}
	if r.family {
		return pinnedModel(model, r.tiers)
	}
	return ""
}

// setModel points the body's model at what served says; true if it changed.
func (r modelRule) setModel(p *ordjson.Obj) bool {
	model, ok := str(p, "model")
	if !ok {
		return false
	}
	if s := r.served(model); s != "" && s != model {
		p.Set("model", s)
		return true
	}
	return false
}

// normalizeBody rewrites first-party-only fields of an inference body in place
// for a routed target; true if anything changed.
func normalizeBody(p *ordjson.Obj, r modelRule) bool {
	r.family = true
	changed := r.setModel(p)
	return makePortable(p, false) || changed
}

// The phone opens a session by downloading every event the host ever uploaded,
// so event weight is open time (one session: 139 MB, 80 s). The app only
// renders this copy; the model reads its local transcript, which this never
// touches. What can go without changing what the phone shows:
//   - each app open re-uploads the full command and agent list (~55 KB, 927
//     times); names stay, descriptions go
//   - thinking signatures: only the model API checks them, from the local copy
//   - tool_use_result's copies of the output (stdout, file text, the whole file
//     before an edit), already in the message; its diffs stay
//   - wire_tool_inputs: a second copy of the tool inputs already in the message
//   - tool output past textMax and full-size screenshots: unreadable on a phone
const (
	textMax  = 4000    // chars of tool output kept per block
	imageMin = 100_000 // base64 chars; smaller images are left as they are
	cutNote  = "\n… [trimmed for Remote Control; full output is on the Mac]"
)

var (
	cutLen          = utf8.RuneCountInString(cutNote)
	duplicateOutput = []string{"originalFile", "stdout", "stderr", "file", "content"}
)

// shrinker is a smaller (base64, media type) of a base64 image, ok false when it cannot.
type shrinker func(data, mediaType string) (string, string, bool)

func cut(s string) (string, bool) {
	if len(s) <= textMax+cutLen { // bytes are at least chars: short text needs no count
		return s, false
	}
	if utf8.RuneCountInString(s) <= textMax+cutLen {
		return s, false
	}
	return string([]rune(s)[:textMax]) + cutNote, true
}

// slimResult is a tool result's content with long text cut and big screenshots
// shrunk; (content, changed).
func slimResult(node any, shrink shrinker) (any, bool) {
	switch n := node.(type) {
	case string:
		return cut(n)
	case []any:
		out, changed := make([]any, len(n)), false
		for i, item := range n {
			var c bool
			out[i], c = slimResult(item, shrink)
			changed = changed || c
		}
		return out, changed
	case *ordjson.Obj:
		if t, _ := str(n, "type"); t == "image" {
			if src := child(n, "source"); src != nil {
				data, _ := str(src, "data")
				if st, _ := str(src, "type"); st == "base64" && len(data) > imageMin {
					mt, _ := str(src, "media_type")
					if small, kind, ok := shrink(data, mt); ok && len(small) < len(data) {
						s := ordjson.Clone(src).(*ordjson.Obj)
						s.Set("data", small)
						s.Set("media_type", kind)
						c := ordjson.Clone(n).(*ordjson.Obj)
						c.Set("source", s)
						return c, true
					}
				}
			}
			return n, false
		}
		if t, _ := str(n, "type"); t == "text" {
			if text, ok := str(n, "text"); ok {
				if cutText, changed := cut(text); changed {
					c := ordjson.Clone(n).(*ordjson.Obj)
					c.Set("text", cutText)
					return c, true
				}
			}
		}
	}
	return node, false
}

// filled is Python's truth of a JSON value, for the signature field.
func filled(v any) bool {
	if o, ok := v.(*ordjson.Obj); ok {
		return o.Len() > 0
	}
	return ordjson.Truthy(v)
}

// slimPayload lightens one event payload in place; true if it changed.
func slimPayload(payload *ordjson.Obj, shrink shrinker) bool {
	changed := false
	kind, _ := str(payload, "type")
	if resp := child(payload, "response"); kind == "control_response" && resp != nil {
		if listing := child(resp, "response"); listing != nil {
			for _, key := range []string{"commands", "agents"} {
				for _, item := range list(listing, key) {
					if m, ok := item.(*ordjson.Obj); ok && del(m, "description") {
						changed = true
					}
				}
			}
		}
	}
	var content []any
	if msg := child(payload, "message"); msg != nil {
		content = list(msg, "content")
	}
	if kind == "assistant" {
		if del(payload, "wire_tool_inputs") {
			changed = true
		}
		for _, b := range content {
			if m, ok := b.(*ordjson.Obj); ok && filled(field(m, "signature")) {
				m.Set("signature", "")
				changed = true
			}
		}
	}
	if kind == "user" {
		for _, b := range content {
			m, ok := b.(*ordjson.Obj)
			if !ok {
				continue
			}
			if t, _ := str(m, "type"); t != "tool_result" {
				continue
			}
			// A tool_result written without `content` has always gained it (as
			// null); a present one is rewritten only when slimming changed it.
			_, has := m.Get("content")
			slimmed, c := slimResult(field(m, "content"), shrink)
			if !has || c {
				m.Set("content", slimmed)
				changed = true
			}
		}
		switch r := field(payload, "tool_use_result").(type) {
		case *ordjson.Obj:
			for _, k := range duplicateOutput {
				if del(r, k) {
					changed = true
				}
			}
		case string:
			if c, ok := cut(r); ok {
				payload.Set("tool_use_result", c)
				changed = true
			}
		}
	}
	return changed
}

// slimEvents lightens a worker/events upload in place; true if anything
// changed. The flag comes from the walk itself: encoding a multi-MB body a
// second time just to diff it against the first doubled the cost of every
// session open.
func slimEvents(body any, shrink shrinker) bool {
	top, ok := body.(*ordjson.Obj)
	if !ok {
		return false
	}
	changed := false
	for _, e := range list(top, "events") {
		if ev, ok := e.(*ordjson.Obj); ok {
			if p := child(ev, "payload"); p != nil {
				changed = slimPayload(p, shrink) || changed
			}
		}
	}
	return changed
}

// imageSlots is how many images the edge shrinks at once, across every upload:
// each is a sips process, and a phone opening a long session uploads many.
var imageSlots = make(chan struct{}, 2)

// sips is the command that shrinks an image (a test points it elsewhere).
var sips = "sips"

// sipsShrink is a 1000 px JPEG of a base64 image, through macOS sips; ok false
// where sips is not there, fails, or ctx ends first (waiting for a slot or
// while it runs).
func sipsShrink(ctx context.Context, data, _ string) (string, string, bool) {
	if runtime.GOOS != "darwin" {
		return "", "", false
	}
	select {
	case imageSlots <- struct{}{}:
		defer func() { <-imageSlots }()
	case <-ctx.Done():
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return "", "", false
	}
	tmp, err := os.MkdirTemp("", "rc-shrink.*")
	if err != nil {
		return "", "", false
	}
	defer os.RemoveAll(tmp)
	src, dst := filepath.Join(tmp, "in"), filepath.Join(tmp, "out.jpg")
	if os.WriteFile(src, raw, 0o600) != nil {
		return "", "", false
	}
	cmd := exec.CommandContext(ctx, sips, "-s", "format", "jpeg", "-s", "formatOptions", "70", "-Z", "1000", src, "--out", dst)
	cmd.WaitDelay = time.Second // a killed sips's pipes never hold the upload
	if cmd.Run() != nil || ctx.Err() != nil {
		return "", "", false
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		return "", "", false
	}
	return base64.StdEncoding.EncodeToString(out), "image/jpeg", true
}
