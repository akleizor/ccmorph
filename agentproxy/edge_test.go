package agentproxy

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyOf is the proxy of edge `parent` (whose start is `started`) over a fresh bridge dir.
func proxyOf(t *testing.T, parent int, started string) (*Edge, string) {
	t.Helper()
	bridge := t.TempDir()
	u, _ := url.Parse("http://127.0.0.1:60111")
	return NewEdge(&Edge{Upstream: u, Key: "route-key", Tiers: map[string]string{"sonnet": "boot-route-sonnet"},
		Inference: filepath.Join(bridge, "inference"), Parent: parent, ParentStarted: started}), bridge
}

func handoff(upstream, key, id string, edge int, started string) []byte {
	m := map[string]any{"upstream": upstream, "key": key, "tiers": map[string]string{}, "id": id}
	if edge != 0 {
		m["edge"], m["edge_started"] = edge, started
	}
	b, _ := json.Marshal(m)
	return b
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func heldProfiles(t *testing.T, bridge string, pid int) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(bridge, "held."+itoa(pid)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var h struct{ Profiles map[string]any }
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	return h.Profiles
}

func (e *Edge) lookup(profile string) (endpoint, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep, ok := e.handoff[profile]
	return ep, ok
}

func none(t *testing.T, dir, pattern string) {
	t.Helper()
	if m, _ := filepath.Glob(filepath.Join(dir, pattern)); len(m) > 0 {
		t.Fatalf("%v left behind", m)
	}
}

// test_inference_switch_is_read_live_and_defaults_to_the_route
func TestTheInferenceSwitchIsReadLive(t *testing.T) {
	sw := filepath.Join(t.TempDir(), "inference")
	if inferenceTarget("") != "route" || inferenceTarget(sw) != "route" {
		t.Fatal("no file is the route")
	}
	for word, want := range map[string]string{"account\n": "account", "route\n": "route", "oli\n": "oli", "../etc/x\n": "route"} {
		write(t, sw, []byte(word))
		if got := inferenceTarget(sw); got != want {
			t.Errorf("%q: got %s want %s", word, got, want)
		}
	}
}

// test_a_session_word_is_read_only_from_a_plain_id, ..._found_by_the_header_claude_code_really_sends
func TestASessionWordIsFoundByTheHeaderClaudeCodeSends(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "cse_1"), []byte("gw\n"))
	write(t, filepath.Join(dir, "bad"), []byte("not a word!\n"))
	for sid, want := range map[string]string{"cse_1": "gw", "bad": "", "missing": "", "../" + filepath.Base(dir) + "/cse_1": ""} {
		if got := sessionTarget(dir, sid); got != want {
			t.Errorf("%q: got %q want %q", sid, got, want)
		}
	}
	if sessionTarget("", "cse_1") != "" {
		t.Fatal("no dir, a word")
	}
	// a real pair: the Start UI host's --sdk-url id and the transcript id its requests carried
	if got := headerID("cse_01UYCe7WrYRm399JWcJrn7Rd"); got != "c3ca46e6-52e5-57cc-a1c3-6a9e88650085" {
		t.Fatal(got)
	}
	write(t, filepath.Join(dir, "cse_01UYCe7WrYRm399JWcJrn7Rd"), []byte("account\n"))
	write(t, filepath.Join(dir, "cse_other"), []byte("gw\n"))
	for sid, want := range map[string]string{"c3ca46e6-52e5-57cc-a1c3-6a9e88650085": "account", headerID("cse_other"): "gw", headerID("cse_absent"): ""} {
		if got := sessionTarget(dir, sid); got != want {
			t.Errorf("%s: got %q want %q", sid, got, want)
		}
	}
}

// test_inference_is_rerouted / test_everything_else_passes_through
func TestOnlyFirstPartyMessagesAreInference(t *testing.T) {
	for _, c := range []struct {
		host, path string
		want       bool
	}{
		{"api.anthropic.com", "/v1/messages", true},
		{"API.anthropic.com.", "/v1/messages/count_tokens", true},
		{"api.anthropic.com", "/v1/code/sessions/abc/worker/heartbeat", false},
		{"api.anthropic.com", "/v1/environments/bridge", false},
		{"api.anthropic.com", "/api/oauth/validate", false},
		{"api.anthropic.com", "/v1/messagesX", false},
		{"evil.example", "/v1/messages", false},
		{"api.anthropic.com.evil.example", "/v1/messages", false},
	} {
		if got := isFirstPartyHost(c.host) && isInference(c.path); got != c.want {
			t.Errorf("%s %s: got %v", c.host, c.path, got)
		}
	}
}

// test_the_addon_reads_a_handoff_once_and_keeps_it_thereafter
func TestAHandoffIsReadOnceAndKeptThereafter(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	write(t, filepath.Join(bridge, "switch-gw.json"), handoff("http://127.0.0.1:54505", "route-key", "h1", 500, "start-500"))
	ep, ok := e.endpoint("gw")
	if !ok || ep.upstream != "http://127.0.0.1:54505" || ep.key != "route-key" {
		t.Fatalf("%+v %v", ep, ok)
	}
	none(t, bridge, "switch-*") // the key lived one hop, never on disk
	none(t, bridge, "taken.*")
	if again, _ := e.endpoint("gw"); again.upstream != ep.upstream {
		t.Fatal("a later turn lost it") // brought up once; every turn follows
	}
}

// test_the_proxy_takes_a_handoff_without_waiting_for_a_request
func TestTheProxyTakesAHandoffWithoutWaitingForARequest(t *testing.T) {
	e, bridge := proxyOf(t, 0, "")
	write(t, filepath.Join(bridge, "switch-gw.json"), handoff("http://127.0.0.1:54505", "k2", "", 0, ""))
	done := make(chan struct{})
	defer close(done)
	e.Run(done, func() {})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ep, ok := e.lookup("gw"); ok {
			if ep.key != "k2" {
				t.Fatal(ep)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the poller never took it")
		}
		time.Sleep(20 * time.Millisecond)
	}
	none(t, bridge, "switch-*")
}

// test_a_taken_handoff_is_recorded_before_it_can_be_seen_gone (issue #900), as it
// reads here: the id a handoff is acknowledged by (held.<pid>.json) never shows
// before its endpoint is held, and the handoff is gone only once it is.
func TestAHandoffIsRecordedBeforeItIsAcknowledged(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	write(t, filepath.Join(bridge, "switch-gw.json"), handoff("http://127.0.0.1:54505", "k2", "h1", 500, "start-500"))
	var wg sync.WaitGroup
	wg.Add(1)
	seen := make(chan string, 1)
	go func() { // --switch's wait: the file is gone and held names the id
		defer wg.Done()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
			b, err := os.ReadFile(filepath.Join(bridge, "held.500.json"))
			if err == nil && strings.Contains(string(b), `"h1"`) {
				if _, ok := e.lookup("gw"); !ok {
					seen <- "acknowledged before it was held"
				} else {
					seen <- ""
				}
				return
			}
		}
		seen <- "never acknowledged"
	}()
	e.Take("gw")
	wg.Wait()
	if msg := <-seen; msg != "" {
		t.Fatal(msg)
	}
	if got := heldProfiles(t, bridge, 500); got["gw"] != "h1" {
		t.Fatal(got)
	}
}

// test_a_handoff_holding_no_endpoint_is_removed_without_recording_anything
func TestAHandoffHoldingNoEndpointIsRemovedWithoutRecordingAnything(t *testing.T) {
	for name, body := range map[string][]byte{
		"holds no key":      []byte(`{"upstream": "http://127.0.0.1:54505"}`),
		"holds no upstream": []byte(`{"key": "k2"}`),
		"does not parse":    []byte("not json"),
		"invalid utf-8":     {0xff},
	} {
		e, bridge := proxyOf(t, 0, "")
		write(t, filepath.Join(bridge, "switch-gw.json"), body)
		if _, ok := e.endpoint("gw"); ok {
			t.Errorf("%s: an endpoint from nothing", name)
		}
		none(t, bridge, "switch-*")
		none(t, bridge, "taken.*")
		if len(e.handoff) != 0 || len(e.held) != 0 {
			t.Errorf("%s: recorded %v %v", name, e.handoff, e.held)
		}
	}
}

// test_a_failed_handoff_rename_leaves_the_endpoint_untouched
func TestAFailedHandoffRenameLeavesTheEndpointUntouched(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	hand := filepath.Join(bridge, "switch-gw.json")
	write(t, hand, handoff("http://127.0.0.1:54505", "k1", "h1", 500, "start-500"))
	e.Take("gw") // the baseline
	// the rename cannot land: its destination is a directory that holds something
	taken := filepath.Join(bridge, "taken."+itoa(os.Getpid())+"."+e.ownTag+".gw.json")
	os.MkdirAll(filepath.Join(taken, "x"), 0o700)
	write(t, hand, handoff("http://127.0.0.1:54599", "k2", "h2", 500, "start-500"))
	e.Take("gw")
	if ep, _ := e.lookup("gw"); ep.upstream != "http://127.0.0.1:54505" {
		t.Fatalf("the new one was recorded: %+v", ep)
	}
	if e.held["gw"] != "h1" {
		t.Fatal(e.held)
	}
	if _, err := os.Stat(hand); err != nil {
		t.Fatal("the handoff no longer stands for a retry")
	}
	// A profile with no prior entry: the failed take records nothing.
	os.MkdirAll(filepath.Join(bridge, "taken."+itoa(os.Getpid())+"."+e.ownTag+".no.json", "x"), 0o700)
	write(t, filepath.Join(bridge, "switch-no.json"), handoff("http://127.0.0.1:54598", "k9", "h9", 500, "start-500"))
	e.Take("no")
	if _, ok := e.lookup("no"); ok {
		t.Fatal("recorded")
	}
	if _, ok := e.held["no"]; ok {
		t.Fatal("held")
	}
}

// test_a_take_that_puts_another_edge_handoff_back_switches_nothing
func TestATakeThatPutsAnotherEdgesHandoffBackSwitchesNothing(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	hand := filepath.Join(bridge, "switch-oli.json")
	write(t, hand, handoff("u1", "k", "h1", 500, "start-500"))
	e.Take("oli")
	e.testHook = func(stage string) {
		if stage == "peeked" { // another edge's publication lands before the move
			write(t, hand, handoff("other", "k", "h600", 600, "start-600"))
		}
	}
	write(t, hand, handoff("u2", "k", "h2", 500, "start-500"))
	e.Take("oli")
	if ep, _ := e.lookup("oli"); ep.upstream != "u1" || e.held["oli"] != "h1" {
		t.Fatalf("switched: %+v %v", ep, e.held)
	}
	b, _ := os.ReadFile(hand)
	if !strings.Contains(string(b), `"h600"`) { // theirs stands, taken by its proxy
		t.Fatalf("their handoff was not put back: %s", b)
	}
	none(t, bridge, "taken.*")
	if heldProfiles(t, bridge, 500)["oli"] != "h1" {
		t.Fatal("held changed")
	}
}

// test_a_same_edge_replacement_the_take_moves_is_recorded_before_it_is_acknowledged:
// the publication the move takes decides, however the one the take looked at read.
func TestASameEdgeReplacementTheTakeMovesIsTheOneRecorded(t *testing.T) {
	for _, first := range []string{"valid", "garbage", "rewritten in place"} {
		e, bridge := proxyOf(t, 500, "start-500")
		hand := filepath.Join(bridge, "switch-oli.json")
		switch first {
		case "garbage":
			write(t, hand, []byte("not json"))
		default:
			write(t, hand, handoff("old", "k", "h1", 500, "start-500"))
		}
		e.testHook = func(stage string) {
			if stage == "peeked" { // the next publication, landing before the move
				tmp := filepath.Join(bridge, "newer")
				write(t, tmp, handoff("new", "k", "h2", 500, "start-500"))
				os.Rename(tmp, hand)
				if first == "rewritten in place" {
					write(t, hand, handoff("newest", "k", "h3", 500, "start-500"))
				}
			}
		}
		e.Take("oli")
		want, id := "new", "h2"
		if first == "rewritten in place" {
			want, id = "newest", "h3"
		}
		if ep, ok := e.lookup("oli"); !ok || ep.upstream != want || e.held["oli"] != id {
			t.Fatalf("%s: %+v %v", first, ep, e.held)
		}
		none(t, bridge, "switch-*")
		none(t, bridge, "taken.*")
		if heldProfiles(t, bridge, 500)["oli"] != id {
			t.Fatalf("%s: held says %v", first, heldProfiles(t, bridge, 500))
		}
	}
}

// test_a_take_that_moved_nothing_of_its_own_leaves_the_endpoint_untouched
func TestATakeThatMovedNothingOfItsOwnLeavesTheEndpointUntouched(t *testing.T) {
	for _, meanwhile := range []string{"withdrawn", "garbage", "invalid utf-8"} {
		e, bridge := proxyOf(t, 500, "start-500")
		hand := filepath.Join(bridge, "switch-oli.json")
		write(t, hand, handoff("old", "k", "h1", 500, "start-500"))
		e.Take("oli")
		prior, _ := os.ReadFile(filepath.Join(bridge, "held.500.json"))
		e.testHook = func(stage string) {
			if stage != "peeked" {
				return
			}
			switch meanwhile {
			case "withdrawn":
				os.Remove(hand)
			case "garbage":
				write(t, hand, []byte("not json"))
			default:
				write(t, hand, []byte{0xff})
			}
		}
		write(t, hand, handoff("new", "k", "h2", 500, "start-500"))
		e.Take("oli")
		if ep, _ := e.lookup("oli"); ep.upstream != "old" || e.held["oli"] != "h1" {
			t.Fatalf("%s: %+v %v", meanwhile, ep, e.held)
		}
		none(t, bridge, "switch-*")
		none(t, bridge, "taken.*")
		if now, _ := os.ReadFile(filepath.Join(bridge, "held.500.json")); string(now) != string(prior) {
			t.Fatalf("%s: held changed: %s", meanwhile, now)
		}
	}
}

// test_a_proxy_takes_only_its_own_edges_handoff_and_says_what_it_holds
func TestAProxyTakesOnlyItsOwnEdgesHandoffAndSaysWhatItHolds(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	if err := e.PublishHeld(); err != nil { // what Run writes at start
		t.Fatal(err)
	}
	held := filepath.Join(bridge, "held.500.json")
	if b, _ := os.ReadFile(held); string(b) != `{"edge":500,"edge_started":"start-500","profiles":{}}` {
		t.Fatal(string(b))
	}
	hand := filepath.Join(bridge, "switch-oli.json")
	for _, other := range [][2]any{{600, "start-600"}, {500, "other-500"}} {
		write(t, hand, handoff("u", "k", "h0", other[0].(int), other[1].(string)))
		if _, ok := e.endpoint("oli"); ok {
			t.Fatalf("took %v's handoff", other)
		}
		if _, err := os.Stat(hand); err != nil {
			t.Fatalf("%v's handoff was touched", other)
		}
	}
	write(t, hand, handoff("u", "k", "h1", 500, "start-500"))
	if _, ok := e.endpoint("oli"); !ok {
		t.Fatal("did not take its own")
	}
	none(t, bridge, "switch-*")
	if heldProfiles(t, bridge, 500)["oli"] != "h1" {
		t.Fatal("held does not say it")
	}
	if b, _ := os.ReadFile(held); strings.Contains(strings.ReplaceAll(string(b), `"oli"`, ""), "k") && strings.Contains(string(b), `"key"`) {
		t.Fatalf("a key in what it says: %s", b)
	}
	e.ForgetHeld()
	if _, err := os.Stat(held); err == nil {
		t.Fatal("what it held outlives it")
	}
}

// test_a_delayed_proxy_never_removes_a_handoff_published_after_its_take
func TestADelayedProxyNeverRemovesAHandoffPublishedAfterItsTake(t *testing.T) {
	e, bridge := proxyOf(t, 500, "old")
	hand := filepath.Join(bridge, "switch-oli.json")
	write(t, hand, handoff("u", "k", "old", 500, "old"))
	e.testHook = func(stage string) {
		if stage == "moved" { // the replacement edge's switch publishes now
			write(t, hand, handoff("u", "k", "new", 500, "new"))
		}
	}
	if _, ok := e.endpoint("oli"); !ok {
		t.Fatal("not taken")
	}
	b, _ := os.ReadFile(hand)
	if !strings.Contains(string(b), `"new"`) {
		t.Fatalf("the newer handoff was removed: %s", b)
	}
	none(t, bridge, "taken.*") // what it took holds no key on disk
}

// test_a_held_record_that_cannot_be_written_never_cuts_the_take_short
func TestAHeldRecordThatCannotBeWrittenNeverCutsTheTakeShort(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	os.MkdirAll(filepath.Join(bridge, "held.500.json", "x"), 0o700) // the record's name is taken
	hand := filepath.Join(bridge, "switch-oli.json")
	write(t, hand, handoff("u", "k", "h1", 500, "start-500"))
	if _, ok := e.endpoint("oli"); !ok || e.held["oli"] != "h1" {
		t.Fatal("the endpoint taken is not held")
	}
	none(t, bridge, "switch-*")
	none(t, bridge, "taken.*")
}

// test_a_proxy_shutdown_never_removes_the_held_record_a_replacement_wrote
func TestAProxyShutdownNeverRemovesTheHeldRecordAReplacementWrote(t *testing.T) {
	e, bridge := proxyOf(t, 500, "old")
	held := filepath.Join(bridge, "held.500.json")
	old := `{"edge":500,"edge_started":"old","profiles":{"oli":"h-old"}}`
	repl := `{"edge":500,"edge_started":"new","profiles":{"oli":"h-new"}}`
	write(t, held, []byte(old))
	e.testHook = func(stage string) {
		if stage == "forget-checked" { // the replacement's proxy writes now
			write(t, filepath.Join(bridge, "new-held"), []byte(repl))
			os.Rename(filepath.Join(bridge, "new-held"), held)
		}
	}
	e.ForgetHeld()
	if b, _ := os.ReadFile(held); string(b) != repl {
		t.Fatalf("the replacement's record is gone: %s", b)
	}
	none(t, bridge, "held.500.json.*") // nothing left aside
	e.testHook = nil
	e.ForgetHeld() // never ours from the start: not touched
	if b, _ := os.ReadFile(held); string(b) != repl {
		t.Fatalf("a record that was never ours was removed: %s", b)
	}
}

// live is a shared edge's record the proxy's note counts: its process runs, its
// proxy answers on loopback and its CA is there.
func live(t *testing.T, edges, name string, extra map[string]any) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	write(t, ca, []byte("ca"))
	rec := map[string]any{"pid": os.Getpid(), "port": ln.Addr().(*net.TCPAddr).Port, "ca": ca}
	for k, v := range extra {
		rec[k] = v
	}
	b, _ := json.Marshal(rec)
	os.MkdirAll(edges, 0o700)
	write(t, filepath.Join(edges, name+".json"), b)
}

// test_the_note_names_switch_only_where_switch_lands
func TestTheNoteNamesSwitchOnlyWhereSwitchLands(t *testing.T) {
	me, ok := procStarted(os.Getpid())
	if !ok {
		t.Skip("no process start on this system")
	}
	for _, c := range []struct {
		name         string
		edges        int
		parent       int
		started      string
		names        bool
		dropCA       bool
		wantContains string
	}{
		{"this proxy's edge names the switch that lands", 1, os.Getpid(), me, true, false, "`ccmorph-rc oli --switch` hands it over"},
		{"another edge's proxy names no switch", 1, 1, me, false, false, "is not the shared edge's"},
		{"a CA gone names no switch that refuses", 1, os.Getpid(), me, false, true, "(0 live)"},
		{"two live edges name no switch that refuses", 2, os.Getpid(), me, false, false, "(2 live)"},
		{"a proxy of an earlier process at the edge's pid", 1, os.Getpid(), "an-earlier-start", false, false, "follows an earlier process"},
		{"a proxy that knows no edge start", 1, os.Getpid(), "", false, false, "predates handoffs"},
	} {
		bridge := t.TempDir()
		for i := 0; i < c.edges; i++ {
			live(t, filepath.Join(bridge, "edges"), "e"+itoa(i), nil)
		}
		if c.dropCA {
			recs, _ := filepath.Glob(filepath.Join(bridge, "edges", "*.json"))
			for _, r := range recs {
				var rec map[string]any
				b, _ := os.ReadFile(r)
				json.Unmarshal(b, &rec)
				os.Remove(rec["ca"].(string))
			}
		}
		note := lostEndpointNote("oli", bridge, c.parent, "", c.started)
		if strings.Contains(note, "ccmorph-rc oli --switch") != c.names {
			t.Errorf("%s: %s", c.name, note)
		}
		if !strings.Contains(note, c.wantContains) {
			t.Errorf("%s: %q lacks %q", c.name, note, c.wantContains)
		}
	}
}

// test_a_sessions_note_names_the_session_switch
func TestASessionsNoteNamesTheSessionSwitch(t *testing.T) {
	me, ok := procStarted(os.Getpid())
	if !ok {
		t.Skip("no process start on this system")
	}
	bridge := t.TempDir()
	live(t, filepath.Join(bridge, "edges"), "gw", nil)
	note := lostEndpointNote("oli", bridge, os.Getpid(), "cse_01ABC", me)
	if !strings.Contains(note, "`ccmorph-rc oli --switch --session cse_01ABC` hands it over") {
		t.Fatal(note)
	}
}

// test_the_note_and_switch_both_skip_an_edge_record_naming_another_route (#846)
func TestTheNoteSkipsAnEdgeRecordNamingAnotherRoute(t *testing.T) {
	bridge := t.TempDir()
	live(t, filepath.Join(bridge, "edges"), "GW", map[string]any{"route": "gw"})
	if routeEdge(filepath.Join(bridge, "edges"), "GW") != nil {
		t.Fatal("a case twin's record counted")
	}
	if n, e := switchEdge(filepath.Join(bridge, "edges")); n != 0 || e != nil {
		t.Fatalf("%d %v", n, e)
	}
	note := lostEndpointNote("oli", bridge, os.Getpid(), "", "")
	if strings.Contains(note, "--switch") || !strings.Contains(note, "(0 live)") {
		t.Fatal(note)
	}
	// a record naming no route counts only under its own spelling
	live(t, filepath.Join(bridge, "edges"), "gx", nil)
	if routeEdge(filepath.Join(bridge, "edges"), "gx") == nil {
		t.Fatal("a record under its own name did not count")
	}
}

// test_an_edge_state_name_keeps_a_profile_name_and_cleans_every_other_route
func TestEdgeNameKeepsAProfileNameAndCleansEveryOtherRoute(t *testing.T) {
	if edgeName("gw_eu") != "gw_eu" {
		t.Fatal("a profile name was changed")
	}
	seen := map[string]string{}
	for _, route := range []string{"gw/eu", "gw-eu", "gw.eu", "../gw", "", strings.Repeat("a", 300) + "/"} {
		n := edgeName(route)
		if strings.ContainsAny(n, "/\\") || len(n) > 100 {
			t.Errorf("%q -> %q", route, n)
		}
		if other, dup := seen[n]; dup {
			t.Errorf("%q and %q share %q", route, other, n)
		}
		seen[n] = route
	}
}

// test_every_session_turn_answers_which_upstream_served_it (GH-920), the verdict
// each turn's reply line carries.
func TestEveryTurnAnswersWhichUpstreamServedIt(t *testing.T) {
	addr := func(ip string, port int) net.Addr { return &net.TCPAddr{IP: net.ParseIP(ip), Port: port} }
	gw := &edgeState{asked: "gw", via: "gw"}
	for _, c := range []struct {
		name       string
		es         *edgeState
		firstParty bool
		host       string
		port       int
		answered   net.Addr
		want       string
	}{
		{"its pick's endpoint answered", gw, false, "127.0.0.1", 54505, addr("127.0.0.1", 54505), "route     <- 200 /v1/messages asked=gw served-by=127.0.0.1:54505"},
		{"subscription answered a routed turn", gw, true, "127.0.0.1", 54505, addr("127.0.0.1", 54505), "miss      <- 200 /v1/messages asked=gw served-by=ANTHROPIC (not routed!)"},
		{"another endpoint replied", gw, false, "127.0.0.1", 54505, addr("127.0.0.1", 60111), "miss      <- 200 /v1/messages asked=gw served-by=127.0.0.1:60111"},
		{"the pick fell back to the route", &edgeState{asked: "ds", via: "route"}, false, "127.0.0.1", 60111, addr("127.0.0.1", 60111), "miss      <- 200 /v1/messages asked=ds via=route served-by=127.0.0.1:60111"},
		{"account answered with its signature", &edgeState{asked: "account", via: "account", account: true}, true, "api.anthropic.com", 443, addr("160.79.104.10", 443), "account   <- 200 /v1/messages asked=account served-by=ANTHROPIC"},
		{"account answered without it", &edgeState{asked: "account", via: "account", account: true}, false, "api.anthropic.com", 443, addr("160.79.104.10", 443), "miss      <- 200 /v1/messages asked=account served-by=api.anthropic.com:443 (no Anthropic signature)"},
		{"the shared route, no false miss", &edgeState{asked: "route", via: "route"}, false, "127.0.0.1", 60111, addr("127.0.0.1", 60111), "route     <- 200 /v1/messages asked=route served-by=127.0.0.1:60111"},
		{"another IP on the pick's port", gw, false, "127.0.0.1", 54505, addr("127.0.0.2", 54505), "miss      <- 200 /v1/messages asked=gw served-by=127.0.0.2:54505"},
		// a named upstream is served by the name it left for; the IP it resolved to is no mismatch
		{"a named upstream by its address", &edgeState{asked: "named", via: "named"}, false, "gw.example", 443, addr("192.0.2.1", 443), "route     <- 200 /v1/messages asked=named served-by=gw.example:443"},
		{"a named upstream on another port", &edgeState{asked: "named", via: "named"}, false, "gw.example", 8443, addr("192.0.2.1", 443), "miss      <- 200 /v1/messages asked=named served-by=gw.example:8443"},
	} {
		if got := verdict(c.es, 200, "/v1/messages", c.firstParty, c.host, c.port, c.answered); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
