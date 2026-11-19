package router

import (
	"bufio"
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

Set kind and tier yourself: you know the task. tier small = search, read, list, summarise or a mechanical edit; mid = ordinary coding or investigation; top = research, design or hard multi-step debugging. Omit them only when unsure; the router then guesses from the prompt. Leave profile and model unset unless the user pinned them.`

// heartbeat: Claude Code aborts a tool call that sends no progress for 1800 s.
var heartbeat = 60 * time.Second

func delegateSchema() map[string]any {
	str := map[string]any{"type": "string"}
	return map[string]any{
		"name": "delegate", "description": delegateDoc,
		"inputSchema": map[string]any{"type": "object", "required": []string{"prompt"}, "properties": map[string]any{
			"prompt": str, "kind": map[string]any{"type": "string", "enum": Kinds},
			"tier": map[string]any{"type": "string", "enum": Tiers}, "profile": str, "model": str, "cwd": str,
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
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
				Meta      struct {
					ProgressToken json.RawMessage `json:"progressToken"`
				} `json:"_meta"`
			}
			if json.Unmarshal(m.Params, &p) != nil || p.Name != "delegate" {
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
				a := p.Arguments
				answer := Delegate(ctx, a["prompt"], a["kind"], a["tier"], a["profile"], a["model"], a["cwd"])
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
