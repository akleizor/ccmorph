package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// The plugin's one stdio MCP tool, delegate. JSON-RPC 2.0, one message per
// line, as the MCP stdio transport defines it.

const delegateDoc = `Run delegated work and return only its final answer. Use this instead of the Agent tool for delegated work; send independent briefs as parallel calls. Keep the Agent tool for work that needs this session.

Set kind and tier yourself: you know the task. tier small = search, read, list, summarise or a mechanical edit; mid = ordinary coding or investigation; top = research, design or hard multi-step debugging. Omit them only when unsure; the router then guesses from the prompt. Leave profile and model unset unless the user pinned them.

family, no_decider and refresh do on this tool exactly what --family, --no-decider and --refresh do on ccmorph router dispatch: family restricts the shortlist to one model family and the call errors when none qualifies or when profile and model are both set; no_decider never asks the decider (kind and tier come from the keyword rules, an unlabeled model is labeled by its name, and the finished answer is not scored: the caller judges it); refresh reads quota live instead of the snapshot cached up to 5 minutes, also when profile and model are both set.`

// delegateArgs decodes a tools/call payload by type, so a value of the wrong
// type (profile: 42, no_decider: "yes") fails the unmarshal and the call is
// refused as bad arguments before routing, never routed with the pin silently
// dropped.
type delegateArgs struct {
	Prompt    string `json:"prompt"`
	Kind      string `json:"kind"`
	Tier      string `json:"tier"`
	Profile   string `json:"profile"`
	Model     string `json:"model"`
	Cwd       string `json:"cwd"`
	Family    string `json:"family"`
	NoDecider bool   `json:"no_decider"`
	Refresh   bool   `json:"refresh"`
}

// decodeDelegateArgs fills a by the keys it declares, exactly: encoding/json
// would match "PROFILE" to the profile field case-insensitively and let an
// undeclared spelling replace a caller's pin, which the CLI never does. A
// declared key of the wrong type refuses the call; an undeclared key is read
// as the string the old map[string]string took, so no value type widens.
func decodeDelegateArgs(js json.RawMessage) (delegateArgs, bool) {
	var a delegateArgs
	if len(js) == 0 || string(js) == "null" {
		return a, true
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(js, &raw) != nil {
		return a, false
	}
	declared := map[string]any{"prompt": &a.Prompt, "kind": &a.Kind, "tier": &a.Tier,
		"profile": &a.Profile, "model": &a.Model, "cwd": &a.Cwd, "family": &a.Family,
		"no_decider": &a.NoDecider, "refresh": &a.Refresh}
	for k, v := range raw {
		if dst, ok := declared[k]; ok {
			// encoding/json takes null into a string or bool as "no value": refuse
			// it, or {"profile": null} would route with the pin silently dropped.
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) || json.Unmarshal(v, dst) != nil {
				return delegateArgs{}, false
			}
			continue
		}
		var s string
		if json.Unmarshal(v, &s) != nil {
			return delegateArgs{}, false
		}
	}
	return a, true
}

// heartbeat: Claude Code aborts a tool call that sends no progress for 1800 s.
var heartbeat = 60 * time.Second

func delegateSchema() map[string]any {
	str := map[string]any{"type": "string"}
	return map[string]any{
		"name": "delegate", "description": delegateDoc,
		"inputSchema": map[string]any{"type": "object", "required": []string{"prompt"}, "properties": map[string]any{
			"prompt": str, "kind": map[string]any{"type": "string", "enum": Kinds},
			"tier": map[string]any{"type": "string", "enum": Tiers}, "profile": str, "model": str, "cwd": str,
			"family": str, "no_decider": map[string]any{"type": "boolean"}, "refresh": map[string]any{"type": "boolean"},
		}},
	}
}

type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   any             `json:"error,omitempty"`
}

// ServeMCP answers MCP requests on in until it closes. Each tools/call runs in
// its own goroutine; notifications/cancelled kills that call's worker.
func ServeMCP(in io.Reader, out io.Writer) {
	var mu sync.Mutex
	send := func(m rpc) {
		m.JSONRPC = "2.0"
		b, _ := json.Marshal(m)
		mu.Lock()
		out.Write(append(b, '\n'))
		mu.Unlock()
	}
	calls := map[string]context.CancelFunc{}
	var callsMu sync.Mutex
	var wg sync.WaitGroup
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var m rpc
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if p.ProtocolVersion == "" {
				p.ProtocolVersion = "2025-06-18"
			}
			send(rpc{ID: m.ID, Result: map[string]any{"protocolVersion": p.ProtocolVersion,
				"capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo":   map[string]any{"name": "ccmorph-router", "version": "1"}}})
		case "ping":
			send(rpc{ID: m.ID, Result: map[string]any{}})
		case "tools/list":
			send(rpc{ID: m.ID, Result: map[string]any{"tools": []any{delegateSchema()}}})
		case "notifications/cancelled":
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			_ = json.Unmarshal(m.Params, &p)
			callsMu.Lock()
			if cancel := calls[string(p.RequestID)]; cancel != nil {
				cancel()
			}
			callsMu.Unlock()
		case "tools/call":
			var p struct {
				Name string `json:"name"`
				Meta struct {
					ProgressToken json.RawMessage `json:"progressToken"`
				} `json:"_meta"`
			}
			// arguments by its exact key: a struct field would also take
			// "Arguments" and let that spelling replace the caller's pins.
			var params map[string]json.RawMessage
			if json.Unmarshal(m.Params, &p) != nil || json.Unmarshal(m.Params, &params) != nil || p.Name != "delegate" {
				send(rpc{ID: m.ID, Error: map[string]any{"code": -32602, "message": "unknown tool or bad arguments"}})
				continue
			}
			a, ok := decodeDelegateArgs(params["arguments"])
			if !ok {
				send(rpc{ID: m.ID, Error: map[string]any{"code": -32602, "message": "unknown tool or bad arguments"}})
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			key := string(m.ID)
			callsMu.Lock()
			calls[key] = cancel
			callsMu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { callsMu.Lock(); delete(calls, key); callsMu.Unlock(); cancel() }()
				if len(p.Meta.ProgressToken) > 0 {
					go func() {
						for n := 1; ; n++ {
							select {
							case <-ctx.Done():
								return
							case <-time.After(heartbeat):
								send(rpc{Method: "notifications/progress", Params: mustJSON(map[string]any{
									"progressToken": p.Meta.ProgressToken, "progress": n})})
							}
						}
					}()
				}
				answer := Delegate(ctx, Request{Prompt: a.Prompt, Kind: a.Kind, Tier: a.Tier,
					Profile: a.Profile, Model: a.Model, Family: a.Family,
					NoDecider: a.NoDecider, Refresh: a.Refresh}, a.Cwd)
				result := "ok"
				switch {
				case answer == abortedAnswer:
					result = "aborted"
				case len(answer) > 7 && answer[:7] == "Error: ":
					result = "error"
				}
				outcomeAppend(Row{"ts": nowSec(), "event": "delegate", "result": result})
				if result == "aborted" {
					return // the client gave up; it expects no answer
				}
				send(rpc{ID: m.ID, Result: map[string]any{"content": []any{map[string]any{"type": "text", "text": answer}},
					"isError": result == "error"}})
			}()
		default:
			if len(m.ID) > 0 && m.Method != "" {
				send(rpc{ID: m.ID, Error: map[string]any{"code": -32601, "message": "method not found"}})
			}
		}
	}
	// stdin closed: the session ended, so its workers end too.
	callsMu.Lock()
	for _, cancel := range calls {
		cancel()
	}
	callsMu.Unlock()
	wg.Wait()
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// MCPMain is `ccmorph router mcp`.
func MCPMain() int { ServeMCP(os.Stdin, os.Stdout); return 0 }
