package router

import (
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akleizor/ccmorph/quota"
)

func TestSlackIsElapsedMinusUsedOfTheTightestWindow(t *testing.T) {
	now := 1000.0
	p := Profile{Windows: []Window{
		{Name: "5h", Used: 0.2, ResetsAt: now + 9000, Length: 18000},                                 // half elapsed: +30
		{Name: "7d", Used: 0.9, ResetsAt: now + 302400, Length: 604800},                              // half elapsed: -40
		{Name: "7d_opus", Used: 1, ResetsAt: now + 302400, Length: 604800, Models: []string{"opus"}}, // -50, opus only
	}}
	if got := Slack(p, now, "sonnet"); math.Abs(got+40) > 1e-9 {
		t.Fatalf("sonnet slack %v, want -40", got)
	}
	if got := Slack(p, now, "opus"); math.Abs(got+50) > 1e-9 {
		t.Fatalf("opus draws on its own spent pool: slack %v", got)
	}
	if Slack(Profile{}, now, "") != 0 {
		t.Fatal("no reading is zero slack")
	}
}

func TestChooseTakesFreeThenBehindPaceThenAheadThenPaid(t *testing.T) {
	opts := []Option{
		{Profile: "paid", Model: "m", Class: "api", Price: 1, QuotaUnknown: true},
		{Profile: "ahead", Model: "m", Class: "subscription", Slack: -10},
		{Profile: "behind-a", Model: "m", Class: "subscription", Slack: 5},
		{Profile: "behind-b", Model: "m", Class: "subscription", Slack: 30},
		{Profile: "free", Model: "m", Class: "free", Price: math.Inf(1)},
	}
	var got []string
	for _, o := range Choose(opts, nil) {
		got = append(got, o.Profile)
	}
	if want := "free behind-b behind-a ahead paid"; strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
	if Choose(opts, []string{"x"})[0].Profile != "free" {
		t.Fatal("an order naming no option changes nothing")
	}
	opts[0].Model = "x"
	if Choose(opts, []string{"x"})[0].Profile != "paid" {
		t.Fatal("the user's order comes first")
	}
}

func TestChooseTakesTheCheapestModelInsideOneProfile(t *testing.T) {
	opts := []Option{
		{Profile: "gw", Model: "dear", Class: "free", Price: 10, Rank: 1},
		{Profile: "gw", Model: "cheap", Class: "free", Price: 1, Rank: 2},
		{Profile: "gw", Model: "unpriced", Class: "free", Price: math.Inf(1), Rank: 1},
	}
	if got := Choose(opts, nil)[0].Model; got != "cheap" {
		t.Fatalf("got %s", got)
	}
}

func TestOptionsDropReservedProfilesAndKeepTopTierToTheKindsTwoFamilies(t *testing.T) {
	labels := func(_, m string) (Label, bool) { return HeuristicLabel(m), true }
	ps := []Profile{
		{Name: "main", Class: "subscription", Models: []string{"opus"}, Reserved: true, Enabled: true},
		{Name: "gw", Class: "api", Models: []string{"claude-opus-5", "claude-opus-4", "gemini-3-pro", "gpt-6-sol"}, Enabled: true},
	}
	kinds := map[string][]string{"code": {"claude", "gpt"}}
	opts, dropped, err := Options(Task{Kind: "code", Tier: "top"}, ps, labels, kinds, 0, Pin{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range opts {
		got = append(got, o.Model)
	}
	if strings.Join(got, ",") != "claude-opus-5,gpt-6-sol" {
		t.Fatalf("options %v; dropped %v", got, dropped)
	}
	joined := strings.Join(dropped, "\n")
	for _, want := range []string{"main: reserved", "gw/claude-opus-4: older served model", "gw/gemini-3-pro: outside the kind's top two families"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("dropped %q missing from:\n%s", want, joined)
		}
	}
	if _, _, err := Options(Task{Kind: "nonsense", Tier: "mid"}, ps, labels, kinds, 0, Pin{}); err == nil {
		t.Fatal("an unknown kind is refused")
	}
}

func TestOnlyLimitsATierWithNoFallback(t *testing.T) {
	labels := func(_, m string) (Label, bool) { return HeuristicLabel(m), true }
	ps := []Profile{{Name: "gw", Class: "api", Models: []string{"claude-haiku-5", "qwen-9-flash"}, Enabled: true}}
	opts, _, _ := Options(Task{Kind: "code", Tier: "small"}, ps, labels, nil, 0, Pin{Only: []string{"qwen-9-flash"}})
	if len(opts) != 1 || opts[0].Model != "qwen-9-flash" {
		t.Fatalf("got %+v", opts)
	}
	opts, _, _ = Options(Task{Kind: "code", Tier: "small"}, ps, labels, nil, 0, Pin{Only: []string{}})
	if len(opts) != 0 {
		t.Fatalf("an empty only allows none: %+v", opts)
	}
}

// The tier's effort is the pick's, not a filter on the call, and inside a
// family the earlier entry of the user's order wins even when both served
// models are in it (GH-693).
func TestOptionsEffortFollowsTierAndOrderBeatsNewer(t *testing.T) {
	labels := func(_, m string) (Label, bool) { return HeuristicLabel(m), true }
	ps := []Profile{{Name: "gw", Class: "api", Models: []string{"claude-opus-5", "claude-opus-4", "gpt-6-sol"}, Enabled: true}}
	kinds := map[string][]string{"code": {"claude", "gpt"}}
	task := Task{Kind: "code", Tier: "top"}
	base, baseDropped, err := Options(task, ps, labels, kinds, 0, Pin{})
	if err != nil {
		t.Fatalf("no effort: %v", err)
	}
	if len(base) == 0 {
		t.Fatalf("no effort: no options at all, dropped %v", baseDropped)
	}
	for _, c := range []struct {
		name string
		pin  Pin
		want string // models in order
		same bool   // the whole option list must equal the no-effort call's
	}{
		{"an explicit effort low routes exactly as no effort", Pin{Effort: "low"}, "claude-opus-5,gpt-6-sol", true},
		{"an explicit effort medium routes exactly as no effort", Pin{Effort: "medium"}, "claude-opus-5,gpt-6-sol", true},
		{"the older of two ordered models is the family's pick", Pin{Order: []string{"claude-opus-4", "claude-opus-5"}}, "claude-opus-4,gpt-6-sol", false},
		{"the newer of two ordered models is the family's pick", Pin{Order: []string{"claude-opus-5", "claude-opus-4"}}, "claude-opus-5,gpt-6-sol", false},
	} {
		opts, dropped, err := Options(task, ps, labels, kinds, 0, c.pin)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, o := range opts {
			got = append(got, o.Model)
			if o.Effort != Efforts["top"] { // the tier picks the effort, never the caller
				t.Errorf("%s: option %s carries effort %q, want %q", c.name, o.Model, o.Effort, Efforts["top"])
			}
		}
		if strings.Join(got, ",") != c.want {
			t.Errorf("%s: options %v, want %s; dropped %v", c.name, got, c.want, dropped)
		}
		if c.same && !reflect.DeepEqual(opts, base) {
			t.Errorf("%s: whole outcome differs from the no-effort call's %v", c.name, base)
		}
	}
}

func TestLabelsReadTheVendorsOwnTierWords(t *testing.T) {
	for model, want := range map[string]Label{
		"haiku":                  {Family: "claude", Tier: "small"},
		"claude-opus-5[1m]":      {Family: "claude", Tier: "top"},
		"gpt-6.1-terra":          {Family: "gpt", Tier: "mid"},
		"gemini-3.5-flash-lite":  {Family: "gemini-flash", Tier: "small"},
		"deepseek-reasoner":      {Family: "deepseek", Tier: "top"},
		"Qwen3.8-Flash-Next-FP8": {Family: "qwen", Tier: "small"},
	} {
		if got := HeuristicLabel(model); got.Family != want.Family || got.Tier != want.Tier {
			t.Errorf("%s: got %+v, want %+v", model, got, want)
		}
	}
	if got := Heuristic("Rename every file in the folder"); got.Kind != "mechanical" || got.Tier != "small" {
		t.Errorf("rename: %+v", got)
	}
	if got := Heuristic("Fix the race in the scheduler"); got.Kind != "code" || got.Tier != "top" {
		t.Errorf("race: %+v", got)
	}
}

func TestHardFailureTellsAnAccountRefusalFromAnAnswer(t *testing.T) {
	for text, want := range map[string]string{
		"API Error: 429 rate limit exceeded": "quota",
		"Error: 401 unauthorized":            "auth",
		"":                                   "empty",
		"The quota module has a bug at line 3; fixed.": "",
	} {
		if got := hardFailure(text, false, ""); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
}

// Two subscriptions, one far behind its pace: the router sends the work there,
// never to the reserved profile, and a free gateway ahead of both.
func TestDecideSendsWorkToTheSubscriptionFurthestBehindItsPace(t *testing.T) {
	root, state := t.TempDir(), t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", state)
	for name, conf := range map[string]string{"busy": "type=sub\n", "idle": "type=sub\n", "main": "type=sub\n"} {
		if err := os.WriteFile(filepath.Join(root, name+".conf"), []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reset := time.Now().Add(150 * time.Minute).Unix() // half of a 5h window elapsed
	entry := func(name string, used float64) quota.Entry {
		return quota.Entry{Profile: name, Kind: "claude", Windows: []quota.Window{{ID: "5h", Used: used, ResetsAt: &reset}}}
	}
	if err := writeJSON(filepath.Join(routerDir(), "quota.json"), snapshot{ReadAt: nowSec(),
		Entries: []quota.Entry{entry("busy", 0.8), entry("idle", 0.1), entry("main", 0)}}); err != nil {
		t.Fatal(err)
	}
	c, _ := LoadConfig()
	c.Profiles = map[string]ProfileConfig{"main": {Reserved: true}}
	d, err := Decide(c, Request{Prompt: "x", Kind: "code", Tier: "mid"})
	if err != nil || d.Pick == nil {
		t.Fatalf("no pick: %v %+v", err, d)
	}
	if d.Pick.Profile != "idle" || d.Pick.Model != "sonnet" || d.Pick.Effort != "medium" {
		t.Fatalf("pick %+v", d.Pick)
	}
	if err := os.WriteFile(filepath.Join(root, "gw.conf"), []byte("type=apikey\nbase_url=http://127.0.0.1:9\nmodel=claude-sonnet-5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.Profiles["gw"] = ProfileConfig{Class: "free"}
	if d, _ = Decide(c, Request{Prompt: "x", Kind: "code", Tier: "mid"}); d.Pick == nil || d.Pick.Profile != "gw" {
		t.Fatalf("free first: %+v", d.Pick)
	}
}

func TestANamedProfileAndModelRunAsNamedPastEveryRoutingFilter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CCMORPH_STATE_DIR", root)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.WriteFile(filepath.Join(root, "boss.conf"), []byte("type=sub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Config{Enabled: true, AdvisorOnly: []string{"fable"}, Kinds: defaultKinds,
		Profiles: map[string]ProfileConfig{"boss": {Disabled: true}}}
	d, err := Decide(c, Request{Prompt: "review", Kind: "design", Tier: "top", Profile: "boss", Model: "claude-fable-5-2"})
	if err != nil || d.Pick == nil || d.Pick.Profile != "boss" || d.Pick.Model != "claude-fable-5-2" {
		t.Fatalf("an explicit choice must run as named: %+v %v", d.Pick, err)
	}
	if _, err := Decide(c, Request{Prompt: "x", Kind: "code", Tier: "mid", Profile: "nope", Model: "m"}); err == nil {
		t.Fatal("an unknown named profile must fail loudly")
	}
}
