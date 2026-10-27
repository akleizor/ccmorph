// Package compat is the stock gateway-shim's generic gateway compatibility: the
// request repairs and header rules that any Anthropic-compatible gateway may need.
// Each rule is decided by the request's structure or the HTTP status code, never
// by a gateway's error text. A custom build calls FromEnv and wraps the hooks it
// returns before handing them to shim.Main.
//
// Profile env (read by FromEnv):
//
//	CCMORPH_AFFINITY_HEADER=<name>   header that carries the Claude Code session id
//	CCMORPH_USER_AGENT=<template>    outbound User-Agent; {version} is the client's claude-cli version
//
// Always on: dangling tool_reference repair, and one resend without thinking
// blocks after a 400 on POST /v1/messages.
package compat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
func FromEnv() (shim.Hooks, error) {
	return build(os.Getenv("CCMORPH_AFFINITY_HEADER"), os.Getenv("CCMORPH_USER_AGENT"))
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
			return strings.ReplaceAll(uaTemplate, "{version}", string(m[1]))
		}
	}
	return h, nil
}

// --- JSON span helpers -------------------------------------------------------

func member(ms []shim.Member, key string) (shim.Span, bool) {
	for i := len(ms) - 1; i >= 0; i-- {
		if ms[i].Key == key {
			return ms[i].Value, true
		}
	}
	return shim.Span{}, false
}

func strOf(b []byte, s shim.Span) (string, bool) {
	if b[s.Start] != '"' {
		return "", false
	}
	return shim.Unquote(b[s.Start:s.End]), true
}

func isStr(b []byte, s shim.Span, want string) bool {
	v, ok := strOf(b, s)
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
		if sp, ok := member(ms, "name"); ok {
			if n, isString := strOf(b, sp); isString {
				names[n] = true
			}
		}
	}
	tools, ok := member(top, "tools")
	if !ok {
		return names
	}
	for _, t := range arrayAt(b, tools) {
		ms := objectAt(b, t)
		add(ms)
		if sp, ok := member(ms, "tools"); ok {
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
	msgs, ok := member(top, "messages")
	if !ok {
		return nil, false
	}
	known := declaredToolNames(body, top)
	var edits []shim.Edit
	for _, m := range arrayAt(body, msgs) {
		blocks, ok := member(objectAt(body, m), "content")
		if !ok {
			continue
		}
		for _, block := range arrayAt(body, blocks) {
			ms := objectAt(body, block)
			if sp, ok := member(ms, "type"); !ok || !isStr(body, sp, "tool_result") {
				continue
			}
			content, ok := member(ms, "content")
			if !ok {
				continue // string content carries no blocks
			}
			for _, inner := range arrayAt(body, content) {
				ims := objectAt(body, inner)
				if sp, ok := member(ims, "type"); !ok || !isStr(body, sp, "tool_reference") {
					continue
				}
				text := "[tool_reference]"
				if sp, ok := member(ims, "tool_name"); ok {
					if name, isString := strOf(body, sp); isString {
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

// resendWithoutThinking is the body to send once more after a 400: the request
// without thinking and redacted_thinking blocks in assistant turns (a gateway
// that spreads a conversation over backends cannot verify another's signatures).
// nil when the request carries none. The status code alone triggers it, so the
// response is not read.
func resendWithoutThinking(request, _ []byte) []byte {
	top, ok := topObject(request)
	if !ok {
		return nil
	}
	msgs, ok := member(top, "messages")
	if !ok {
		return nil
	}
	var edits []shim.Edit
	for _, m := range arrayAt(request, msgs) {
		ms := objectAt(request, m)
		if sp, ok := member(ms, "role"); !ok || !isStr(request, sp, "assistant") {
			continue
		}
		content, ok := member(ms, "content")
		if !ok {
			continue
		}
		blocks := arrayAt(request, content)
		var kept [][]byte
		for _, b := range blocks {
			if sp, ok := member(objectAt(request, b), "type"); ok {
				if t, _ := strOf(request, sp); t == "thinking" || t == "redacted_thinking" {
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
	fmt.Fprintln(os.Stderr, "gateway-shim: upstream answered 400; resending once without thinking blocks")
	return shim.Apply(request, edits)
}
