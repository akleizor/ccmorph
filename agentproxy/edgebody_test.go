package agentproxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustObj(t *testing.T, s string) *obj {
	t.Helper()
	v, err := parseJSON([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return v.(*obj)
}

func enc(t *testing.T, v any) string {
	t.Helper()
	b, err := encodeJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOrderedJSONKeepsKeyOrderAndNumbers(t *testing.T) {
	in := `{"z":1,"a":{"y":[1.50,true,null,"é<>&"],"b":12345678901234567890},"m":""}`
	if got := enc(t, mustObj(t, in)); got != in {
		t.Fatalf("round trip changed the body:\n got %s\nwant %s", got, in)
	}
	if _, err := parseJSON([]byte(`{"a":1} x`)); err == nil {
		t.Fatal("trailing data was accepted")
	}
}

// rc_bridge_test.test_first_party_only_thinking_display_is_made_portable
func TestFirstPartyOnlyThinkingDisplayIsMadePortable(t *testing.T) {
	p := mustObj(t, `{"thinking":{"type":"adaptive","display":"updates"}}`)
	if !normalizeBody(p, modelRule{}) {
		t.Fatal("nothing changed")
	}
	if got := enc(t, p); got != `{"thinking":{"type":"adaptive","display":"omitted"}}` {
		t.Fatal(got)
	}
}

// test_first_party_cache_scope_is_dropped_but_the_breakpoint_kept
func TestCacheScopeIsDroppedButTheBreakpointKept(t *testing.T) {
	cc := `{"type":"ephemeral","ttl":"1h","scope":"global"}`
	p := mustObj(t, `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b","cache_control":`+cc+`}],`+
		`"messages":[{"role":"user","content":[{"type":"text","text":"q","cache_control":`+cc+`}]}],`+
		`"tools":[{"name":"t","cache_control":`+cc+`}]}`)
	if !normalizeBody(p, modelRule{}) {
		t.Fatal("nothing changed")
	}
	out := enc(t, p)
	if strings.Contains(out, "scope") || !strings.Contains(out, `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Fatal(out)
	}
}

func turn(t *testing.T, tail, messages string) *obj {
	t.Helper()
	return mustObj(t, `{"model":"claude-opus-5-5","tools":[{"name":"t"}],"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=sdk-cli;`+tail+`"},{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral","ttl":"1h","scope":"global"}}],"messages":`+messages+`}`)
}

// test_consecutive_first_party_turns_share_a_cacheable_prefix: the route caches
// on the request prefix, so turn 2's body must literally start with turn 1's.
func TestConsecutiveTurnsShareACacheablePrefix(t *testing.T) {
	asked := `[{"role":"user","content":[{"type":"text","text":"q"}]}`
	t1 := turn(t, " cch=1f2e3; cc_prompt_id=11111111-1111-4111-8111-111111111111; cc_turn_origin=user; cc_prompt_index=0; cc_turn_index=1;", asked+`]`)
	t2 := turn(t, " cch=9a8b7; cc_prev_req=req_011CAbc; cc_prompt_id=22222222-2222-4222-8222-222222222222; cc_turn_origin=user; cc_prompt_index=1; cc_turn_index=2;",
		asked+`,{"role":"assistant","content":[{"type":"text","text":"a"}]},{"role":"user","content":[{"type":"text","text":"q2"}]}]`)
	if !normalizeBody(t1, modelRule{}) || !normalizeBody(t2, modelRule{}) {
		t.Fatal("a turn was left as it was")
	}
	j1, j2 := enc(t, t1), enc(t, t2)
	if !strings.HasPrefix(j2, j1[:len(j1)-2]) || j2[len(j1)-2] != ',' {
		t.Fatalf("turn 2 does not open with turn 1:\n%s\n%s", j1, j2)
	}
	if got, _ := t2.list("system")[0].(*obj).str("text"); got != "x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=sdk-cli;" {
		t.Fatal(got)
	}
}

// test_a_turn_field_never_takes_its_neighbour_with_it
func TestATurnFieldNeverTakesItsNeighbourWithIt(t *testing.T) {
	head := "x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=sdk-cli;"
	for _, c := range [][2]string{
		{" cch=abc cc_workload=cron;", " cc_workload=cron;"},     // no ";" yet: the next field stays
		{"\tcch=abc;\ncc_workload=cron;", "\ncc_workload=cron;"}, // its own separator goes, the next one stays
	} {
		b, _ := json.Marshal(head + c[0])
		p := mustObj(t, `{"system":[{"type":"text","text":`+string(b)+`}]}`)
		if !normalizeBody(p, modelRule{}) {
			t.Fatal("unchanged")
		}
		if got, _ := p.list("system")[0].(*obj).str("text"); got != head+c[1] {
			t.Fatalf("got %q want %q", got, head+c[1])
		}
	}
}

// test_portable_bodies_are_left_alone
func TestPortableBodiesAreLeftAlone(t *testing.T) {
	for _, body := range []string{
		`{"thinking":{"type":"adaptive","display":"omitted"}}`,
		`{"thinking":{"type":"adaptive","display":"summarized"}}`,
		`{"thinking":{"type":"disabled"}}`,
		`{"messages":[]}`,
		`{"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}}]}`,
		`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.302.a1b; cc_entrypoint=cli; cc_workload=cron; cc_is_subagent=true; cc_gateway=env;"}]}`,
	} {
		p := mustObj(t, body)
		if normalizeBody(p, modelRule{}) || enc(t, p) != body {
			t.Fatalf("a portable body changed: %s", body)
		}
	}
}

// The edge, unlike a launcher's agent proxy, never drops a server tool: a route
// that serves web search or the advisor keeps its tools.
func TestTheEdgeKeepsServerTools(t *testing.T) {
	body := `{"tools":[{"name":"Read","input_schema":{}},{"type":"web_search_20250305","name":"web_search"}]}`
	p := mustObj(t, body)
	if normalizeBody(p, modelRule{}) || enc(t, p) != body {
		t.Fatal("a server tool was dropped")
	}
}

// test_pinned_tier_covers_full_ids
func TestPinnedTierCoversFullIds(t *testing.T) {
	for _, c := range [][2]string{
		{"claude-fable-5-1", "gpt-6-astra"}, {"claude-fable-5", "gpt-6-astra"}, {"fable", "gpt-6-astra"},
		{"claude-opus-5-5", "claude-opus-5-5"}, {"gpt-6-astra", "gpt-6-astra"},
	} {
		p := mustObj(t, `{"model":"`+c[0]+`"}`)
		changed := normalizeBody(p, modelRule{tiers: map[string]string{"fable": "gpt-6-astra"}, isRoute: true})
		if got, _ := p.str("model"); got != c[1] || changed != (c[0] != c[1]) {
			t.Errorf("%s: sent %s changed=%v, want %s", c[0], got, changed, c[1])
		}
	}
}

// test_a_pinned_tier_reaches_the_backend_without_the_context_marker
func TestAPinnedTierLosesTheContextMarker(t *testing.T) {
	tiers := map[string]string{"opus": "claude-opus-5-5[1m]"}
	p := mustObj(t, `{"model":"claude-opus-5-5"}`)
	normalizeBody(p, modelRule{tiers: tiers, isRoute: true})
	if m, _ := p.str("model"); m != "claude-opus-5-5" {
		t.Fatal(m)
	}
	p = mustObj(t, `{"model":"claude-opus-5-5[1m]"}`)
	if !normalizeBody(p, modelRule{tiers: tiers, isRoute: true}) {
		t.Fatal("the marker stayed")
	}
	if m, _ := p.str("model"); m != "claude-opus-5-5" {
		t.Fatal(m)
	}
}

// test_a_route_pinned_tier_is_served_as_the_targets_own_model
func TestARoutePinnedTierIsServedAsTheTargetsOwnModel(t *testing.T) {
	route := map[string]string{"fable": "gpt-6-astra", "opus": "claude-opus-5-5"}
	gpro := map[string]string{"fable": "claude-opus-4-6-thinking", "opus": "claude-opus-4-6-thinking"}
	p := mustObj(t, `{"model":"gpt-6-astra"}`)
	if !normalizeBody(p, modelRule{tiers: gpro, routeTiers: route}) {
		t.Fatal("unchanged")
	}
	if m, _ := p.str("model"); m != "claude-opus-4-6-thinking" {
		t.Fatal(m)
	}
	// the account (family=false) changes nothing else the phone picked
	p = mustObj(t, `{"model":"gpt-6-astra"}`)
	if !(modelRule{tiers: map[string]string{"fable": "claude-fable-5-2"}, routeTiers: route}).setModel(p) {
		t.Fatal("unchanged")
	}
	if m, _ := p.str("model"); m != "claude-fable-5-2" {
		t.Fatal(m)
	}
	p = mustObj(t, `{"model":"claude-sonnet-5-5"}`)
	if (modelRule{tiers: map[string]string{"sonnet": "x"}, routeTiers: route}).setModel(p) {
		t.Fatal("the account changed a model the phone picked")
	}
	p = mustObj(t, `{"model":"gpt-6-astra"}`)
	if normalizeBody(p, modelRule{tiers: route, routeTiers: route, isRoute: true}) { // on the route itself nothing moves
		t.Fatal("the route moved its own model")
	}
}

// test_event_uploads_keep_only_what_the_phone_shows
func TestEventUploadsKeepOnlyWhatThePhoneShows(t *testing.T) {
	long := strings.Repeat("x", textMax*3)
	shot := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("A", 200_000) + `"}}`
	icon := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + strings.Repeat("B", 500) + `"}}`
	body := mustObj(t, `{"worker_epoch":1,"events":[
	 {"payload":{"type":"control_response","response":{"response":{"models":[{"value":"opus"}],"commands":[{"name":"a","argumentHint":"<x>","description":"long"}]}}}},
	 {"payload":{"type":"assistant","wire_tool_inputs":{"t":{"command":"ls"}},"message":{"content":[{"type":"thinking","thinking":"why","signature":"`+strings.Repeat("S", 900)+`"},{"type":"tool_use","id":"t","input":{"command":"ls"}}]}}},
	 {"payload":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[`+shot+`,`+icon+`,{"type":"text","text":"`+long+`"}]},{"type":"tool_result","tool_use_id":"u","content":"`+long+`"}]},
	   "tool_use_result":{"originalFile":"`+long+`","structuredPatch":[{"lines":["-a","+b"]}],"stdout":"`+long+`"}}}]}`)
	small := func(string, string) (string, string, bool) { return strings.Repeat("C", 9), "image/jpeg", true }
	if !slimEvents(body, small) {
		t.Fatal("nothing slimmed")
	}
	ev := body.list("events")
	payload := func(i int) *obj { return ev[i].(*obj).obj("payload") }
	cmds := payload(0).obj("response").obj("response")
	if enc(t, cmds.list("commands")) != `[{"name":"a","argumentHint":"<x>"}]` || enc(t, cmds.list("models")) != `[{"value":"opus"}]` {
		t.Fatal(enc(t, cmds))
	}
	said := payload(1)
	if _, has := said.get("wire_tool_inputs"); has {
		t.Fatal("wire_tool_inputs kept")
	}
	blocks := said.obj("message").list("content")
	if enc(t, blocks[0]) != `{"type":"thinking","thinking":"why","signature":""}` || enc(t, blocks[1].(*obj).obj("input")) != `{"command":"ls"}` {
		t.Fatal(enc(t, blocks))
	}
	result := payload(2)
	first := result.obj("message").list("content")[0].(*obj).list("content")
	if enc(t, first[0].(*obj).obj("source")) != `{"type":"base64","media_type":"image/jpeg","data":"CCCCCCCCC"}` {
		t.Fatal("screenshot not shrunk")
	}
	if enc(t, first[1]) != icon {
		t.Fatal("a small image changed")
	}
	if txt, _ := first[2].(*obj).str("text"); !strings.HasPrefix(txt, strings.Repeat("x", textMax)) || len(txt) > textMax+100 {
		t.Fatalf("long text kept: %d", len(txt))
	}
	if second, _ := result.obj("message").list("content")[1].(*obj).get("content"); len(second.(string)) > textMax+100 {
		t.Fatal("long result kept")
	}
	if enc(t, result.obj("tool_use_result")) != `{"structuredPatch":[{"lines":["-a","+b"]}]}` { // the diff stays
		t.Fatal(enc(t, result.obj("tool_use_result")))
	}
	none := func(string, string) (string, string, bool) { return "", "", false }
	if slimEvents(body, none) {
		t.Fatal("an already slim upload reported a change")
	}
	// A tool_result without `content` has always gained `content: null`; that key
	// insertion is a change the flag carries, like any rewrite of its value.
	bare := mustObj(t, `{"events":[{"payload":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"v"}]}}}]}`)
	if !slimEvents(bare, none) || !strings.Contains(enc(t, bare), `"tool_use_id":"v","content":null`) {
		t.Fatal(enc(t, bare))
	}
	if slimEvents(bare, none) {
		t.Fatal("a second pass changed it")
	}
}

func TestEventUploadPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/v1/code/sessions/cse_01Ab/worker/events":        true,
		"/v1/code/sessions/cse_01Ab/worker/events/stream": false,
		"/v1/code/sessions/cse_01Ab/worker/heartbeat":     false,
	} {
		if isEventUpload("api.anthropic.com", path) != want {
			t.Errorf("%s: want %v", path, want)
		}
	}
	if isEventUpload("evil.example", "/v1/code/sessions/a/worker/events") {
		t.Fatal("another host's upload")
	}
}

// Python's len and slice count characters, not bytes.
func TestLongTextIsCutByCharacters(t *testing.T) {
	s := strings.Repeat("é", textMax+cutLen+1)
	got, changed := cut(s)
	if !changed || got != strings.Repeat("é", textMax)+cutNote {
		t.Fatal("not cut at the character")
	}
	if _, changed := cut(strings.Repeat("é", textMax+cutLen)); changed {
		t.Fatal("text at the limit was cut")
	}
}
