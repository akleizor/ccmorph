// Package quirks is the stock shim's gateway quirks: the
// request repairs and header rules that any Anthropic-compatible gateway may need.
// Each rule is decided by the request's structure or the HTTP status code, never
// by a gateway's error text. A custom build calls FromEnv and wraps the hooks it
// returns before handing them to shim.Main.
//
// Profile env (read by FromEnv):
//
//	CCMORPH_AFFINITY_HEADER=<name>   header that carries the Claude Code session id
//	CCMORPH_USER_AGENT=<template>    outbound User-Agent; {version} is the client's claude-cli version
//	CCMORPH_TRANSLATOR_CHAT_MODELS=<ids>        comma-separated chat-path model ids
//	CCMORPH_TRANSLATOR_CHAT_MODELS_FILE=<path>  the same ids in a file, reread on each resend
//	CCMORPH_QUIRKS=off               no hooks at all: the shim passes requests through untouched
//
// On by default: dangling tool_reference repair, and one resend without thinking
// blocks after a 400 on POST /v1/messages.
package quirks

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/akleizor/ccmorph/shim"
)

var (
	headerName = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	cliVersion = regexp.MustCompile(`claude-cli/(\d+\.\d+\.\d+)`)
	bareSemver = regexp.MustCompile(`^\s*(\d+\.\d+\.\d+)`)
)

// FromEnv builds the hooks from the profile env. A bad setting is an error the
// caller must surface at startup, never mid-flight.
func FromEnv() (shim.Hooks, error) { return FromGetenv(os.Getenv) }

// FromGetenv is FromEnv over any lookup, for the adapter that runs the shim in its own process.
func FromGetenv(getenv func(string) string) (shim.Hooks, error) {
	if strings.TrimSpace(getenv("CCMORPH_QUIRKS")) == "off" {
		return shim.Hooks{}, nil
	}
	h, err := build(getenv("CCMORPH_AFFINITY_HEADER"), getenv("CCMORPH_USER_AGENT"))
	if err != nil {
		return shim.Hooks{}, err
	}
	chat := modelSet(getenv("CCMORPH_TRANSLATOR_CHAT_MODELS"))
	file := strings.TrimSpace(getenv("CCMORPH_TRANSLATOR_CHAT_MODELS_FILE"))
	if len(chat) > 0 || file != "" {
		h.ResendBody = func(request, _ []byte) []byte { return stripThinking(request, chatNow(chat, file)) }
	}
	return h, nil
}

// chatNow is the translator's chat-path model set: the launch-time list plus the
// ids in file, which the adapter rewrites whenever it renders its config or
// prepares a model mid-session, so a shim that started before that model was
// classified still keeps its carrier.
func chatNow(static map[string]bool, file string) map[string]bool {
	if file == "" {
		return static
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return static
	}
	set := modelSet(string(b))
	for m := range static {
		set[m] = true
	}
	return set
}

// modelSet is a comma-, space- or newline-separated list of model ids as a set.
func modelSet(list string) map[string]bool {
	set := map[string]bool{}
	for _, m := range strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		if m != "" {
			set[m] = true
		}
	}
	return set
}

func build(affinity, uaTemplate string) (shim.Hooks, error) {
	affinity = strings.TrimSpace(affinity)
	uaTemplate = strings.TrimSpace(uaTemplate)
	if affinity != "" && !headerName.MatchString(affinity) {
		return shim.Hooks{}, fmt.Errorf("CCMORPH_AFFINITY_HEADER must be a header name of [A-Za-z0-9-], got %q", affinity)
	}
	h := shim.Hooks{RepairRequest: repairToolReferences, ResendBody: resendWithoutThinking}
	if affinity != "" {
		h.OutboundHeaders = func(hd http.Header, session string) {
			if session != "" {
				for k := range hd {
					if strings.EqualFold(k, affinity) {
						delete(hd, k)
					}
				}
				hd.Set(affinity, session)
			}
		}
	}
	if uaTemplate != "" {
		h.UserAgent = func(client string) string {
			m := cliVersion.FindStringSubmatch(client)
			if m == nil {
				return client
			}
			return strings.ReplaceAll(uaTemplate, "{version}", m[1])
		}
		h.TranslatorUserAgent = func() string {
			version := claudeVersion()
			if version == "" {
				return ""
			}
			return strings.ReplaceAll(uaTemplate, "{version}", version)
		}
	}
	return h, nil
}

// claudeVersion is `claude --version`'s semver, read once per process: the
// launch-time render and the shim subcommand both ask for it (#636).
var claudeVersion = sync.OnceValue(readClaudeVersion)

func readClaudeVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "--version").Output()
	if err != nil {
		return ""
	}
	m := bareSemver.FindSubmatch(out)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// --- JSON span helpers -------------------------------------------------------

func isStr(b []byte, s shim.Span, want string) bool {
	v, ok := shim.SpanStr(b, s)
	return ok && v == want
}

func objectAt(b []byte, s shim.Span) []shim.Member {
	if b[s.Start] != '{' {
		return nil
	}
	ms, _ := shim.Object(b, s.Start)
	return ms
}

func arrayAt(b []byte, s shim.Span) []shim.Span {
	if b[s.Start] != '[' {
		return nil
	}
	es, _ := shim.Array(b, s.Start)
	return es
}

// topObject is the body's top-level members, or false when it is not an object.
func topObject(b []byte) ([]shim.Member, bool) {
	i := 0
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	if i >= len(b) || b[i] != '{' {
		return nil, false
	}
	ms, _ := shim.Object(b, i)
	return ms, true
}

// --- dangling tool_reference -------------------------------------------------

// declaredToolNames covers plain tools ({"name": ...}) and grouped toolsets
// ({"tools": [...]}). Deliberately depth 2: a deeper walk would read a `name`
// property inside an input_schema as a tool.
func declaredToolNames(b []byte, top []shim.Member) map[string]bool {
	names := map[string]bool{}
	add := func(ms []shim.Member) {
		if sp, ok := shim.LastMember(ms, "name"); ok {
			if n, isString := shim.SpanStr(b, sp); isString {
				names[n] = true
			}
		}
	}
	tools, ok := shim.LastMember(top, "tools")
	if !ok {
		return names
	}
	for _, t := range arrayAt(b, tools) {
		ms := objectAt(b, t)
		add(ms)
		if sp, ok := shim.LastMember(ms, "tools"); ok {
			for _, m := range arrayAt(b, sp) {
				add(objectAt(b, m))
			}
		}
	}
	return names
}

// repairToolReferences turns every tool_reference inside a tool_result whose
// tool_name this request does not declare into a text block: such a reference
// earns the whole request a 400. Declared references and every other byte stay.
func repairToolReferences(body []byte) ([]byte, bool) {
	top, ok := topObject(body)
	if !ok {
		return nil, false
	}
	msgs, ok := shim.LastMember(top, "messages")
	if !ok {
		return nil, false
	}
	known := declaredToolNames(body, top)
	var edits []shim.Edit
	for _, m := range arrayAt(body, msgs) {
		blocks, ok := shim.LastMember(objectAt(body, m), "content")
		if !ok {
			continue
		}
		for _, block := range arrayAt(body, blocks) {
			ms := objectAt(body, block)
			if sp, ok := shim.LastMember(ms, "type"); !ok || !isStr(body, sp, "tool_result") {
				continue
			}
			content, ok := shim.LastMember(ms, "content")
			if !ok {
				continue // string content carries no blocks
			}
			for _, inner := range arrayAt(body, content) {
				ims := objectAt(body, inner)
				if sp, ok := shim.LastMember(ims, "type"); !ok || !isStr(body, sp, "tool_reference") {
					continue
				}
				text := "[tool_reference]"
				if sp, ok := shim.LastMember(ims, "tool_name"); ok {
					if name, isString := shim.SpanStr(body, sp); isString {
						if known[name] {
							continue
						}
						text = "[tool_reference: " + name + "]"
					}
				}
				edits = append(edits, shim.Edit{Start: inner.Start, End: inner.End,
					Raw: append(append([]byte(`{"type":"text","text":`), shim.Quote(text)...), '}')})
			}
		}
	}
	if len(edits) == 0 {
		return nil, false
	}
	return shim.Apply(body, edits), true
}

// --- thinking resend ---------------------------------------------------------

// carrierPrefix starts the signature of the thinking block in which the
// translator (akleizor/CLIProxyAPI from v8.0.76-akleizor.4) hands Claude Code a
// chat gateway's reasoning_details, Gemini's thought signatures among them, and
// reads them back from on the next request (#646).
const carrierPrefix = "cliproxy-reasoning-details:"

// resendWithoutThinking is the body to send once more after a 400: the request
// without thinking and redacted_thinking blocks in assistant turns (a gateway
// that spreads a conversation over backends cannot verify another's signatures).
// nil when the request carries none. The status code alone triggers it, so the
// response is not read.
func resendWithoutThinking(request, _ []byte) []byte { return stripThinking(request, nil) }

// stripThinking is resendWithoutThinking, except that when the request's model
// is in chat (the models the translator serves on its chat path, from
// CCMORPH_TRANSLATOR_CHAT_MODELS) the translator's carrier blocks stay: that
// route needs them back, and Gemini refuses a tool turn without them. Any other
// route strips them with the rest: a native Anthropic route rejects them.
func stripThinking(request []byte, chat map[string]bool) []byte {
	top, ok := topObject(request)
	if !ok {
		return nil
	}
	msgs, ok := shim.LastMember(top, "messages")
	if !ok {
		return nil
	}
	keepCarrier := false
	if sp, ok := shim.LastMember(top, "model"); ok {
		if m, isString := shim.SpanStr(request, sp); isString {
			keepCarrier = chat[strings.TrimSuffix(m, "[1m]")]
		}
	}
	var edits []shim.Edit
	for _, m := range arrayAt(request, msgs) {
		ms := objectAt(request, m)
		if sp, ok := shim.LastMember(ms, "role"); !ok || !isStr(request, sp, "assistant") {
			continue
		}
		content, ok := shim.LastMember(ms, "content")
		if !ok {
			continue
		}
		blocks := arrayAt(request, content)
		var kept [][]byte
		for _, b := range blocks {
			bm := objectAt(request, b)
			if sp, ok := shim.LastMember(bm, "type"); ok {
				t, _ := shim.SpanStr(request, sp)
				if t == "redacted_thinking" || (t == "thinking" && !(keepCarrier && isCarrier(request, bm))) {
					continue
				}
			}
			kept = append(kept, request[b.Start:b.End])
		}
		if len(kept) == len(blocks) {
			continue
		}
		raw := []byte(`[{"type":"text","text":"."}]`) // an assistant turn may not be empty
		if len(kept) > 0 {
			raw = append(append([]byte{'['}, bytes.Join(kept, []byte{','})...), ']')
		}
		edits = append(edits, shim.Edit{Start: content.Start, End: content.End, Raw: raw})
	}
	if len(edits) == 0 {
		return nil
	}
	fmt.Fprintln(os.Stderr, "shim: upstream answered 400; resending once without thinking blocks")
	return shim.Apply(request, edits)
}

// isCarrier is a block whose signature is the translator's carrier.
func isCarrier(b []byte, block []shim.Member) bool {
	sp, ok := shim.LastMember(block, "signature")
	if !ok {
		return false
	}
	s, isString := shim.SpanStr(b, sp)
	return isString && strings.HasPrefix(s, carrierPrefix)
}
