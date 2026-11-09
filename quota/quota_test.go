package quota

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// roundTrip is an in-process upstream: quota never needs a listening port to be
// verified, and a test that tried to bind one fails in a sandbox.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func useTransport(t *testing.T, f roundTrip) {
	t.Helper()
	old := transport
	transport = f
	t.Cleanup(func() { transport = old })
}

func reply(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// useClock pins the clock and the zone: reset times are formatted in local time.
func useClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { now = old; time.Local = local })
}

func ptr[T any](v T) *T { return &v }

func liveReply() string {
	return `{"five_hour":{"utilization":3.0},"limits":[` +
		`{"kind":"session","percent":12,"resets_at":"2026-10-29T19:10:00+00:00"},` +
		`{"kind":"weekly_all","percent":40,"resets_at":"2026-11-03T01:00:00+00:00"},` +
		`{"kind":"weekly_opus","percent":5,"resets_at":"2026-11-03T01:00:00+00:00"},` +
		`{"kind":"weekly_oauth_apps","percent":null}]}`
}

func claudeCreds(token, subscription, tier string) string {
	return fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"expiresAt":1893456000000,`+
		`"subscriptionType":%q,"rateLimitTier":%q}}`, token, subscription, tier)
}

// writeCreds lays a credential out where this platform can find it.
func writeCreds(t *testing.T, home, name, raw string) string {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// keychainFrom makes subscriptionToken see creds[dir] on macOS.
func keychainFrom(t *testing.T, creds map[string]string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	t.Setenv("LOGNAME", "quota-user")
	readKeychain = func(service, account string) ([]byte, error) {
		if account != "quota-user" {
			return nil, os.ErrNotExist
		}
		for dir, raw := range creds {
			if service == "Claude Code-credentials-"+hashOf(dir) {
				return []byte(raw), nil
			}
		}
		return nil, os.ErrNotExist
	}
}

func hashOf(dir string) string {
	sum := sha256.Sum256([]byte(dir))
	return hex.EncodeToString(sum[:])[:8]
}

// row is one expected text line: the label, the filled cells of its bar, the
// percent and the reset text.
func row(label string, filled int, pct, reset string) string {
	return fmt.Sprintf("  %-8s %s%s %4s  %s",
		label, strings.Repeat("█", filled), strings.Repeat("░", barCells-filled), pct, reset)
}

func run(t *testing.T, argv []string, stdin string) (string, string, int) {
	t.Helper()
	var out, errw bytes.Buffer
	code := Main(argv, strings.NewReader(stdin), &out, &errw)
	return out.String(), errw.String(), code
}

func TestQuota(t *testing.T) {
	at := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	useClock(t, at)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var auth []string
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		auth = append(auth, r.Header.Get("Authorization"))
		switch r.Header.Get("Authorization") {
		case "Bearer live":
			return reply(200, liveReply()), nil
		case "Bearer qt":
			// The answer a token profile's repeated reads earn once the endpoint
			// tires of refusing them (#270): the row must not change with it.
			return reply(429, ""), nil
		default:
			return reply(403, ""), nil
		}
	})

	home := t.TempDir()
	creds := map[string]string{
		"live":    claudeCreds("live", "max", "default_claude_max_20x"),
		"expired": `{"claudeAiOauth":{"accessToken":"old","expiresAt":1000,"subscriptionType":"pro","rateLimitTier":"default_claude_pro"}}`,
		// An account sign-in the usage endpoint refuses (403): its row keeps the
		// account wording, unlike a token's refusal (#270).
		"refused": claudeCreds("refused", "pro", "default_claude_pro"),
	}
	dirs := map[string]string{}
	for name, raw := range creds {
		dirs[name] = writeCreds(t, home, name, raw)
	}
	keychainFrom(t, map[string]string{
		dirs["live"]: creds["live"], dirs["expired"]: creds["expired"], dirs["refused"]: creds["refused"]})

	in := strings.Join([]string{
		"live\tsub\t" + dirs["live"],
		"expired\tsub\t" + dirs["expired"],
		"refused\tsub\t" + dirs["refused"],
		"none\tsub\t" + filepath.Join(home, "none"),
		"tk\ttoken\t" + home + "\ttk",
		"qt\ttoken\t" + home + "\tqt",
	}, "\n")
	out, errOut, code := run(t, nil, in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := strings.Join([]string{
		"Claude accounts and tokens",
		"live  Max 20x",
		row("5h", 2, "12%", "in 4h 10m"),
		row("7d", 6, "40%", "Tue 01:00"),
		row("7d Opus", 1, "5%", "Tue 01:00"),
		"expired  Pro",
		"  sign-in expired; start a session on it to refresh",
		"refused  Pro",
		"  sign-in refused; start a session on it to sign in",
		"none",
		"  not signed in — sign in again: ccmorph login none",
		"tk",
		"  plan usage needs a Claude account profile (ccmorph add)",
		"qt",
		"  plan usage needs a Claude account profile (ccmorph add)",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("\n got %q\nwant %q", out, want)
	}
	// Every sign-in that can be sent is, both tokens included: an expired or
	// missing sign-in is never sent. Whatever the endpoint answers a token, a
	// refusal (tk) or the rate limit repeated reads earn (qt), its row is the
	// one explanation above (#270).
	if strings.Join(auth, ",") != "Bearer live,Bearer refused,Bearer tk,Bearer qt" {
		t.Fatalf("sent %v", auth)
	}
	// A refused read must not overwrite the cache; a good one does.
	if _, ok := load("refused"); ok {
		t.Fatal("a refused read must not be cached")
	}
	if _, ok := load("tk"); ok {
		t.Fatal("a refused read must not be cached")
	}
	if r, ok := load("live"); !ok || r.ReadAt != at.Unix() || len(r.Windows) != 3 {
		t.Fatalf("cached %+v %v", r, ok)
	}

	// --json carries the same readings for tools such as the model router.
	jout, _, code := run(t, []string{"--json"}, in)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Profiles []Entry `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatalf("%v: %s", err, jout)
	}
	if len(doc.Profiles) != 6 {
		t.Fatalf("%d entries in %s", len(doc.Profiles), jout)
	}
	live := doc.Profiles[0]
	if live.Profile != "live" || live.Kind != "claude" || live.Plan != "Max 20x" {
		t.Fatalf("live entry %+v", live)
	}
	if len(live.Windows) != 3 || live.Windows[0].ID != "5h" || live.Windows[0].Used != 0.12 {
		t.Fatalf("windows %+v", live.Windows)
	}
	if live.Windows[1].ID != "7d" || live.Windows[2].ID != "7d_opus" {
		t.Fatalf("windows %+v", live.Windows)
	}
	if want := time.Date(2026, 10, 29, 19, 10, 0, 0, time.UTC).Unix(); live.Windows[0].ResetsAt == nil || *live.Windows[0].ResetsAt != want {
		t.Fatalf("reset %v, want %d", live.Windows[0].ResetsAt, want)
	}
	if live.ReadAt == nil || *live.ReadAt != at.Unix() || live.AgeSeconds == nil || *live.AgeSeconds != 0 || live.Stale {
		t.Fatalf("fresh entry %+v", live)
	}
	// The refusal path an account hits: its plan stays, its wording names the
	// fix, and the read happened (the auth assertion above).
	refused := doc.Profiles[2]
	if refused.Profile != "refused" || refused.Plan != "Pro" ||
		refused.Error != "sign-in refused; start a session on it to sign in" || refused.Windows == nil {
		t.Fatalf("refused entry %+v", refused)
	}
	// A token profile's explanation is one wording, in both views, whatever the
	// endpoint would answer for the token: a refusal (tk) or the rate limit its
	// repeated reads earn (qt) — #270.
	for _, p := range doc.Profiles[4:6] {
		if p.Error != "plan usage needs a Claude account profile (ccmorph add)" || p.Windows == nil {
			t.Fatalf("token entry %+v", p)
		}
	}
}

func TestRenderPieces(t *testing.T) {
	at := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	useClock(t, at)
	unix := func(s string) *int64 {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return ptr(ts.Unix())
	}
	for _, c := range []struct {
		name string
		at   *int64
		want string
	}{
		{"none", nil, ""},
		{"past", unix("2026-10-29T14:00:00Z"), "reset"},
		{"minutes", unix("2026-10-29T15:14:00Z"), "in 14m"},
		{"hours", unix("2026-10-29T17:14:00Z"), "in 2h 14m"},
		{"hour", unix("2026-10-29T16:00:00Z"), "in 1h"},
		{"days", unix("2026-11-01T09:00:00Z"), "Sun 09:00"},
		{"weeks", unix("2026-11-18T09:00:00Z"), "Nov 18"},
	} {
		if got := resetText(c.at); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		used   float64
		filled int
		pct    string
	}{
		{0, 0, "0%"},
		{0.5, 8, "50%"},
		{1, 16, "100%"},
		{1.4, 16, "140%"},
		{-0.2, 0, "-20%"},
	} {
		if got, want := bar(c.used), strings.Repeat("█", c.filled)+strings.Repeat("░", barCells-c.filled); got != want {
			t.Errorf("bar(%v) %q, want %q", c.used, got, want)
		}
		if got := percent(c.used); got != c.pct {
			t.Errorf("percent(%v) %q, want %q", c.used, got, c.pct)
		}
	}
	for _, c := range []struct {
		seconds int64
		want    string
	}{
		{0, "1 min old"},
		{720, "12 min old"},
		{3 * 3600, "3 h old"},
		{50 * 3600, "2 d old"},
	} {
		if got := ageText(c.seconds); got != c.want {
			t.Errorf("ageText(%d) %q, want %q", c.seconds, got, c.want)
		}
	}
}

func TestQuotaStale(t *testing.T) {
	at := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	useClock(t, at)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		return reply(429, ""), nil
	})
	home := t.TempDir()
	slow := writeCreds(t, home, "slow", claudeCreds("tok-slow", "pro", "default_claude_pro"))
	fresh := writeCreds(t, home, "fresh", claudeCreds("tok-fresh", "pro", "default_claude_pro"))
	keychainFrom(t, map[string]string{
		slow:  claudeCreds("tok-slow", "pro", "default_claude_pro"),
		fresh: claudeCreds("tok-fresh", "pro", "default_claude_pro"),
	})
	save("slow", Reading{
		Plan:    "Pro",
		Windows: []Window{{ID: "5h", Label: "5h", Used: 0.2, ResetsAt: ptr(at.Add(time.Hour).Unix())}},
		ReadAt:  at.Add(-12 * time.Minute).Unix(),
	})
	in := "slow\tsub\t" + slow + "\nfresh\tsub\t" + fresh + "\n"

	out, _, code := run(t, nil, in)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := strings.Join([]string{
		"Claude accounts and tokens",
		"slow  Pro  12 min old, rate-limited",
		row("5h", 3, "20%", "in 1h"),
		"fresh  Pro",
		"  not read yet: usage endpoint is rate-limiting",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("\n got %q\nwant %q", out, want)
	}

	colored, _, _ := run(t, []string{"--color"}, in)
	if !strings.Contains(colored, "\033[2mslow  Pro  12 min old, rate-limited\033[0m") {
		t.Fatalf("stale entry not dimmed: %q", colored)
	}
	if strings.Contains(colored, "\033[2mfresh") {
		t.Fatalf("a never-read entry is not stale, must not be dimmed: %q", colored)
	}

	jout, _, _ := run(t, []string{"--json"}, in)
	var doc struct {
		Profiles []Entry `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatal(err)
	}
	s := doc.Profiles[0]
	if !s.Stale || s.Error != "rate-limited" || s.AgeSeconds == nil || *s.AgeSeconds != 720 {
		t.Fatalf("stale entry %+v", s)
	}
	if s.Plan != "Pro" || len(s.Windows) != 1 || s.Windows[0].Used != 0.2 {
		t.Fatalf("cached reading %+v", s)
	}
	if doc.Profiles[1].Stale || doc.Profiles[1].Error == "" {
		t.Fatalf("never-read entry %+v", doc.Profiles[1])
	}
}

func TestQuotaSpend(t *testing.T) {
	at := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	useClock(t, at)
	// Spending is local-day money and the day folders are UTC days: a zone east
	// of UTC puts local midnight on the previous UTC day, so a folder walk that
	// steps local midnights as instants stops before today's folder. HKT is the
	// machine ccmorph is measured on.
	time.Local = time.FixedZone("HKT", 8*3600)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Errorf("a gateway or budget entry must not reach the network: %s", r.URL)
		return nil, nil
	})
	usage := filepath.Join(state, "ccmorph", "usage")
	if err := os.MkdirAll(usage, 0o700); err != nil {
		t.Fatal(err)
	}
	receipt := func(n, profile, stamp string, u map[string]any, complete bool, day ...string) {
		raw, _ := json.Marshal(map[string]any{
			"complete": complete, "profile": profile, "timestamp": stamp, "usage": u,
		})
		folder := usage
		if len(day) > 0 {
			folder = filepath.Join(usage, day[0])
			if err := os.MkdirAll(folder, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(folder, n+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	receipt("1", "gw", "2026-10-20T09:00:00Z", map[string]any{"cost": 1.2}, true)           // today, billed
	receipt("2", "gw", "2026-10-02T00:00:00Z", map[string]any{"cost": 3.3}, true)           // earlier this month
	receipt("3", "gw", "2026-10-20T10:00:00Z", map[string]any{"estimated_cost": 0.5}, true) // today, estimated
	receipt("4", "gw", "2026-09-15T00:00:00Z", map[string]any{"cost": 9.9}, true)           // last month
	receipt("5", "other", "2026-10-20T09:00:00Z", map[string]any{"cost": 7.0}, true)        // another profile
	receipt("6", "gw", "2026-10-20T09:30:00Z", map[string]any{"cost": 4.0}, false)          // incomplete
	receipt("7", "cap", "2026-10-05T00:00:00Z", map[string]any{"cost": 100.0}, true)        // a budget account
	// Turns no price covered (#234): tokens but no cost, one on gw among priced
	// turns, a profile whose every turn is unpriced, and one never launched.
	receipt("8", "gw", "2026-10-20T11:00:00Z", map[string]any{"input_tokens": 120, "output_tokens": 14}, true)
	receipt("9", "gw2", "2026-10-20T09:30:00Z", map[string]any{"input_tokens": 100, "cache_read_input_tokens": 20, "output_tokens": 14}, true)
	receipt("10", "gw2", "2026-10-05T08:00:00Z", map[string]any{"input_tokens": 100, "output_tokens": 20}, true)
	// A budget account whose every turn this month is unpriced (#301): its bar
	// must say the cost is unknown, never 0% — the picture of no spend.
	receipt("15", "capu", "2026-10-20T09:00:00Z", map[string]any{"input_tokens": 200, "output_tokens": 30}, true)
	receipt("16", "capu", "2026-10-06T09:00:00Z", map[string]any{"input_tokens": 50}, true)
	// The same store sharded by day: a row lands in the day folder of its UTC
	// stamp, and the spend still reads it beside the flat rows (#227).
	receipt("11", "gw", "2026-10-20T08:00:00Z", map[string]any{"cost": 0.25}, true, "2026-10-20")           // today, sharded
	receipt("12", "gw", "2026-10-02T00:00:00Z", map[string]any{"estimated_cost": 0.25}, true, "2026-10-02") // this month
	receipt("13", "gw", "2026-09-15T00:00:00Z", map[string]any{"cost": 50.0}, true, "2026-09-15")           // last month
	receipt("14", "other", "2026-10-20T07:00:00Z", map[string]any{"cost": 6.0}, true, "2026-10-20")         // another profile

	in := "gw\tgateway\t\t\ncap\tgateway\t\t500\ncapu\tgateway\t\t40\ncap0\tgateway\t\t40\ngw2\tgateway\t\t\nwork\tgateway\t\t\n"
	out, _, code := run(t, nil, in)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	want := strings.Join([]string{
		"Budget accounts",
		"cap",
		row("month", 3, "20%", "Nov 1"),
		"  today $0.00 · month $100.00 of $500",
		// A budget whose turns are all unpriced has no share to show: an empty
		// bar labelled cost unknown, not the 0% of a month with no spend, which
		// cap0 — never launched — still shows (#301).
		"capu",
		row("month", 0, "cost unknown", "Nov 1"),
		"  2 turns · 280 tokens this month · cost unknown",
		"cap0",
		row("month", 0, "0%", "Nov 1"),
		"  not used this month",
		"",
		"API key and provider profiles",
		"gw",
		"  today $1.95 · month $5.50 (est.)",
		"  1 turn with no price · cost unknown",
		"gw2",
		"  2 turns · 254 tokens this month · cost unknown",
		"work",
		"  not used this month",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("\n got %q\nwant %q", out, want)
	}
	checkWidth(t, out)

	jout, _, _ := run(t, []string{"--json"}, in)
	var doc struct {
		Profiles []Entry `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatal(err)
	}
	gw := doc.Profiles[0]
	if gw.Kind != "gateway" || gw.Spend == nil || gw.Spend.TodayUSD != 1.95 || gw.Spend.MonthUSD != 5.5 || !gw.Spend.Estimated {
		t.Fatalf("gw entry %+v", gw)
	}
	// The same turn and token counts the text shows, per window (#234), with
	// the sharded rows' turns counted beside the flat ones (#227).
	if gw.Spend.TodayTurns != 4 || gw.Spend.MonthTurns != 6 || gw.Spend.TodayTokens != 134 || gw.Spend.MonthTokens != 134 || gw.Spend.UnpricedTurns != 1 {
		t.Fatalf("gw counts %+v", gw.Spend)
	}
	cap := doc.Profiles[1]
	if cap.Kind != "budget" || cap.Spend == nil || cap.Spend.BudgetUSD == nil || *cap.Spend.BudgetUSD != 500 {
		t.Fatalf("cap entry %+v", cap)
	}
	if len(cap.Windows) != 1 || cap.Windows[0].ID != "month" || cap.Windows[0].Used != 0.2 {
		t.Fatalf("budget window %+v", cap.Windows)
	}
	// The all-unpriced budget keeps its --json shape: the window's used stays 0
	// beside the spend counts that say why, and the text view carries the
	// unknown (#301).
	capu := doc.Profiles[2]
	if capu.Kind != "budget" || capu.Spend == nil || capu.Spend.BudgetUSD == nil || *capu.Spend.BudgetUSD != 40 {
		t.Fatalf("capu entry %+v", capu)
	}
	if len(capu.Windows) != 1 || capu.Windows[0].Used != 0 ||
		capu.Spend.MonthTurns != 2 || capu.Spend.MonthTokens != 280 || capu.Spend.UnpricedTurns != 2 {
		t.Fatalf("capu unknown-cost entry %+v", capu)
	}
	cap0 := doc.Profiles[3]
	if cap0.Kind != "budget" || cap0.Spend == nil || cap0.Spend.MonthTurns != 0 || len(cap0.Windows) != 1 {
		t.Fatalf("cap0 entry %+v", cap0)
	}
	gw2 := doc.Profiles[4]
	if gw2.Spend == nil || gw2.Spend.TodayUSD != 0 || gw2.Spend.MonthUSD != 0 ||
		gw2.Spend.TodayTurns != 1 || gw2.Spend.MonthTurns != 2 ||
		gw2.Spend.TodayTokens != 134 || gw2.Spend.MonthTokens != 254 || gw2.Spend.UnpricedTurns != 2 {
		t.Fatalf("gw2 entry %+v", gw2.Spend)
	}
	work := doc.Profiles[5]
	if work.Spend == nil || work.Spend.MonthTurns != 0 || work.Spend.MonthTokens != 0 || work.Spend.UnpricedTurns != 0 {
		t.Fatalf("work entry %+v", work.Spend)
	}
}

var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

func checkWidth(t *testing.T, out string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		plain := []rune(ansi.ReplaceAllString(line, ""))
		if len(plain) > maxWidth {
			t.Errorf("%d runes: %q", len(plain), line)
		}
	}
}

func TestQuotaFixtureHomes(t *testing.T) {
	at := time.Date(2026, 10, 29, 15, 0, 0, 0, time.UTC)
	useClock(t, at)
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	var seen []string
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		switch r.Header.Get("Authorization") {
		case "Bearer pro-secret":
			return reply(200, `{"limits":[{"kind":"session","percent":10,"resets_at":"2026-10-29T19:10:00+00:00"},{"kind":"weekly_all","percent":20,"resets_at":"2026-11-03T01:00:00+00:00"}]}`), nil
		case "Bearer max-secret":
			return reply(429, ""), nil
		case "Bearer codex-secret":
			return reply(200, `{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":35,"limit_window_seconds":18000,"reset_at":`+
				fmt.Sprint(at.Add(2*time.Hour+14*time.Minute).Unix())+`},"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_at":`+
				fmt.Sprint(at.Add(72*time.Hour).Unix())+`}}}`), nil
		}
		return reply(403, ""), nil
	})

	home := t.TempDir()
	pro := writeCreds(t, home, "pro", claudeCreds("pro-secret", "pro", "default_claude_pro"))
	maxd := writeCreds(t, home, "max", claudeCreds("max-secret", "max", "default_claude_max_5x"))
	keychainFrom(t, map[string]string{
		pro:  claudeCreds("pro-secret", "pro", "default_claude_pro"),
		maxd: claudeCreds("max-secret", "max", "default_claude_max_5x"),
	})
	codexDir := filepath.Join(home, "auth", "cx")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	codexCred := `{"type":"codex","access_token":"codex-secret","account_id":"acct-1","expired":"2030-01-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(codexDir, "codex-a@b.c.json"), []byte(codexCred), 0o600); err != nil {
		t.Fatal(err)
	}
	// The Max profile was read twelve minutes ago and now answers 429.
	save("max", Reading{
		Plan: "Max 5x",
		Windows: []Window{
			{ID: "5h", Label: "5h", Used: 0.55, ResetsAt: ptr(at.Add(90 * time.Minute).Unix())},
			{ID: "7d", Label: "7d", Used: 0.7, ResetsAt: ptr(at.Add(48 * time.Hour).Unix())},
		},
		ReadAt: at.Add(-12 * time.Minute).Unix(),
	})
	usage := filepath.Join(state, "ccmorph", "usage")
	if err := os.MkdirAll(usage, 0o700); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("gw", 35) // 70 characters: forces the 60-column truncation
	gwReceipt, _ := json.Marshal(map[string]any{
		"complete": true, "profile": long, "timestamp": "2026-10-29T09:00:00Z",
		"usage": map[string]any{"cost": 2.5},
	})
	if err := os.WriteFile(filepath.Join(usage, "1.json"), gwReceipt, 0o600); err != nil {
		t.Fatal(err)
	}

	in := strings.Join([]string{
		"pro\tsub\t" + pro,
		"max\tsub\t" + maxd,
		"cx\tcodex\t" + codexDir,
		long + "\tgateway\t\t",
	}, "\n")
	out, errOut, code := run(t, nil, in)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"Claude accounts and tokens", "Codex subscriptions", "API key and provider profiles",
		"pro  Pro", row("5h", 2, "10%", "in 4h 10m"),
		"max  Max 5x  12 min old, rate-limited", row("5h", 9, "55%", "in 1h 30m"),
		"cx  Plus", row("5h", 6, "35%", "in 2h 14m"), row("7d", 10, "60%", "Sun 15:00"),
		"  today $2.50 · month $2.50",
		string([]rune(long)[:maxWidth-1]) + "…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	checkWidth(t, out)
	colored, _, _ := run(t, []string{"--color"}, in)
	checkWidth(t, colored)
	if strings.Contains(out, "\x1b") {
		t.Errorf("no-colour output carries escapes: %q", out)
	}

	jout, _, _ := run(t, []string{"--json"}, in)
	var doc struct {
		Profiles []Entry `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Profiles) != 4 {
		t.Fatalf("%d entries", len(doc.Profiles))
	}
	if p := doc.Profiles[1]; !p.Stale || p.AgeSeconds == nil || *p.AgeSeconds != 720 || len(p.Windows) != 2 || p.Windows[0].Used != 0.55 {
		t.Fatalf("stale fixture %+v", p)
	}
	if c := doc.Profiles[2]; c.Plan != "Plus" || len(c.Windows) != 2 || c.Windows[0].Used != 0.35 || c.Windows[1].ID != "7d" {
		t.Fatalf("codex fixture %+v", c)
	}
	if g := doc.Profiles[3]; g.Profile != long || g.Spend == nil || g.Spend.MonthUSD != 2.5 || g.Spend.BudgetUSD != nil {
		t.Fatalf("gateway fixture %+v", g)
	}

	// Never billable: every request is a usage read, none an inference call.
	if len(seen) != 9 {
		t.Fatalf("requests %v", seen)
	}
	for _, r := range seen {
		if r != "GET /api/oauth/usage" && r != "GET /backend-api/wham/usage" {
			t.Errorf("a request outside the usage endpoints: %s", r)
		}
	}

	// Credentials never reach output or the cache.
	secrets := []string{"pro-secret", "max-secret", "codex-secret", "acct-1", "Bearer"}
	for _, secret := range secrets {
		for _, blob := range []string{out, jout, colored} {
			if strings.Contains(blob, secret) {
				t.Errorf("%q reached the output", secret)
			}
		}
	}
	filepath.Walk(state, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, secret := range secrets {
			if strings.Contains(string(b), secret) {
				t.Errorf("%q reached %s", secret, path)
			}
		}
		return nil
	})
}

// The service name is shared with an MCP-only keychain item under another
// account, so the lookup must name the user's account (as
// addons/remote-control/ccmorph-rc-hosts does).
func TestSubscriptionTokenNamesTheKeychainAccount(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the keychain is macOS")
	}
	t.Setenv("LOGNAME", "quota-user")
	orig := readKeychain
	t.Cleanup(func() { readKeychain = orig })
	var gotService, gotAccount string
	readKeychain = func(service, account string) ([]byte, error) {
		gotService, gotAccount = service, account
		return []byte(`{"claudeAiOauth":{"accessToken":"live","expiresAt":1893456000000}}`), nil
	}
	dir := filepath.Join(t.TempDir(), "live")
	tok, plan, err := subscriptionToken(dir)
	if err != nil || tok != "live" || plan != "" {
		t.Fatalf("%q %q %v", tok, plan, err)
	}
	if gotAccount != "quota-user" {
		t.Fatalf("account %q", gotAccount)
	}
	if want := "Claude Code-credentials-" + hashOf(dir); gotService != want {
		t.Fatalf("service %q, want %q", gotService, want)
	}
}

func TestQuotaBalance(t *testing.T) {
	useClock(t, time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC))
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	replies := map[string]string{
		"api.deepseek.com/user/balance":                                  `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"0.00"},{"currency":"CNY","total_balance":"40.75"}]}`,
		"openrouter.ai/api/v1/credits":                                   `{"data":{"total_credits":100,"total_usage":25.5}}`,
		"api.moonshot.ai/v1/users/me/balance":                            `{"code":0,"data":{"available_balance":12.3}}`,
		"api.siliconflow.cn/v1/user/info":                                `{"code":20000,"data":{"totalBalance":"8.5"}}`,
		"api.stepfun.ai/v1/accounts":                                     `{"balance":3}`,
		"api.z.ai/api/biz/account/query-customer-account-report":         `{"success":true,"data":{"availableBalance":7}}`,
		"open.bigmodel.cn/api/biz/account/query-customer-account-report": `{"success":false,"msg":"token expired or incorrect"}`,
	}
	var sent []string
	useTransport(t, func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.URL.Host+" "+r.Header.Get("Authorization"))
		if strings.HasSuffix(r.Header.Get("Authorization"), "bad") {
			return reply(401, ""), nil
		}
		body, ok := replies[r.URL.Host+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s", r.URL)
			return reply(404, ""), nil
		}
		return reply(200, body), nil
	})
	in := strings.Join([]string{
		"ds\tgateway\thttps://api.deepseek.com/anthropic\t\tsk-ds",
		"or\tgateway\thttps://openrouter.ai/api\t\tsk-or",
		"kimi\tgateway\thttps://api.moonshot.ai/anthropic\t\tsk-k",
		"sf\tgateway\thttps://api.siliconflow.cn\t\tsk-sf",
		"glm\tgateway\thttps://api.z.ai/api/anthropic\t\tzk",
		"glmno\tgateway\thttps://open.bigmodel.cn/api/anthropic\t\trefused",
		"step\tgateway\thttps://api.stepfun.ai/step_plan\t\tsk-st",
		"badkey\tgateway\thttps://api.deepseek.com\t\tbad",
		"gw\tgateway\thttps://gateway.example\t\tsk-gw",
	}, "\n")
	jout, _, code := run(t, []string{"--json"}, in)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var doc struct {
		Profiles []Entry `json:"profiles"`
	}
	if err := json.Unmarshal([]byte(jout), &doc); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range doc.Profiles {
		got[e.Profile] = e.Balance + "|" + e.Error
	}
	want := map[string]string{
		"ds": "¥40.75|", "or": "$74.50|", "kimi": "$12.30|", "sf": "¥8.50|",
		"glm": "$7.00|", "glmno": "|balance: key refused: token expired or incorrect",
		"step": "$3.00|", "badkey": "|balance: key refused by api.deepseek.com", "gw": "|",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q want %q", k, got[k], v)
		}
	}
	// Zhipu takes the bare key, the rest a Bearer token; an unknown host is never sent a key.
	joined := strings.Join(sent, ",")
	if strings.Contains(joined, "sk-gw") || !strings.Contains(joined, "api.z.ai zk") || !strings.Contains(joined, "Bearer sk-ds") {
		t.Fatalf("sent %v", sent)
	}
	out, _, _ := run(t, nil, "ds\tgateway\thttps://api.deepseek.com/anthropic\t\tsk-ds\n")
	if want := "API key and provider profiles\nds\n  not used this month\n  balance ¥40.75\n"; out != want {
		t.Fatalf("\n got %q\nwant %q", out, want)
	}
}

func TestDeepseekShowsEmptyWalletsOnlyWhenAllAreEmpty(t *testing.T) {
	got, err := deepseekBalance([]byte(`{"balance_infos":[{"currency":"USD","total_balance":"0.00"},{"currency":"CNY","total_balance":0}]}`))
	if err != nil || got != "$0.00 ¥0.00" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := deepseekBalance([]byte(`{"balance_infos":[]}`)); err == nil {
		t.Fatal("an empty wallet list is unreadable, not zero")
	}
}
