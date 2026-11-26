package router

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/akleizor/ccmorph/agentproxy"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
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

// aliasFor is the Agent tool's model alias that runs model natively in this
// session. A tier the session's env does not pin runs as the profile's own tier
// default, which is what a Remote Control edge serves when it passes the
// aliases through as real Claude ids.
func aliasFor(profile, model string) string {
	for _, a := range []string{"haiku", "sonnet", "opus"} {
		v := os.Getenv(util.TierEnvKey(a))
		if v == "" {
			v = profileTier(profile, a)
		}
		if v != "" && strings.TrimSuffix(v, "[1m]") == model || v == "" && model == a {
			return a
		}
	}
	return ""
}

func profileTier(profile, tier string) string {
	path, err := ctxwindow.ProfilePath(profile)
	if err != nil {
		return ""
	}
	_, f, err := ctxwindow.ReadConf(path)
	if err != nil {
		return ""
	}
	return f["env."+util.TierEnvKey(tier)]
}

// hookBudget is PreToolUse's deadline for the route and its ledger write, under
// the 20 s hooks.json timeout. A variable so a test can shorten it.
var hookBudget = 16 * time.Second

// PreToolUse routes an Agent call: a pick the session runs natively gets its
// alias and effort filled in; when the pick is not one, the router picks again
// among the session's native aliases, and only then, or with no pick, is the
// call left as written. The hook never denies a call. An explicit model or a fork stays the caller's.
func PreToolUse(in io.Reader, w io.Writer) {
	var ev struct {
		SessionID string         `json:"session_id"`
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
	// One window for both routes: hookBudget is an absolute deadline under the
	// hooks.json timeout, and Decide hands its deadline to every ledger write
	// that follows, so the second route shares the window the hook started
	// with instead of re-arming it (#873).
	deadline := time.Now().Add(hookBudget)
	d, err := Decide(c, Request{Prompt: str("prompt"), Effort: str("effort"), Only: only, Deadline: deadline})
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
		alias = aliasFor(session, d.Pick.Model)
	}
	route := func(alias string, extra Row, via string) {
		reserve(newID("native"), d, str("prompt"), extra)
		input := map[string]any{}
		for k, v := range ev.ToolInput {
			input[k] = v
		}
		if alias != "" {
			input["model"] = alias
		}
		input["effort"] = d.Pick.Effort
		hookOut(w, map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "updatedInput": input,
			"additionalContext": fmt.Sprintf("router: %s/%s → %s · %s · effort %s%s.", d.Task.Kind, d.Task.Tier, d.Pick.Profile, d.Pick.Model, d.Pick.Effort, via)}})
	}
	// An edge session's proxy serves any profile it holds: the pick runs as this
	// session's own subagent, its requests sent to the picked profile. The Agent
	// tool takes only Claude's aliases, so the model is left to the proxy.
	if alias == "" {
		if word, ok := edgeWord(d.Pick.Profile, deadline); ok && leavePick(ev.SessionID, str("prompt"), word, d.Pick.Model) == nil {
			route("", Row{"tool_use_id": ev.ToolUseID, "native": true, "edge": word}, " (through the edge)")
			return
		}
		// Any other ccmorph session runs behind the launcher's agent proxy,
		// which serves the profiles whose sign-in it can carry.
		if dir := os.Getenv(agentproxy.DirEnv); dir != "" && proxyServes(d.Pick.Profile) &&
			agentproxy.LeavePick(dir, ev.SessionID, str("prompt"), agentproxy.Pick{Word: d.Pick.Profile, Model: d.Pick.Model}) == nil {
			route("", Row{"tool_use_id": ev.ToolUseID, "native": true, "proxy": d.Pick.Profile}, " (through the agent proxy)")
			return
		}
	}
	if alias == "" && session != "" { // decide again among what this session runs natively
		d2, err := Decide(c, Request{Prompt: str("prompt"), Effort: str("effort"), Kind: d.Task.Kind, Tier: d.Task.Tier, Only: []string{session},
			Keep: func(o Option) bool { return aliasFor(session, o.Model) != "" }, Deadline: deadline})
		if err == nil && d2.Pick != nil {
			d2.Task = d.Task
			d, alias = d2, aliasFor(session, d2.Pick.Model)
		}
	}
	if alias == "" { // never deny the call to force a delegate
		unrouted("pick is on profile " + d.Pick.Profile)
		return
	}
	route(alias, Row{"tool_use_id": ev.ToolUseID, "native": true}, "")
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
	dropAgent(ev.AgentID)
	agentproxy.DropAgent(os.Getenv(agentproxy.DirEnv), ev.AgentID)
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
