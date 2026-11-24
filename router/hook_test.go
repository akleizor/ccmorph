package router

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	t.Setenv("CCMORPH_EDGE_ROUTE_ENV", "") // the suite may itself run inside an edge session
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
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

func TestPreToolUseRunsASameProfilePickNativelyAndAnotherProfilesPickFallsBackToTheSessionsOwn(t *testing.T) {
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
	if strings.Contains(out.String(), `"deny"`) || !strings.Contains(out.String(), "→ here · sonnet") {
		t.Fatalf("another profile's pick never denies the call; the session's own sonnet runs it: %s", out.String())
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

// gateProfile adds an API profile serving kimi-k3 (the user's first choice, with
// no native alias) beside a Claude sonnet and a small haiku-tier model.
func gateProfile(t *testing.T, root string) {
	t.Helper()
	conf := "model=kimi-k3\nenv.ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-5-5\nenv.ANTHROPIC_DEFAULT_HAIKU_MODEL=deepseek-v4.1-flash\n"
	if err := os.WriteFile(filepath.Join(root, "gate.conf"), []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigPath(), []byte(`{"enabled":true,"order":["kimi-k3"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// edgeSession makes this process a Remote Control edge session whose switch word is word.
func edgeSession(t *testing.T, root, word string, ports map[string]int) {
	t.Helper()
	bridge := filepath.Join(root, "profiles", ".rc-bridge")
	if err := os.MkdirAll(filepath.Join(bridge, "edges"), 0o700); err != nil {
		t.Fatal(err)
	}
	if word != "" {
		if err := os.WriteFile(filepath.Join(bridge, "inference"), []byte(word+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for route, port := range ports {
		if err := os.WriteFile(filepath.Join(bridge, "edges", route+".json"), []byte(fmt.Sprintf(`{"pid":1,"port":%d}`, port)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CCMORPH_EDGE_ROUTE_ENV", "CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47670")
}

// A Remote Control edge session keeps the account profile as its config dir
// while inference goes where the edge's switch word says; the router must
// route for the latter (2026-11-24: 160 Agent calls ran on the parent's Opus).
func TestSessionProfileInAnEdgeSessionIsWhereInferenceRuns(t *testing.T) {
	root := hookEnv(t, true)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:47670") // a user's own loopback proxy is not an edge
	if got := SessionProfile(); got != "here" {
		t.Fatalf("no edge record: want the config dir's profile, got %q", got)
	}
	for _, c := range []struct {
		word  string
		ports map[string]int
		want  string
	}{
		{"route", map[string]int{"llmgate": 47670, "other": 47100}, "llmgate"},
		{"", map[string]int{"llmgate": 47670}, "llmgate"}, // a missing switch file is "route"
		{"account", map[string]int{"llmgate": 47670}, "here"},
		{"there", map[string]int{"llmgate": 47670}, "there"},
		{"route", map[string]int{"other": 47100}, ""},
	} {
		os.RemoveAll(filepath.Join(root, "profiles", ".rc-bridge"))
		edgeSession(t, root, c.word, c.ports)
		if got := SessionProfile(); got != c.want {
			t.Errorf("word %q, edges %v: want %q, got %q", c.word, c.ports, c.want, got)
		}
	}
}

// A pick with no native alias is not the end of the call: the router decides
// again among the aliases the session runs, so the call never inherits the parent's model.
func TestPreToolUseDecidesAgainAmongNativeAliasesWhenThePickHasNone(t *testing.T) {
	root := hookEnv(t, true)
	gateProfile(t, root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "profiles", "gate"))
	t.Setenv(util.TierEnvKey("sonnet"), "claude-sonnet-5-5")
	var out bytes.Buffer
	PreToolUse(strings.NewReader(agentCall), &out)
	if !strings.Contains(out.String(), `"model":"sonnet"`) {
		t.Fatalf("the native sonnet runs the call: %q", out.String())
	}
	// With no native alias at all, the call is still left as written.
	t.Setenv(util.TierEnvKey("sonnet"), "gpt-x")
	t.Setenv(util.TierEnvKey("haiku"), "gpt-y")
	t.Setenv(util.TierEnvKey("opus"), "gpt-z")
	out.Reset()
	PreToolUse(strings.NewReader(agentCall), &out)
	if out.Len() != 0 {
		t.Fatalf("no native alias leaves the call as written: %s", out.String())
	}
}

// In an edge session the tier env is unset (the edge passes real Claude ids),
// so a pick of the route's sonnet id runs natively as the sonnet alias.
func TestPreToolUseInAnEdgeSessionRunsTheRoutesClaudeIdAsItsAlias(t *testing.T) {
	root := hookEnv(t, true)
	gateProfile(t, root)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(root, "profiles", "here"))
	edgeSession(t, root, "route", map[string]int{"gate": 47670})
	var out bytes.Buffer
	PreToolUse(strings.NewReader(agentCall), &out)
	if !strings.Contains(out.String(), `"model":"sonnet"`) {
		t.Fatalf("the edge route's sonnet runs the call: %q", out.String())
	}
}
