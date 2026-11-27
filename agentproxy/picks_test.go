package agentproxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func firstRequest(prompt string) []byte {
	return []byte(`{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>context</system-reminder>"},{"type":"text","text":` + quote(prompt) + `}]}]}`)
}

// test_the_pending_key_is_the_routers: router/edge.go promptKey pins the same vector.
func TestThePendingKeyIsTheRouters(t *testing.T) {
	if got := PromptKey("  say hi\n"); got != "53eae7058490affdf5ca03d9cc9f08b34b7f408f21f6f8a5e397068709243ede" {
		t.Fatal(got)
	}
}

// test_a_pick_binds_the_one_agent_whose_prompt_it_names
func TestAPickBindsTheOneAgentWhoseSessionAndPromptItNames(t *testing.T) {
	agents := t.TempDir()
	if err := LeavePick(agents, "s1", "Fix parse()", Pick{Word: "kathy", Model: "claude-sonnet-5-5"}); err != nil {
		t.Fatal(err)
	}
	if !HasPending(agents, "s1") {
		t.Fatal("no pick waits")
	}
	if p, ok := Claim(agents, "s1", "a1", firstRequest("other work"), time.Now()); ok {
		t.Fatalf("an unrelated prompt bound %+v", p)
	}
	if p, ok := Claim(agents, "s1", "a1", firstRequest("Fix parse()\n"), time.Now()); !ok || p != (Pick{"kathy", "claude-sonnet-5-5"}) {
		t.Fatalf("%+v %v", p, ok)
	}
	if HasPending(agents, "s1") {
		t.Fatal("a claimed pick still waits")
	}
	if p, ok := Bound(agents, "a1"); !ok || p.Word != "kathy" {
		t.Fatalf("%+v %v", p, ok)
	}
	// Same-session fan-out, identical prompt: the pick went to the first agent;
	// the second finds none and falls through to the session's own route (#959).
	if p, ok := Claim(agents, "s1", "a2", firstRequest("Fix parse()"), time.Now()); ok {
		t.Fatalf("a second agent bound %+v", p)
	}
	for _, bad := range []string{"../a1", "a/1", "", "-a"} {
		if _, ok := Bound(agents, bad); ok {
			t.Errorf("%q named a binding", bad)
		}
	}
}

// test_a_foreign_stale_pick_never_binds_a_later_agent (#959 probe arm 2)
func TestAForeignClaimTakesNothing(t *testing.T) {
	agents := t.TempDir()
	LeavePick(agents, "s1", "Review the diff before merge", Pick{Word: "gw9", Model: "m9"})
	if !HasPending(agents, "s1") || HasPending(agents, "s2") {
		t.Fatal("the pick waits for the wrong session")
	}
	if _, ok := Claim(agents, "s2", "agentB", firstRequest("Review the diff before merge"), time.Now()); ok {
		t.Fatal("a foreign session's agent bound the pick")
	}
	if !HasPending(agents, "s1") { // the foreign claim took nothing
		t.Fatal("the pick was taken")
	}
	if p, ok := Claim(agents, "s1", "agentA", firstRequest("Review the diff before merge"), time.Now()); !ok || p.Word != "gw9" {
		t.Fatalf("%+v %v", p, ok)
	}
}

// test_a_claim_without_a_named_session_binds_nothing
func TestAClaimWithoutANamedSessionBindsNothing(t *testing.T) {
	for _, session := range []string{"", "../s1", "a/b", " s1"} {
		agents := t.TempDir()
		LeavePick(agents, "s1", "Fix parse()", Pick{Word: "kathy", Model: "m"})
		if HasPending(agents, session) {
			t.Errorf("%q: a request that names no session was parked", session)
		}
		if _, ok := Claim(agents, session, "a1", firstRequest("Fix parse()"), time.Now()); ok {
			t.Errorf("%q: bound a pick", session)
		}
		if !HasPending(agents, "s1") {
			t.Errorf("%q: the pick was taken", session)
		}
	}
}

// test_a_pick_nobody_claims_is_swept
func TestAPickNobodyClaimsIsSwept(t *testing.T) {
	agents := t.TempDir()
	LeavePick(agents, "s1", "late", Pick{Word: "kathy", Model: "m"})
	old := time.Now().Add(-pendingTTL - 5*time.Second)
	files, _ := filepath.Glob(filepath.Join(agents, "pending", "s1", "*.json"))
	for _, f := range files {
		os.Chtimes(f, old, old)
	}
	// Same session, expired: only the sweep keeps this matching pick from binding.
	if _, ok := Claim(agents, "s1", "a1", firstRequest("late"), time.Now()); ok {
		t.Fatal("an expired pick bound")
	}
	if HasPending(agents, "s1") {
		t.Fatal("the expired pick was not swept")
	}
}
