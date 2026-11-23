package router

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/util/tier"
)

// dispatchEnv is an isolated router with two profiles of two families: "sub"
// is a Claude subscription reading ahead of its pace, "gw" a gateway that
// costs nothing, so an unpinned dispatch picks family gpt. A fake `ccmorph`
// on PATH answers every worker run at once, so no test launches a session.
func dispatchEnv(t *testing.T) {
	t.Helper()
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv(WorkerEnv, "")
	for _, a := range []string{"OPUS", "SONNET", "HAIKU"} {
		t.Setenv(tier.TierEnvKey(a), "")
	}
	for name, conf := range map[string]string{
		"sub": "type=sub\n",
		"gw":  "type=apikey\nbase_url=http://127.0.0.1:9\nmodel=gpt-6-terra\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeJSON(filepath.Join(root, "router.json"),
		Config{Profiles: map[string]ProfileConfig{"gw": {Class: "free"}}}); err != nil {
		t.Fatal(err)
	}
	quotaSnapshot(t, map[string]float64{"sub": 0.9})
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "ccmorph"), []byte("#!/bin/sh\necho delegated-answer\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func lastLedgerRow(t *testing.T) Row {
	t.Helper()
	rows := readRows(ledgerPath())
	if len(rows) == 0 {
		t.Fatal("the ledger holds no row")
	}
	return rows[len(rows)-1]
}

// The three dispatch flags the parser already honored: the pinned family is
// the ledgered family, a family none serves fails cleanly, and --refresh
// forces one live quota read where a fresh snapshot answers on its own.
func TestDispatchHonorsFamilyAndRefresh(t *testing.T) {
	dispatchEnv(t)
	calls := 0
	QuotaInput = func(names []string) string { calls++; return "" }
	t.Cleanup(func() { QuotaInput = nil })

	dispatch := func(args ...string) int {
		t.Helper()
		var out, errb bytes.Buffer
		code := Main(append([]string{"dispatch", "--kind", "code", "--tier", "mid"}, args...), strings.NewReader(""), &out, &errb)
		if code == 0 && strings.HasPrefix(out.String(), "Error: ") {
			t.Fatalf("answer %q while exit said ok", out.String())
		}
		return code
	}

	// The default route takes the free gateway, family gpt; a fresh snapshot
	// is served from cache.
	if code := dispatch("fix the parser"); code != 0 {
		t.Fatalf("plain dispatch: exit %d", code)
	}
	if row := lastLedgerRow(t); row["family"] != "gpt" || row["profile"] != "gw" {
		t.Fatalf("unpinned dispatch ledgered %+v, want gpt on gw", row)
	}
	if calls != 0 {
		t.Fatalf("a fresh snapshot must not cost a live read, got %d", calls)
	}
	if code := dispatch("--refresh", "fix the parser"); code != 0 {
		t.Fatalf("--refresh dispatch: exit %d", code)
	}
	if calls != 1 {
		t.Fatalf("--refresh must cause exactly one live quota read, got %d", calls)
	}

	// A pinned family is the ledgered family, even against a cheaper pick.
	if code := dispatch("--family", "claude", "fix the parser"); code != 0 {
		t.Fatalf("--family claude: exit %d", code)
	}
	if row := lastLedgerRow(t); row["family"] != "claude" || row["profile"] != "sub" {
		t.Fatalf("dispatch --family claude ledgered %+v, want claude on sub", row)
	}

	// A family no served model belongs to fails cleanly, and says which.
	var out, errb bytes.Buffer
	if code := Main([]string{"dispatch", "--kind", "code", "--tier", "mid", "--family", "gemini", "fix the parser"},
		strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), "gemini") {
		t.Fatalf("a family none serves must fail by name: exit %d err %q", code, errb.String())
	}
}

// --no-decider must not ask the decider at all: a decider that would leave
// its failure note in the decision stays unasked.
func TestNoDeciderSkipsTheClassificationCall(t *testing.T) {
	dispatchEnv(t)
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	c.Decider = &Decider{URL: "http://127.0.0.1:1/decide"} // nothing answers there
	d, err := Decide(c, Request{Prompt: "fix the bug in parse()"})
	if err != nil || d.Pick == nil {
		t.Fatalf("%v %+v", err, d)
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "decider unavailable") {
		t.Fatalf("an asked decider that cannot answer leaves its note: %+v", d.Notes)
	}
	d, err = Decide(c, Request{Prompt: "fix the bug in parse()", NoDecider: true})
	if err != nil || d.Pick == nil {
		t.Fatalf("%v %+v", err, d)
	}
	for _, n := range d.Notes {
		if strings.Contains(n, "decider") {
			t.Fatalf("NoDecider must not ask the decider at all: %+v", d.Notes)
		}
	}
}

// The flags mean the same in help as they do on dispatch.
func TestDispatchHelpStatesEveryFlag(t *testing.T) {
	var out bytes.Buffer
	if code := Main([]string{"dispatch", "--help"}, strings.NewReader(""), &out, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"--family", "--no-decider", "--refresh", "--kind", "--tier", "--profile", "--model", "--cwd",
		"--dry-run", "finished answer is not scored", "when --profile and --model are both named",
		"--dry-run exits 3", "a named --profile and --model too"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("dispatch --help omits %s:\n%s", want, out.String())
		}
	}
}

// A family next to a named profile and model cannot narrow anything: the pair
// runs as named (#710), so the family is refused by name, never dropped.
func TestFamilyBesideANamedProfileAndModelIsRefusedByName(t *testing.T) {
	dispatchEnv(t)
	args := []string{"--kind", "code", "--tier", "mid", "--profile", "gw", "--model", "gpt-6-terra", "--family", "claude", "fix the parser"}
	var out, errb bytes.Buffer
	if code := Main(append([]string{"dispatch"}, args...), strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), `family "claude"`) {
		t.Fatalf("dispatch must refuse the family by name: exit %d err %q", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Main(append([]string{"dispatch", "--dry-run"}, args...), strings.NewReader(""), &out, &errb); code != 1 || !strings.Contains(errb.String(), `family "claude"`) {
		t.Fatalf("dispatch --dry-run must refuse the family by name: exit %d err %q", code, errb.String())
	}
	if rows := readRows(ledgerPath()); len(rows) != 0 {
		t.Fatalf("a refused family must reserve nothing, ledger holds %+v", rows)
	}
}

// A named profile and model still refreshes the snapshot when the caller asks:
// the pair runs as named, but --refresh is never silently dropped.
func TestRefreshReadsLiveEvenForANamedProfileAndModel(t *testing.T) {
	dispatchEnv(t)
	calls := 0
	QuotaInput = func(names []string) string { calls++; return "" }
	t.Cleanup(func() { QuotaInput = nil })
	var out, errb bytes.Buffer
	if code := Main([]string{"dispatch", "--kind", "code", "--tier", "mid", "--profile", "gw", "--model", "gpt-6-terra", "--refresh", "fix the parser"},
		strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("exit %d out %q", code, out.String())
	}
	if calls != 1 {
		t.Fatalf("--refresh on a named pair must cause exactly one live quota read, got %d", calls)
	}
	if row := lastLedgerRow(t); row["profile"] != "gw" || row["model"] != "gpt-6-terra" {
		t.Fatalf("the named pair must run as named: %+v", row)
	}
}

// --dry-run picks with the same flags and says so the way the help does: a
// family none serves exits 3 and reserves nothing.
func TestDryRunWithAFamilyNoneServesExitsThree(t *testing.T) {
	dispatchEnv(t)
	var out, errb bytes.Buffer
	if code := Main([]string{"dispatch", "--dry-run", "--kind", "code", "--tier", "mid", "--family", "gemini", "fix the parser"},
		strings.NewReader(""), &out, &errb); code != 3 {
		t.Fatalf("dispatch --dry-run --family gemini: exit %d out %q err %q", code, out.String(), errb.String())
	}
	if rows := readRows(ledgerPath()); len(rows) != 0 {
		t.Fatalf("no pick, no reservation: %+v", rows)
	}
}
