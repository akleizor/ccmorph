package router

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/quota"
	"github.com/akleizor/ccmorph/util"
)

func TestTheMCPServerListsDelegateAndRefusesAnEmptyPrompt(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"  "}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	ServeMCP(strings.NewReader(in), &out)
	replies := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		b, _ := json.Marshal(m["id"])
		replies[string(b)] = m
	}
	if len(replies) != 3 {
		t.Fatalf("want 3 replies, got %s", out.String())
	}
	if !strings.Contains(out.String(), `"name":"delegate"`) {
		t.Fatalf("tools/list: %s", out.String())
	}
	res := replies["3"]["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(out.String(), "non-empty prompt") {
		t.Fatalf("empty prompt: %v", res)
	}
}

func hookEnv(t *testing.T, enabled bool) string {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(WorkerEnv, "")
	for _, a := range []string{"OPUS", "SONNET", "HAIKU"} {
		t.Setenv(util.TierEnvKey(a), "")
	}
	for _, n := range []string{"here", "there"} {
		if err := os.WriteFile(filepath.Join(root, n+".conf"), []byte("type=sub\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SetEnabled(enabled); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "profiles", "here"))
	return root
}

func quotaSnapshot(t *testing.T, used map[string]float64) {
	reset := time.Now().Add(150 * time.Minute).Unix()
	var entries []quota.Entry
	for n, u := range used {
		entries = append(entries, quota.Entry{Profile: n, Kind: "claude", Windows: []quota.Window{{ID: "5h", Used: u, ResetsAt: &reset}}})
	}
	if err := writeJSON(filepath.Join(routerDir(), "quota.json"), snapshot{ReadAt: nowSec(), Entries: entries}); err != nil {
		t.Fatal(err)
	}
}

const agentCall = `{"tool_name":"Agent","tool_use_id":"tu1","tool_input":{"prompt":"Fix the bug in parse()","description":"fix"}}`

func TestPreToolUseRunsASameProfilePickNativelyAndSendsAnotherToDelegate(t *testing.T) {
	hookEnv(t, true)
	quotaSnapshot(t, map[string]float64{"here": 0.1, "there": 0.9})
	var out bytes.Buffer
	PreToolUse(strings.NewReader(agentCall), &out)
	if !strings.Contains(out.String(), `"updatedInput"`) || !strings.Contains(out.String(), `"model":"sonnet"`) {
		t.Fatalf("same-profile pick runs natively: %s", out.String())
	}
	if !ledgerUpdate("tu1", Row{"seen": true}) {
		t.Fatal("the native route is in the ledger under its tool_use_id")
	}
	quotaSnapshot(t, map[string]float64{"here": 0.9, "there": 0.1})
	out.Reset()
	PreToolUse(strings.NewReader(agentCall), &out)
	if out.Len() != 0 {
		t.Fatalf("another profile's pick never denies the call: %s", out.String())
	}
}

// A failed run whose text names no quota, auth or timeout is still not an
// answer: it must be ledgered as an error and back its model off, exactly as
// Delegate's status != 0 fallback does.
func TestAHookFailureWithUnmatchedTextIsLedgeredErrorAndBacksTheModelOff(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event string
		hook  func(io.Reader, io.Writer)
	}{
		{"post tool use", `{"tool_name":"Agent","tool_use_id":"tu1","tool_response":{"content":[{"type":"text","text":"the model stopped halfway"}],"is_error":true}}`, PostToolUse},
		{"subagent stop", `{"agent_type":"general-purpose","agent_id":"tu1","last_assistant_message":"the model stopped halfway","status":"failed"}`, SubagentStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hookEnv(t, true)
			ledgerAppend(Row{"id": "route-1", "tool_use_id": "tu1", "profile": "here", "model": "sonnet",
				"brief": "fix the bug", "kind": "code", "tier": "mid", "outcome": nil})
			var out bytes.Buffer
			tc.hook(strings.NewReader(tc.event), &out)
			rows := readRows(ledgerPath())
			if len(rows) != 1 {
				t.Fatalf("ledger rows: %v", rows)
			}
			if got := rows[0]["outcome"]; got != "error" {
				t.Fatalf("outcome %v, want error: a failure is not a real answer", got)
			}
			if rows[0]["quality"] != nil {
				t.Fatalf("the failure verdict reached the decider: %v", rows[0])
			}
			if unavailableUntil("here/sonnet") <= nowSec() {
				t.Fatal("the failed model is still routable")
			}
		})
	}
}

func TestHooksStayQuietWhileTheRouterIsOffOrTheModelIsPinned(t *testing.T) {
	hookEnv(t, false)
	quotaSnapshot(t, map[string]float64{"here": 0.9, "there": 0.1})
	var out bytes.Buffer
	PreToolUse(strings.NewReader(agentCall), &out)
	if out.Len() != 0 {
		t.Fatalf("router off: %s", out.String())
	}
	if err := SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	PreToolUse(strings.NewReader(`{"tool_name":"Agent","tool_input":{"prompt":"x","model":"opus"}}`), &out)
	if out.Len() != 0 {
		t.Fatalf("an explicit model is the caller's: %s", out.String())
	}
}

func TestAGatewayRefusalBacksOffTheModelAndASubscriptionRefusalTheAccount(t *testing.T) {
	root := hookEnv(t, true)
	if err := os.WriteFile(filepath.Join(root, "gw.conf"), []byte("type=apikey\nbase_url=http://127.0.0.1:9\nmodel=gemini-4-carbon\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig()
	ledgerAppend(Row{"id": "g", "profile": "gw", "model": "gemini-4-carbon"})
	complete(c, "g", "", "auth")
	if unavailableUntil("gw") != 0 || unavailableUntil("gw/gemini-4-carbon") == 0 {
		t.Fatal("a gateway's 403 must back off that model only, never the whole gateway")
	}
	ledgerAppend(Row{"id": "s", "profile": "there", "model": "claude-opus-5-5"})
	complete(c, "s", "", "quota")
	if unavailableUntil("there") == 0 {
		t.Fatal("a subscription's quota refusal is the account's")
	}
}

func TestPreToolUseNeverBlocksWhenNoProfileCanTakeTheCall(t *testing.T) {
	hookEnv(t, true) // "worker" is defined nowhere in this profile, so only the session profile may take it
	quotaSnapshot(t, map[string]float64{"here": 1, "there": 0.1})
	var out bytes.Buffer
	PreToolUse(strings.NewReader(`{"tool_name":"Agent","tool_use_id":"tu2","tool_input":{"prompt":"Fix the bug in parse()","subagent_type":"worker"}}`), &out)
	if strings.Contains(out.String(), `"deny"`) {
		t.Fatalf("an unplaceable call runs as asked, never denied: %s", out.String())
	}
}
