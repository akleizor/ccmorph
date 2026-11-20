package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
	"github.com/akleizor/ccmorph/util"
)

// The Claude Code hooks. Each reads one event on stdin and never fails the
// tool call: a router error leaves the Agent call as the agent wrote it.

func hookOut(w io.Writer, v any) { _ = json.NewEncoder(w).Encode(v) }

// portableAgent: an agent defined outside this session's profile cannot run on another profile.
func portableAgent(agent string) bool {
	if agent == "" || agent == "general-purpose" || strings.Contains(agent, ":") {
		return true
	}
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if home == "" {
		h, _ := os.UserHomeDir()
		home = filepath.Join(h, ".claude")
	}
	cwd, _ := os.Getwd()
	for _, dir := range []string{filepath.Join(home, "agents"), filepath.Join(cwd, ".claude", "agents")} {
		if _, err := os.Stat(filepath.Join(dir, agent+".md")); err == nil {
			return true
		}
	}
	return false
}

// aliasFor is the Agent tool's model alias that runs model natively in this session.
func aliasFor(model string) string {
	for _, a := range []string{"haiku", "sonnet", "opus"} {
		if v := os.Getenv(util.TierEnvKey(a)); v != "" && strings.TrimSuffix(v, "[1m]") == model || v == "" && model == a {
			return a
		}
	}
	return ""
}

// PreToolUse routes an Agent call: a same-profile pick runs natively with the
// model and effort filled in; any other pick, or no pick, leaves the call as
// written. The hook never denies a call. An explicit model or a fork stays the caller's.
func PreToolUse(in io.Reader, w io.Writer) {
	var ev struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
		ToolUseID string         `json:"tool_use_id"`
	}
	if json.NewDecoder(in).Decode(&ev) != nil || ev.ToolName != "Agent" && ev.ToolName != "Task" {
		return
	}
	if os.Getenv(WorkerEnv) == "1" { // a worker's own subagents run as it asked
		return
	}
	str := func(k string) string { s, _ := ev.ToolInput[k].(string); return s }
	unrouted := func(reason string) {
		outcomeAppend(Row{"ts": nowSec(), "event": "unrouted", "reason": reason, "tool_use_id": ev.ToolUseID})
	}
	if str("model") != "" || str("subagent_type") == "fork" {
		unrouted("pinned")
		return
	}
	c, err := LoadConfig()
	if err != nil || !c.Enabled {
		return
	}
	session := SessionProfile()
	var only []string
	if !portableAgent(str("subagent_type")) {
		only = []string{session}
	}
	d, err := Decide(c, Request{Prompt: str("prompt"), Effort: str("effort"), Only: only, Deadline: time.Now().Add(16 * time.Second)})
	if err != nil || d.Pick == nil { // the router picks a model; it never blocks the call
		reason := "no profile serves this agent's kind and tier"
		if err != nil {
			reason = err.Error()
		}
		unrouted(reason)
		return
	}
	alias := ""
	if d.Pick.Profile == session {
		alias = aliasFor(d.Pick.Model)
	}
	switch {
	case alias == "" && only != nil:
		unrouted("session-only agent has no native alias")
		return
	case alias == "": // another profile's pick needs the delegate tool; never deny the call to force it
		unrouted("pick is on profile " + d.Pick.Profile)
		return
	}
	reserve(newID("native"), d, str("prompt"), Row{"tool_use_id": ev.ToolUseID, "native": true})
	input := map[string]any{}
	for k, v := range ev.ToolInput {
		input[k] = v
	}
	input["model"], input["effort"] = alias, d.Pick.Effort
	hookOut(w, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": input,
		"additionalContext": fmt.Sprintf("router: %s/%s → %s · %s · effort %s.", d.Task.Kind, d.Task.Tier, d.Pick.Profile, d.Pick.Model, d.Pick.Effort)}})
}

func resultText(v any) string {
	if blocks, ok := v.([]any); ok {
		var parts []string
		for _, b := range blocks {
			if m, ok := b.(map[string]any); ok {
				s, _ := m["text"].(string)
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	}
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}

// PostToolUse joins an Agent result to its route; an async launch stores its agent id.
func PostToolUse(in io.Reader, w io.Writer) {
	var ev struct {
		ToolName     string `json:"tool_name"`
		ToolUseID    string `json:"tool_use_id"`
		ToolResponse any    `json:"tool_response"`
		Error        any    `json:"error"`
	}
	if json.NewDecoder(in).Decode(&ev) != nil || ev.ToolName != "Agent" && ev.ToolName != "Task" {
		return
	}
	resp, ok := ev.ToolResponse.(map[string]any)
	if !ok {
		resp = map[string]any{"content": ev.ToolResponse}
	}
	if resp["status"] == "async_launched" {
		ledgerUpdate(ev.ToolUseID, Row{"agent_id": resp["agentId"]})
		return
	}
	c, err := LoadConfig()
	if err != nil {
		return
	}
	text, status := resultText(resp["content"]), fmt.Sprint(resp["status"])
	failed := ordjson.Truthy(resp["is_error"]) || ordjson.Truthy(resp["error"]) || ordjson.Truthy(ev.Error) || status == "failed" || status == "error"
	hard := hardFailure(text+" "+fmt.Sprint(resp["error"]), failed, status)
	if !failed {
		hard = hardFailure(text, false, status)
	}
	if hard == "" && failed {
		hard = "error" // Delegate's status != 0 fallback: a failed run is never a real answer
	}
	if hint := complete(c, ev.ToolUseID, text, hard); hint != "" {
		hookOut(w, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PostToolUse", "additionalContext": hint}})
	}
}

// SubagentStop joins a background agent's final answer to its route.
func SubagentStop(in io.Reader, w io.Writer) {
	var ev struct {
		AgentType      string `json:"agent_type"`
		AgentID        string `json:"agent_id"`
		ToolUseID      string `json:"tool_use_id"`
		LastMessage    any    `json:"last_assistant_message"`
		TranscriptPath string `json:"agent_transcript_path"`
		Error          any    `json:"error"`
		Status         string `json:"status"`
	}
	if json.NewDecoder(in).Decode(&ev) != nil || ev.AgentType == "" {
		return
	}
	text := resultText(ev.LastMessage)
	if text == "" && ev.TranscriptPath != "" {
		text = lastAssistant(ev.TranscriptPath)
	}
	c, err := LoadConfig()
	if err != nil {
		return
	}
	id := ev.AgentID
	if id == "" {
		id = ev.ToolUseID
	}
	failed := ordjson.Truthy(ev.Error) || ev.Status == "failed" || ev.Status == "error"
	hard := hardFailure(text, failed, ev.Status)
	if hard == "" && failed {
		hard = "error"
	}
	if hint := complete(c, id, text, hard); hint != "" {
		hookOut(w, map[string]any{"systemMessage": hint})
	}
}

func lastAssistant(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > 200 {
			lines = lines[1:]
		}
	}
	for _, line := range slices.Backward(lines) {
		var e struct {
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Message.Role == "assistant" {
			if t := resultText(e.Message.Content); t != "" {
				return t
			}
		}
	}
	return ""
}

var sinceRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)([hmd])$`)
