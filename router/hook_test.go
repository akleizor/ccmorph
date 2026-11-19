package router

import (
	"bytes"
	"encoding/json"
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
	if !strings.Contains(out.String(), `"permissionDecision":"deny"`) || !strings.Contains(out.String(), "delegate tool") {
		t.Fatalf("another profile's pick goes through delegate: %s", out.String())
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
