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
	"github.com/akleizor/ccmorph/util/tier"
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
	for _, p := range []string{`"family"`, `"no_decider"`, `"refresh"`, "the finished answer is not scored",
		"the caller judges it", "also when profile and model are both set"} {
		if !strings.Contains(out.String(), p) {
			t.Fatalf("delegate schema lacks %s: %s", p, out.String())
		}
	}
	res := replies["3"]["result"].(map[string]any)
	if res["isError"] != true || !strings.Contains(out.String(), "non-empty prompt") {
		t.Fatalf("empty prompt: %v", res)
	}
}

func TestTheMCPServerRefusesArgumentsOfTheWrongTypeBeforeRouting(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","profile":42}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","no_decider":"yes"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","family":["sonnet"]}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","profile":null}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","no_decider":null}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"hi","refresh":null}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	ServeMCP(strings.NewReader(in), &out)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("want 6 refusals, got %s", out.String())
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q", line)
		}
		if m["result"] != nil {
			t.Fatalf("a mistyped argument reached routing: %s", line)
		}
		e, ok := m["error"].(map[string]any)
		if !ok || e["code"] != float64(-32602) {
			t.Fatalf("want -32602 invalid arguments, got %s", line)
		}
	}
	if _, err := os.Stat(filepath.Join(routerDir(), "outcomes.jsonl")); !os.IsNotExist(err) {
		t.Fatal("a refused call must never reach routing")
	}
}

func hookEnv(t *testing.T, enabled bool) string {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(WorkerEnv, "")
	for _, a := range []string{"OPUS", "SONNET", "HAIKU"} {
		t.Setenv(tier.TierEnvKey(a), "")
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
	// Decide arms the process-wide ledger budget from a hook's deadline; the
	// next test starts unarmed.
	t.Cleanup(func() { armLock(time.Time{}) })
	return root
}

func quotaSnapshot(t *testing.T, used map[string]float64) {
	reset := time.Now().Add(150 * time.Minute).Unix()
	var entries []quota.Entry
	for n, u := range used {
		u := u
		entries = append(entries, quota.Entry{Profile: n, Kind: "claude", Windows: []quota.Window{{ID: "5h", Used: &u, ResetsAt: &reset}}})
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

func TestAHookPathQuotaReadNeverRunsTheLiveSweep(t *testing.T) {
	hookEnv(t, true) // a fresh state dir: no stored quota snapshot
	calls := 0
	old := QuotaInput
	QuotaInput = func(names []string) string {
		calls++
		return ""
	}
	defer func() { QuotaInput = old }()
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decide(c, Request{Prompt: "Fix the bug in parse()", Deadline: time.Now().Add(16 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the hook started the live quota sweep %d time(s)", calls)
	}
	if notes := strings.Join(d.Notes, "\n"); !strings.Contains(notes, "no stored quota reading") {
		t.Fatalf("the missing reading is not named in the notes: %v", d.Notes)
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

// An undeclared spelling of a key never replaces the declared one: the tool
// takes the keys it declares, exactly, as the cli does.
func TestAnUndeclaredKeyNeverReplacesACallerSPin(t *testing.T) {
	dispatchEnv(t)
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"fix the parser","kind":"code","tier":"mid","profile":"gw","PROFILE":"missing-profile"}}}` + "\n"
	var out bytes.Buffer
	ServeMCP(strings.NewReader(in), &out)
	if strings.Contains(out.String(), "no ccmorph profile") {
		t.Fatalf("an undeclared key replaced the pin: %s", out.String())
	}
	if row := lastLedgerRow(t); row["profile"] != "gw" {
		t.Fatalf("the call must route on the pinned profile, ledgered %+v", row)
	}
}

// The arguments object is read by its exact key too: an "Arguments" spelling
// after the real one never replaces the caller's pins.
func TestAnUndeclaredArgumentsSpellingNeverReplacesTheArguments(t *testing.T) {
	dispatchEnv(t)
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delegate","arguments":{"prompt":"fix the parser","kind":"code","tier":"mid","profile":"sub"},"Arguments":{"prompt":"fix the parser"}}}` + "\n"
	var out bytes.Buffer
	ServeMCP(strings.NewReader(in), &out)
	if row := lastLedgerRow(t); row["profile"] != "sub" {
		t.Fatalf("the call must route on the pinned profile, ledgered %+v: %s", row, out.String())
	}
}

// GH-544: a tier pinned with the window marker in either case is the alias
// that runs its bare model natively.
func TestAliasForReadsAPinUnderEitherMarkerSpellingIssue544(t *testing.T) {
	for _, a := range []string{"opus", "sonnet", "haiku"} {
		t.Setenv(tier.TierEnvKey(a), "")
	}
	for _, pin := range []string{"gw-opus[1m]", "gw-opus[1M]"} {
		t.Setenv(tier.TierEnvKey("opus"), pin)
		if got := aliasFor("gw-opus"); got != "opus" {
			t.Errorf("pin %q: alias %q, want opus", pin, got)
		}
	}
}
