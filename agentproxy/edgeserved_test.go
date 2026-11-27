package agentproxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type servedRecord struct {
	Edge        int            `json:"edge"`
	EdgeStarted string         `json:"edge_started"`
	Turns       int            `json:"turns"`
	Misses      int            `json:"misses"`
	LastMiss    map[string]any `json:"last_miss"`
}

func readServed(t *testing.T, e *Edge) servedRecord {
	t.Helper()
	b, err := os.ReadFile(e.servedPath())
	if err != nil {
		t.Fatal(err)
	}
	var r servedRecord
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func newServedEdge(t *testing.T, started string) *Edge {
	t.Helper()
	return NewEdge(&Edge{Inference: filepath.Join(t.TempDir(), "inference"), Parent: 4321, ParentStarted: started})
}

// test_the_outcome_is_kept_without_a_debug_log_for_the_probe (GH-977): #920's
// verdict reached only the debug log, so a turn served by the wrong endpoint was
// audited and forgotten. The edge keeps the asked-vs-served outcome in the
// bridge dir whatever the log says: Log is nil here, as on a normal start.
func TestTheOutcomeIsKeptWithoutADebugLog(t *testing.T) {
	r := newEdgeRig(t, func(e *Edge, _ *edgeRig) { e.Parent, e.ParentStarted, e.Log = 4321, "s1", nil })
	os.MkdirAll(filepath.Join(r.bridge, "sessions"), 0o700)
	write(t, filepath.Join(r.bridge, "sessions", "cse_pick"), []byte("gw\n"))
	h, _ := json.Marshal(map[string]any{"upstream": r.otherU, "key": "gw-key", "tiers": map[string]string{}})
	write(t, filepath.Join(r.bridge, "switch-gw.json"), h)
	r.e.Take("gw")
	r.post(t, msgs, headerID("cse_pick"), "", "sonnet", "x") // the pick's endpoint answers: served
	r.expect(t, r.other, "the pick's endpoint")
	r.post(t, msgs, headerID("cse_none"), "", "sonnet", "x") // another session on the route: asked=route, served
	r.expect(t, r.route, "the route")
	got := readServed(t, r.e)
	if got.Turns != 2 || got.Misses != 0 || got.Edge != 4321 || got.EdgeStarted != "s1" {
		t.Fatalf("served turns are counted, none missed: %+v", got)
	}
	write(t, filepath.Join(r.bridge, "sessions", "cse_lost"), []byte("ds\n")) // a pick the proxy holds no endpoint for
	r.post(t, msgs, headerID("cse_lost"), "", "sonnet", "x")
	r.expect(t, r.route, "the fallback route")
	got = readServed(t, r.e)
	if got.Turns != 3 || got.Misses != 1 {
		t.Fatalf("the fallback is one miss: %+v", got)
	}
	if line, _ := got.LastMiss["line"].(string); line == "" || got.LastMiss["at"] == nil {
		t.Fatalf("the last miss keeps its line and its time: %+v", got.LastMiss)
	}
	r.post(t, "/v1/environments/bridge", headerID("cse_pick"), "", "sonnet", "x") // not inference: never counts
	if got = readServed(t, r.e); got.Turns != 3 {
		t.Fatalf("a non-inference turn was counted: %+v", got)
	}
	none(t, r.bridge, "served.*.tmp")
}

// A record left by an earlier run of the edge is no history of this one; the
// same proxy continuing its own run keeps counting; one that refuses to write
// never breaks the turn and the next write carries what it missed.
func TestTheOutcomeBeginsAfreshForANewEdgeRunAndSurvivesARefusedWrite(t *testing.T) {
	e := newServedEdge(t, "s1")
	e.CountServed(true, "<- 200 /v1/messages asked=gw served-by=ANTHROPIC (not routed!)")
	e.CountServed(false, "x")
	if got := readServed(t, e); got.Turns != 2 || got.Misses != 1 {
		t.Fatalf("%+v", got)
	}
	again := NewEdge(&Edge{Inference: e.Inference, Parent: 4321, ParentStarted: "s1"}) // the same run, a proxy that restarted
	again.CountServed(false, "x")
	if got := readServed(t, again); got.Turns != 3 || got.Misses != 1 {
		t.Fatalf("the same edge run continues its count: %+v", got)
	}
	next := NewEdge(&Edge{Inference: e.Inference, Parent: 4321, ParentStarted: "s2"}) // another edge, the pid reused
	next.CountServed(false, "x")
	if got := readServed(t, next); got.Turns != 1 || got.Misses != 0 || got.EdgeStarted != "s2" {
		t.Fatalf("another run's record seeded this one: %+v", got)
	}
	dir := filepath.Dir(next.Inference)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	next.CountServed(true, "refused") // the dir takes no new file: the turn goes on
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := readServed(t, next); got.Turns != 1 {
		t.Fatalf("the refused write changed the record: %+v", got)
	}
	next.CountServed(false, "x")
	if got := readServed(t, next); got.Turns != 3 || got.Misses != 1 {
		t.Fatalf("the next write carries the lost verdict: %+v", got)
	}
	none(t, dir, "served.*.tmp")
}

// The record goes with its proxy, and only its own does: a record another edge
// run wrote at the same pid is left standing (the held record's rule).
func TestTheOutcomeGoesWithItsProxyAndOnlyItsOwn(t *testing.T) {
	e := newServedEdge(t, "s1")
	e.CountServed(false, "x")
	e.Stop()
	e.ForgetHeld()
	none(t, filepath.Dir(e.Inference), "served.*")
	e.CountServed(false, "x") // a stopped proxy writes nothing
	none(t, filepath.Dir(e.Inference), "served.*")
	stranger := newServedEdge(t, "s2")
	stranger.CountServed(false, "x")
	mine := NewEdge(&Edge{Inference: stranger.Inference, Parent: 4321, ParentStarted: "s1"})
	mine.ForgetHeld()
	if got := readServed(t, stranger); got.EdgeStarted != "s2" || got.Turns != 1 {
		t.Fatalf("another run's record was removed: %+v", got)
	}
}
