package agentproxy

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/ordjson"
)

func parsed(t *testing.T, body string) *ordjson.Obj {
	t.Helper()
	v, err := ordjson.Parse([]byte(body))
	o, _ := v.(*ordjson.Obj)
	if err != nil || o == nil {
		t.Fatalf("test body: %v", err)
	}
	return o
}

const (
	toolUse     = `{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]}`
	toolResult  = `{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}`
	humanPrompt = `{"role":"user","content":"fix it"}`
)

// test_midloop_is_judged_by_structure: real request shapes from Claude Code
// transcripts, last message only.
func TestAToolLoopIsToldFromANewTurnByTheFinalMessagesStructure(t *testing.T) {
	for _, c := range []struct {
		name string
		last string
		mid  bool
	}{
		{"tool_result only", `{"role":"user","content":[` + toolResult + `]}`, true},
		{"several tool_results", `{"role":"user","content":[` + toolResult + `,` + toolResult + `]}`, true},
		{"tool_result then a reminder text", `{"role":"user","content":[` + toolResult + `,{"type":"text","text":"<system-reminder>x</system-reminder>"}]}`, true},
		{"string prompt", `{"role":"user","content":"hello"}`, false},
		{"text blocks prompt", `{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"hello"}]}`, false},
		{"image and text prompt", `{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA"}},{"type":"text","text":"look"}]}`, false},
		{"stop hook feedback", `{"role":"user","content":"Stop hook feedback:\n[observe and fix]: keep going"}`, false},
		{"continue after resume", `{"role":"user","content":[{"type":"text","text":"Continue from where you left off."}]}`, false},
		{"last is the assistant", toolUse, false},
	} {
		body := `{"messages":[` + humanPrompt + `,` + toolUse + `,` + c.last + `]}`
		if got := midLoop(parsed(t, body)); got != c.mid {
			t.Errorf("%s: midLoop = %v, want %v", c.name, got, c.mid)
		}
	}
	// The earlier loop in the history does not matter, only the last message.
	hist := `{"messages":[` + humanPrompt + `,` + toolUse + `,{"role":"user","content":[` + toolResult + `]},` + toolUse + `,` + humanPrompt + `]}`
	if midLoop(parsed(t, hist)) {
		t.Error("a new human prompt after a finished loop is a new turn")
	}
	for _, bad := range []string{`{}`, `{"messages":[]}`, `{"messages":["x"]}`, `{"messages":[{"role":"user"}]}`} {
		if midLoop(parsed(t, bad)) {
			t.Errorf("%s is no loop", bad)
		}
	}
}

func TestPinsExpireAndStayBounded(t *testing.T) {
	var l loopPins
	now := time.Now()
	l.set("a", "gw", "", now)
	if p, ok := l.get("a", now.Add(pinTTL-time.Second)); !ok || p.target != "gw" {
		t.Fatalf("%+v %v", p, ok)
	}
	if _, ok := l.get("a", now.Add(pinTTL+time.Second)); ok {
		t.Fatal("an idle pin outlived its bound")
	}
	old := pinMax
	pinMax = 3
	defer func() { pinMax = old }()
	for i, k := range []string{"k1", "k2", "k3", "k4", "k5"} {
		l.set(k, "route", "", now.Add(time.Duration(i)*time.Second))
	}
	if len(l.m) != 3 {
		t.Fatalf("table holds %d, want 3", len(l.m))
	}
	if _, ok := l.get("k1", now); ok {
		t.Fatal("the oldest pin should be the one dropped")
	}
}

func loopBody(prompt bool) []byte {
	last := `{"role":"user","content":[` + toolResult + `]}`
	if prompt {
		last = `{"role":"user","content":"next question"}`
	}
	return []byte(`{"model":"claude-opus-5-5","messages":[` + humanPrompt + `,` + toolUse + `,` + last + `]}`)
}

// test_a_switch_waits_for_the_turn_boundary: a loop started on route A stays
// there when the held route becomes B, and the next new prompt goes to B.
func TestASwitchTakesEffectAtTheNextTurnNeverMidLoop(t *testing.T) {
	r := newEdgeRig(t, nil)
	h, _ := json.Marshal(map[string]any{"upstream": r.otherU, "key": "gw-key", "tiers": map[string]string{"opus": "gw-opus"}})
	write(t, filepath.Join(r.bridge, "switch-gw.json"), h)
	sess, agent := "s1", "agent-1"

	r.post(t, msgs, sess, "", "claude-opus-5-5", "start the loop") // a new turn on the route
	r.expect(t, r.route, "the route")
	r.word(t, "gw") // the switch lands mid-loop
	r.send(t, msgs, sess, "", loopBody(false), nil)
	r.expect(t, r.route, "the loop's own route")
	r.quiet(t, r.other, "the new profile")
	r.send(t, msgs, sess, "", loopBody(false), nil) // and keeps landing there
	r.expect(t, r.route, "the loop's own route")

	r.send(t, msgs, "s2", "", loopBody(false), nil) // a session with no pin (edge restart): the held route
	g := r.expect(t, r.other, "a pinless session")
	if g.body["model"] != "gw-opus" {
		t.Fatalf("%+v", g)
	}

	r.send(t, msgs, sess, "", loopBody(true), nil) // the next prompt: the switch
	r.expect(t, r.other, "the next turn")
	r.send(t, msgs, sess, "", loopBody(false), nil) // its loop now stays on the profile
	r.expect(t, r.other, "the new loop")

	r.word(t, "route") // a switch back waits the same way
	r.send(t, msgs, sess, "", loopBody(false), nil)
	r.expect(t, r.other, "the loop's own profile")
	// A stop hook's text continuation is a new turn: the switch lands.
	hook := []byte(`{"model":"claude-opus-5-5","messages":[` + humanPrompt + `,` + toolUse + `,{"role":"user","content":"Stop hook feedback: go on"}]}`)
	r.send(t, msgs, sess, "", hook, nil)
	r.expect(t, r.route, "the turn after the hook")

	// A subagent's loop is its own: it neither inherits nor disturbs the main one.
	r.word(t, "gw")
	r.send(t, msgs, sess, agent, loopBody(true), nil) // the subagent starts fresh on gw
	r.expect(t, r.other, "the subagent")
	r.word(t, "route")
	r.send(t, msgs, sess, "", loopBody(false), nil) // main loop is still on the route
	r.expect(t, r.route, "the main loop")
	r.send(t, msgs, sess, agent, loopBody(false), nil) // the subagent's still on gw
	r.expect(t, r.other, "the subagent's loop")
}
