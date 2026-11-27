package agentproxy

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

// What the Remote Control edge does to a request body: which model it names
// under a target's tiers, the fields only first party accepts made portable,
// and the event uploads the phone downloads lightened.

// A pinned tier (`tier.fable=gpt-6-astra` in rc-hosts) reaches the session as
// ANTHROPIC_DEFAULT_FABLE_MODEL, which only covers the bare alias. The phone's
// model picker sends the full id (claude-fable-5-1), so the proxy pins the
// whole family.
var claudeFamily = regexp.MustCompile(`^claude-(fable|opus|sonnet|haiku)-\d`)

// pinnedModel is the model a pinned tier sends model to, "" when none does.
func pinnedModel(model string, tiers map[string]string) string {
	key := model
	if m := claudeFamily.FindStringSubmatch(model); m != nil {
		key = m[1]
	}
	// `[1m]` is Claude Code's own context marker, stripped before a request is
	// sent; a profile's tier carries it (claude-opus-5-5[1m]) and the backend
	// 404s on it.
	return strings.TrimSuffix(tiers[key], "[1m]")
}

// aliasOrder is the order a model shared by two aliases is read back in.
var aliasOrder = []string{"fable", "opus", "sonnet", "haiku"}

// routeAlias is the alias the route's own tiers map to model, "" when none.
func routeAlias(model string, routeTiers map[string]string) string {
	for _, a := range aliasOrder {
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
func (r modelRule) setModel(p *obj) bool {
	model, ok := p.str("model")
	if !ok {
		return false
	}
	if s := r.served(model); s != "" && s != model {
		p.set("model", s)
		return true
	}
	return false
}

// normalizeBody rewrites first-party-only fields of an inference body in place
// for a routed target; true if anything changed.
func normalizeBody(p *obj, r modelRule) bool {
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
	case *obj:
		if t, _ := n.str("type"); t == "image" {
			if src := n.obj("source"); src != nil {
				data, _ := src.str("data")
				if st, _ := src.str("type"); st == "base64" && len(data) > imageMin {
					mt, _ := src.str("media_type")
					if small, kind, ok := shrink(data, mt); ok && len(small) < len(data) {
						s := src.clone()
						s.set("data", small)
						s.set("media_type", kind)
						c := n.clone()
						c.set("source", s)
						return c, true
					}
				}
			}
			return n, false
		}
		if t, _ := n.str("type"); t == "text" {
			if text, ok := n.str("text"); ok {
				if cutText, changed := cut(text); changed {
					c := n.clone()
					c.set("text", cutText)
					return c, true
				}
			}
		}
	}
	return node, false
}

// truthy is Python's truth of a JSON value, for the signature field.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case bool:
		return x
	case []any:
		return len(x) > 0
	case *obj:
		return len(x.keys) > 0
	}
	return true // a number: 0 is the one that is not, and no signature is 0
}

// slimPayload lightens one event payload in place; true if it changed.
func slimPayload(payload *obj, shrink shrinker) bool {
	changed := false
	kind, _ := payload.str("type")
	if resp := payload.obj("response"); kind == "control_response" && resp != nil {
		if listing := resp.obj("response"); listing != nil {
			for _, key := range []string{"commands", "agents"} {
				for _, item := range listing.list(key) {
					if m, ok := item.(*obj); ok && m.del("description") {
						changed = true
					}
				}
			}
		}
	}
	var content []any
	if msg := payload.obj("message"); msg != nil {
		content = msg.list("content")
	}
	if kind == "assistant" {
		if payload.del("wire_tool_inputs") {
			changed = true
		}
		for _, b := range content {
			if m, ok := b.(*obj); ok && truthy(m.m["signature"]) {
				m.set("signature", "")
				changed = true
			}
		}
	}
	if kind == "user" {
		for _, b := range content {
			m, ok := b.(*obj)
			if !ok {
				continue
			}
			if t, _ := m.str("type"); t != "tool_result" {
				continue
			}
			// A tool_result written without `content` has always gained it (as
			// null); a present one is rewritten only when slimming changed it.
			_, has := m.get("content")
			slimmed, c := slimResult(m.m["content"], shrink)
			if !has || c {
				m.set("content", slimmed)
				changed = true
			}
		}
		switch r := payload.m["tool_use_result"].(type) {
		case *obj:
			for _, k := range duplicateOutput {
				if r.del(k) {
					changed = true
				}
			}
		case string:
			if c, ok := cut(r); ok {
				payload.set("tool_use_result", c)
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
	top, ok := body.(*obj)
	if !ok {
		return false
	}
	changed := false
	for _, e := range top.list("events") {
		if ev, ok := e.(*obj); ok {
			if p := ev.obj("payload"); p != nil {
				changed = slimPayload(p, shrink) || changed
			}
		}
	}
	return changed
}

// sipsShrink is a 1000 px JPEG of a base64 image, through macOS sips; ok false
// where sips is not there or fails.
func sipsShrink(data, _ string) (string, string, bool) {
	if runtime.GOOS != "darwin" {
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
	if exec.Command("sips", "-s", "format", "jpeg", "-s", "formatOptions", "70", "-Z", "1000", src, "--out", dst).Run() != nil {
		return "", "", false
	}
	out, err := os.ReadFile(dst)
	if err != nil {
		return "", "", false
	}
	return base64.StdEncoding.EncodeToString(out), "image/jpeg", true
}
