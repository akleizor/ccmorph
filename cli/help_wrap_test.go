package cli

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/router"
	"github.com/akleizor/ccmorph/util/paths"
)

// helpPage is one help page as usage prints it wrapped to width columns. The
// width is the caller's, so pages can be checked at 80 columns without a
// terminal; 0 is the width a pipe receives.
func helpPage(t *testing.T, topic string, width int) string {
	t.Helper()
	return helpPageFrom(t, &launcher{}, topic, width)
}

// helpPageFrom is helpPage on a launcher the caller built, so a page can be
// checked as the machine that resolved it prints it.
func helpPageFrom(t *testing.T, l *launcher, topic string, width int) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	rc := l.usageAt(topic, width)
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	if rc != 0 {
		t.Fatalf("help %q at %d columns: rc %d", topic, width, rc)
	}
	return string(b)
}

// GH-364: help pages were fixed lines with no regard for the terminal's
// width, so an 80-column terminal cut them inside words. Wrapped to 80, no
// prose line may be wider, and the words must be exactly the piped page's
// words: wrapping re-breaks lines, it never splits or drops a word. The router
// page's settings path is copied, not read, so it is printed outside the wrap
// (GH-730); the router test measures it at 80 columns.
func TestEveryHelpPageFitsEightyColumnsWithoutCuttingAWord(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no bridge on PATH: the plain add page
	topics := []string{"", "all"}
	for topic := range usageTopics {
		topics = append(topics, topic)
	}
	for _, topic := range topics {
		wide, piped := helpPage(t, topic, 80), helpPage(t, topic, 0)
		lines := strings.Split(wide, "\n")
		for i, line := range lines {
			// The router page's last line is its settings path, which the
			// page prints unwrapped so it stays one copyable piece (GH-730).
			if topic == "router" && i == len(lines)-1 {
				continue
			}
			if cells(line) > 80 {
				t.Errorf("help %q at 80 columns: line of %d cells: %q", topic, cells(line), line)
			}
		}
		if got, want := strings.Join(strings.Fields(wide), " "), strings.Join(strings.Fields(piped), " "); got != want {
			t.Errorf("help %q wrapped to 80 lost or cut words:\n got: %q\nwant: %q", topic, got, want)
		}
	}
}

// GH-1035: the update and uninstall pages owe a brew user its command, and the
// command must stay copyable. The guard above proves no word is cut, but it
// would not notice "brew upgrade" ending a line and "ccmorph" opening the next
// — words preserved, command uncopyable. The commands are authored as short
// lines of their own, which the wrap never re-breaks; this proves it.
func TestUpdateAndUninstallHelpPrintTheBrewCommandWholeAtEightyColumns(t *testing.T) {
	for _, page := range []struct{ topic, command string }{
		{"update", "brew upgrade ccmorph"},
		{"uninstall", "brew uninstall ccmorph"},
	} {
		found := false
		for _, line := range strings.Split(helpPage(t, page.topic, 80), "\n") {
			if strings.Contains(line, page.command) {
				found = true
			}
		}
		if !found {
			t.Errorf("help %q at 80 columns never prints %q on one line", page.topic, page.command)
		}
	}
}

// GH-364: with a bridge installed, help add carries its longest line — the
// provider sentence. On an 80-column terminal it wraps as one list item, its
// continuation under the item's own indent, while a pipe still receives the
// single authored line.
func TestHelpAddWrapsTheProviderSentenceAsOneListItem(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\n"
	for _, p := range []string{"claude", "antigravity", "codex", "devin", "kimi-ai", "kimi", "meta", "xai"} {
		script += "echo '  -" + p + "-login'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "cliproxyapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	wide, piped := helpPage(t, "add", 80), helpPage(t, "add", 0)
	sentence := "  Antigravity, Codex, Devin, Kimi For Coding (global), Kimi For Coding (China), Meta, xAI: sign in to that service with your browser (through cliproxyapi)."
	if !strings.Contains(piped, sentence) {
		t.Fatalf("piped help add lost the authored provider line:\n%s", piped)
	}
	lines := strings.Split(wide, "\n")
	first := -1
	for i, line := range lines {
		if strings.HasPrefix(line, "  Antigravity,") {
			first = i
			break
		}
	}
	if first < 0 {
		t.Fatalf("help add at 80 columns lost the provider sentence:\n%s", wide)
	}
	if want := "  Antigravity, Codex, Devin, Kimi For Coding (global), Kimi For Coding (China),"; lines[first] != want {
		t.Errorf("provider line = %q, want %q", lines[first], want)
	}
	if want := "  Meta, xAI: sign in to that service with your browser (through cliproxyapi)."; lines[first+1] != want {
		t.Errorf("provider continuation = %q, want %q", lines[first+1], want)
	}
}

// GH-371: probe --help used to be context-probe's argparse dump, twelve
// options and no meaning. The page is the cli's now: it opens with a capital
// Usage line, says what the command looks up, and gives every option the
// parser accepts — plus -i, which the cli adds — one line with its meaning.
// The names come from the parser's own list, so an option cannot ship unhelped.
func TestProbeHelpGivesEveryOptionOneLine(t *testing.T) {
	for _, width := range []int{0, 80} {
		page := helpPage(t, "probe", width)
		if !strings.HasPrefix(page, "Usage: ccmorph probe [profile] [options]\n") {
			t.Fatalf("width %d: probe page starts %q", width, page)
		}
		if !strings.Contains(page, "context window") {
			t.Errorf("width %d: probe page does not say what it looks up:\n%s", width, page)
		}
		meanings := map[string]string{}
		counts := map[string]int{}
		for _, line := range strings.Split(page, "\n") {
			if !strings.HasPrefix(line, "  -") {
				continue
			}
			row := strings.TrimPrefix(line, "  ")
			cut := strings.Index(row, "  ")
			if cut < 0 {
				continue
			}
			label, meaning := row[:cut], strings.TrimSpace(row[cut:])
			for _, name := range strings.FieldsFunc(label, func(r rune) bool { return r == ' ' || r == ',' }) {
				meanings[name], counts[name] = meaning, counts[name]+1
			}
		}
		names := append(append([]string{}, ctxwindow.ProbeOptions...), "-i")
		for _, name := range names {
			if meanings[name] == "" || counts[name] != 1 {
				t.Errorf("width %d: want one option row with a meaning for %s, got %d:\n%s",
					width, name, counts[name], page)
			}
		}
	}
}

// GH-384: no help page named masking, so a person who met the word on the
// inspect screen could not learn what it is or how to turn it on without
// opening the guide. The command catalog names it, and the two pages someone
// checks from that row — inspect, the screen that shows it, and config, the
// settings command — carry the switch that turns it on.
func TestHelpNamesMaskingAndHowToTurnItOn(t *testing.T) {
	for _, topic := range []string{"all", "inspect", "config"} {
		for _, width := range []int{0, 80} {
			// The word the way a person greps for it: any case, as in the
			// issue's `ccmorph help <topic> | grep -i mask`.
			if page := helpPage(t, topic, width); !strings.Contains(strings.ToLower(page), "masking") {
				t.Errorf("help %q at %d columns never names masking:\n%s", topic, width, page)
			}
		}
	}
	for _, topic := range []string{"inspect", "config"} {
		if page := helpPage(t, topic, 0); !strings.Contains(page, "env.CCMORPH_MASK=1") {
			t.Errorf("help %q names no switch that turns masking on:\n%s", topic, page)
		}
	}
}

// GH-384, follow-up: the masking paragraphs spelled the conf file
// ~/.config/ccmorph/<profile>.conf for every reader while the launch reads the
// state root CCMORPH_STATE_DIR or XDG_CONFIG_HOME selects, so on a redirected
// host the advertised path was a file the launch never read — adding the line
// there left inspect reporting masking off. The pages name the root this
// launcher resolved; the default root keeps its tilde spelling.
func TestHelpNamesTheConfFileAtTheResolvedStateRoot(t *testing.T) {
	// The fixture names the home its roots are read against: a host whose
	// temporary directory sits under the real home would otherwise put the
	// "redirected" root there too, and the page would spell it ~/… while the
	// test still demanded the absolute spelling (#730 verification).
	home := t.TempDir()
	t.Setenv("HOME", home)
	redirected := filepath.Join(t.TempDir(), "elsewhere", "ccmorph")
	underHome := filepath.Join(home, "moved-ccmorph")
	defaultRoot := filepath.Join(home, ".config", "ccmorph")
	spellings := map[string]string{
		redirected:  redirected,
		underHome:   "~/moved-ccmorph",
		defaultRoot: "~/.config/ccmorph",
	}
	for root, spelled := range spellings {
		for _, topic := range []string{"inspect", "config"} {
			l := &launcher{dir: root}
			if page := helpPageFrom(t, l, topic, 0); !strings.Contains(page, spelled+"/<profile>.conf") {
				t.Errorf("help %q with the state root at %s does not name it as %s:\n%s", topic, root, spelled, page)
			}
		}
	}
	// A redirected page must not keep pointing at the default root.
	for _, topic := range []string{"inspect", "config"} {
		if page := helpPageFrom(t, &launcher{dir: redirected}, topic, 0); strings.Contains(page, "~/.config/ccmorph") {
			t.Errorf("help %q with the state root at %s still names the default root:\n%s", topic, redirected, page)
		}
	}
}

// GH-392: the README called profile.conf.example the template that lists every
// allowed key, but no help page named it, and a Homebrew install keeps it in
// its prefix, far from the profiles. help config names the file this install
// holds: at the code root on a clone or one-line install, in share/ on Homebrew
// and in a checkout; with neither, it says the file is missing.
func TestHelpConfigNamesTheProfileTemplateThisInstallHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the roots below are not under it: absolute spellings
	for _, rel := range []string{"profile.conf.example", filepath.Join("share", "profile.conf.example")} {
		root := t.TempDir()
		tpl := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(tpl), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tpl, []byte("type=apikey\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		page := helpPageFrom(t, &launcher{libDir: filepath.Join(root, "lib")}, "config", 0)
		if !strings.Contains(page, "\n  "+tpl+"\n") {
			t.Errorf("help config with the template at %s does not name it on a line of its own:\n%s", tpl, page)
		}
	}
	root := t.TempDir()
	page := helpPageFrom(t, &launcher{libDir: filepath.Join(root, "lib")}, "config", 0)
	flat := strings.Join(strings.Fields(page), " ")
	if !strings.Contains(flat, "the template profile.conf.example, which this install is missing: reinstall ccmorph to restore it.") {
		t.Errorf("help config with no template does not say it is missing:\n%s", page)
	}
	if strings.Contains(page, root) {
		t.Errorf("help config with no template names a path under %s:\n%s", root, page)
	}
}

// GH-730: help router ended with Settings: `~/state-verify/router.json`. —
// the resolved state root rewritten to a ~/ spelling that pastes to a file
// that is not there, wrapped in markdown backticks this plain-text page
// prints raw — while `ccmorph router status` named the real file two screens
// away. The page names the file as the command names it, in plain text:
// a line of the page IS router.ConfigPath() — nothing glued before it,
// nothing glued after it — so selecting the line copies the exact file the
// router reads, for any spelling of the root, "." among them (a substring
// check let both a joined-on period and a "./" the router never prints pass).
func TestHelpRouterNamesTheSettingsFileAsTheRouterDoes(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no bridge on PATH: the plain pages
	// The third root is longer than a terminal is wide and holds two spaces in
	// a row: a page that wrapped it as prose broke it across lines and
	// collapsed the spaces, so the path must be printed outside the wrap
	// (GH-730).
	roots := []string{
		filepath.Join(t.TempDir(), "state-verify"),
		".",
		filepath.Join(t.TempDir(), "a home directory with two  spaces in its name", "state-verify"),
	}
	for _, root := range roots {
		t.Setenv("CCMORPH_STATE_DIR", root)
		want := "  " + router.ConfigPath()
		for _, width := range []int{0, 80} {
			page := helpPageFrom(t, &launcher{dir: paths.ProfileRoot()}, "router", width)
			lines := strings.Split(strings.TrimRight(page, "\n"), "\n")
			if last := lines[len(lines)-1]; last != want {
				t.Errorf("state root %q at %d columns: the page's last line is %q, want exactly %q:\n%s", root, width, last, want, page)
			}
			if n := strings.Count(page, "router.json"); n != 1 {
				t.Errorf("state root %q at %d columns: the page names router.json %d times, want once:\n%s", root, width, n, page)
			}
		}
	}
	l := &launcher{dir: t.TempDir()}
	topics := []string{"", "all"}
	for topic := range usageTopics {
		topics = append(topics, topic)
	}
	for _, topic := range topics {
		if page := helpPageFrom(t, l, topic, 0); strings.Contains(page, "`") {
			t.Errorf("help %q prints markdown backticks on a plain-text page:\n%s", topic, page)
		}
	}
}

// GH-730, second refusal: a reader took the closing delimiter of the router
// page's raw string for a backtick printed after the path. The template
// itself holds no backtick at all, and ends with the settings path alone on
// its own indented line: nothing follows router.json in the source either.
func TestHelpRouterTemplateHoldsNoBacktick(t *testing.T) {
	body := usageTopics["router"][1]
	if strings.Contains(body, "`") {
		t.Errorf("the router page template holds a backtick:\n%s", body)
	}
	if !strings.HasSuffix(body, "\n  ~/.config/ccmorph/router.json") {
		t.Errorf("the router page template does not end with the settings path alone on its line:\n%q", body)
	}
}

// GH-730, same screen: help all sold the router as `ccmorph router [on|off]`
// while its own page, and the command, teach status, dispatch and report.
// The list line is the page's synopsis, so the two cannot drift apart.
func TestHelpAllCarriesTheRouterSynopsis(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	all := helpPageFrom(t, &launcher{dir: t.TempDir()}, "all", 0)
	if !strings.Contains(all, router.Usage) {
		t.Errorf("help all does not carry the router page's synopsis (%s):\n%s", router.Usage, all)
	}
	if strings.Contains(all, "router [on|off]") {
		t.Errorf("help all still carries the stale two-verb synopsis:\n%s", all)
	}
}

// GH-364: a description longer than the width left after its label column
// wraps under that column, the way the doctor row's description already sits
// under the column its long usage line opens.
func TestHelpAllWrapsALongDescriptionUnderItsOwnColumn(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	lines := strings.Split(helpPage(t, "all", 80), "\n")
	row := -1
	for i, line := range lines {
		if strings.Contains(line, "Delete a profile") {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("help all at 80 columns lost the rm row:\n%s", strings.Join(lines, "\n"))
	}
	col := strings.Index(lines[row], "Delete")
	if want := strings.Repeat(" ", col) + "state"; lines[row+1] != want {
		t.Errorf("rm row continues with %q, want %q", lines[row+1], want)
	}
}

// GH-1034: the overview list printed its own spelling of every command's
// arguments and taught login, models, get, set, cred (and use on the short
// page) as demanding a profile that their pages and parsers leave optional.
// Each row now carries the command's own page synopsis. overviewSynopsis reads
// that line from the page, so get and set are checked as printed and not
// through usageTopics' empty map fields. The whole label must stand as the row:
// the label inside a longer label, a description or a footer is not a row, and
// the vocabulary is driven from realVerbs — not from the rows that survived —
// so a command dropped from either overview fails here.
func TestHelpOverviewCarriesTheSynopsisOfEveryCommand(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	short, all := helpPage(t, "", 0), helpPage(t, "all", 0)
	shortW, allW := helpPage(t, "", 80), helpPage(t, "all", 80)
	// The short page's everyday set, named here so a row dropped from the
	// short page fails this test rather than hiding in the all-page match.
	everyday := map[string]bool{"run": true, "hop": true, "add": true, "list": true,
		"quota": true, "use": true, "config": true, "inspect": true, "doctor": true}
	for _, topic := range realVerbs {
		switch {
		case topic == "help" || topic == "version":
			continue // the footer's own lines cover these two
		case topic == "run":
			continue // shown per use; TestHelpOverviewStillTeachesEveryRunUse
		}
		usage := overviewSynopsis(t, topic)
		if !overviewHasRow(all, usage) {
			t.Errorf("help all does not carry %s's page synopsis %q as a full row label:\n%s", topic, usage, all)
		}
		if !overviewHasRow(allW, usage) {
			t.Errorf("help all at 80 columns does not carry %s's synopsis %q:\n%s", topic, usage, allW)
		}
		if everyday[topic] && (!overviewHasRow(short, usage) || !overviewHasRow(shortW, usage)) {
			t.Errorf("help (short page) does not carry %s's synopsis %q:\n%s", topic, usage, short)
		}
	}
	// The shapes the issue read off the screen, plus the --json and [options]
	// syntax a substring match would miss, pinned exactly as printed.
	for _, want := range []string{
		"ccmorph login [profile|-i]",
		"ccmorph models [profile] [--filtered|--filtered-json]",
		"ccmorph get [profile] [model|effort|base_url|api_key|--json]",
		"ccmorph set [profile] [model|effort] [value]",
		"ccmorph cred [profile]",
		"ccmorph price <model>... [--json]",
		"ccmorph check <profile> <model> [--json]",
		"ccmorph probe [profile] [options]",
	} {
		if !overviewHasRow(all, want) {
			t.Errorf("help all does not print %q as a row:\n%s", want, all)
		}
	}
	// The rows are exactly the vocabulary: every verb but the two the footer
	// covers on help all, and exactly the everyday set on the short page.
	wantAll := []string{}
	wantShort := []string{}
	for _, v := range realVerbs {
		if v == "help" || v == "version" {
			continue
		}
		wantAll = append(wantAll, v)
		if everyday[v] {
			wantShort = append(wantShort, v)
		}
	}
	sort.Strings(wantAll)
	sort.Strings(wantShort)
	if got := rowVerbs(all); !reflect.DeepEqual(got, wantAll) {
		t.Errorf("help all teaches %v, want the full vocabulary %v", got, wantAll)
	}
	if got := rowVerbs(short); !reflect.DeepEqual(got, wantShort) {
		t.Errorf("help (short page) teaches %v, want the everyday set %v", got, wantShort)
	}
}

// GH-1034: the five list lines the walk read plus use on the short page held
// the required brackets their commands never demanded. These exact stale
// shapes are the regression pin; the generation checked above keeps every
// other command out of the class.
func TestHelpOverviewNoLongerDemandsOptionalProfiles(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, topic := range []string{"", "all"} {
		page := helpPage(t, topic, 0)
		for _, stale := range []string{
			"ccmorph login <profile>",
			"ccmorph models <profile>",
			"ccmorph get <profile> <field",
			"ccmorph set <profile> <model",
			"ccmorph cred <profile>",
			"ccmorph use <profile>",
		} {
			if strings.Contains(page, stale) {
				t.Errorf("help %q still prints the stale required shape %q:\n%s", topic, stale, page)
			}
		}
	}
}

// GH-1034 kept run as the one command shown per use: each row is one whole
// invocation whose brackets are true for that use, the three together name
// every everyday use, and none teaches a profile as required to launch.
func TestHelpOverviewStillTeachesEveryRunUse(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, topic := range []string{"", "all"} {
		for _, width := range []int{0, 80} {
			page := helpPage(t, topic, width)
			for _, row := range []string{"ccmorph run", "ccmorph run <profile>", "ccmorph run -i"} {
				if !overviewHasRow(page, row) {
					t.Errorf("help %q at width %d lost the run row %q:\n%s", topic, width, row, page)
				}
			}
		}
	}
}

// GH-1034: the map is the same map before and after the first profile exists
// — a saved profile changes the footer's Start here line (an example, not a
// row), never the commands on offer.
func TestHelpOverviewRowsDoNotDependOnSavedProfiles(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	empty, saved := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(saved, "work.conf"), []byte("type=apikey\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, topic := range []string{"", "all"} {
		fresh := helpPageFrom(t, &launcher{dir: empty}, topic, 0)
		after := helpPageFrom(t, &launcher{dir: saved}, topic, 0)
		if !strings.Contains(fresh, "Start here: ccmorph add work") {
			t.Errorf("help %q with no profile does not say where to start:\n%s", topic, fresh)
		}
		if strings.Contains(after, "Start here") {
			t.Errorf("help %q with a saved profile still sends a started reader to a first profile:\n%s", topic, after)
		}
		if got, want := overviewRows(after), overviewRows(fresh); !reflect.DeepEqual(got, want) {
			t.Errorf("help %q teaches different rows once a profile exists:\n got %q\nwant %q", topic, got, want)
		}
	}
}

// GH-1034 "each line names every use": a row is its synopsis plus description,
// and rows taught less than their own pages do. login sold one of the three
// uses (and must not claim replacing a key, which the page sends to config),
// and never said the Claude-account re-login happens in its browser; hop hid
// that running it bare lists the profiles to choose from; add hid the
// long-lived Claude token a script signs in with; use hid that running bare
// shows the current default; context hid reading a limit; probe said a flat
// "no inference" although measuring is opt-in, not absent. The named-word
// patterns are the reader's, not phrases only this test knows: listing is a
// list/show/name/see/display/print verb with what it lists, the token is the
// word token, and the browser must sit on the same side of no semicolon or
// comma as the Claude account it belongs to. Checked whitespace-normalized at
// both widths, where the 80-column row wraps its description under the label
// column.
var (
	hopListingUse = regexp.MustCompile(`(?i)\b(?:list|show|name|see|display|print)\w*\b`)
	hopListedSet  = regexp.MustCompile(`(?i)\b(?:choices|options|destinations|profiles)\b`)
	addTokenUse   = regexp.MustCompile(`(?i)\btokens?\b`)
	browserReauth = regexp.MustCompile(`\b(?:browser|web sign[ -]?in)\b[^;,]*\bClaude\b|\bClaude\b[^;,]*\b(?:browser|web sign[ -]?in)\b`)
)

func TestHelpRowsNameTheUsesTheirPagesTeach(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, width := range []int{0, 80} {
		short, all := helpPage(t, "", width), helpPage(t, "all", width)
		login := overviewRowText(t, all, "ccmorph login [profile|-i]", width)
		for _, use := range []string{"Claude account", "provider account", "refused API key"} {
			if !strings.Contains(login, use) {
				t.Errorf("width %d: the login row hides a use, no %q in %q", width, use, login)
			}
		}
		if !browserReauth.MatchString(login) {
			t.Errorf("width %d: the login row never names the browser the Claude re-login runs in: %q", width, login)
		}
		if strings.Contains(login, "replace") {
			t.Errorf("width %d: the login row claims a use the page sends to config: %q", width, login)
		}
		for _, page := range []string{short, all} {
			hopRow := overviewRowText(t, page, "ccmorph hop [profile]", width)
			if !hopListingUse.MatchString(hopRow) || !hopListedSet.MatchString(hopRow) {
				t.Errorf("width %d: the hop row does not name the profiles bare listing shows: %q", width, hopRow)
			}
			// The hop guard in tests/cli_messages_test.py reads the first
			// screen's hop row for this phrase: a rewrite must append the new
			// use, never trade the in-session guidance away for it.
			if !strings.Contains(hopRow, "inside Claude") {
				t.Errorf("width %d: the hop row dropped 'inside Claude', which tests/cli_messages_test.py reads on the first screen: %q", width, hopRow)
			}
			addRow := overviewRowText(t, page, "ccmorph add <profile>", width)
			if !addTokenUse.MatchString(addRow) {
				t.Errorf("width %d: the add row hides the long-lived Claude token for scripts/CI: %q", width, addRow)
			}
		}
		for _, page := range []string{short, all} {
			row := overviewRowText(t, page, "ccmorph use [profile|-i]", width)
			if !strings.Contains(row, "no argument shows the current default") {
				t.Errorf("width %d: the use row does not teach what running bare does: %q", width, row)
			}
		}
		ctx := overviewRowText(t, all, "ccmorph context <profile> <model> [tokens|auto]", width)
		for _, w := range []string{"Read", "set", "automatic"} {
			if !strings.Contains(ctx, w) {
				t.Errorf("width %d: the context row hides a use, no %q in %q", width, w, ctx)
			}
		}
		probe := overviewRowText(t, all, "ccmorph probe [profile] [options]", width)
		if !strings.Contains(probe, "opt-in") {
			t.Errorf("width %d: the probe row does not separate the default lookup from opted-in measuring: %q", width, probe)
		}
		if strings.Contains(all, "no inference") {
			t.Errorf("width %d: the overview still sells probe's default as an unconditional guarantee:\n%s", width, all)
		}
	}
}

// GH-1034: the overview borrows each page's synopsis string, never the page.
// Rendering a detail body for the list would drag the add page's bridge probe
// into every `ccmorph help`: with a cliproxyapi installed the overview must
// not run it, while the add page still names the installed providers.
func TestHelpOverviewNeverRunsTheProviderBridge(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "bridge-ran")
	// The stub writes its witness with the shell's own ":" and a redirection:
	// no PATH lookup. The test narrows PATH to this one directory, so a
	// `touch` in here is never found and the witness would stay silent whether
	// or not the bridge ran — a check that cannot fail.
	script := "#!/bin/sh\n: > '" + marker + "'\n"
	for _, p := range []string{"claude", "antigravity", "codex", "devin", "kimi-ai", "kimi", "meta", "xai"} {
		script += "echo '  -" + p + "-login'\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "cliproxyapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// Prove the witness live before trusting its silence: the add page is the
	// one page that must probe an installed bridge, so its body must leave the
	// marker. A marker that never lands cannot testify that the overviews were
	// quiet.
	if page := helpPage(t, "add", 0); !strings.Contains(page, "Antigravity") {
		t.Fatalf("help add stopped naming the installed bridge's providers:\n%s", page)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("help add did not run the installed cliproxyapi (%s not created): %v", marker, err)
	}

	// Now the silence means something: neither overview may recreate it.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	for _, topic := range []string{"", "all"} {
		page := helpPage(t, topic, 0)
		if strings.Contains(page, "cliproxyapi") || strings.Contains(page, "Antigravity") {
			t.Errorf("help %q carries the installed bridge's provider list:\n%s", topic, page)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("help %q ran the cliproxyapi on PATH", topic)
		}
	}
}

// overviewSynopsis is the Usage line a topic's own page prints at width 0.
func overviewSynopsis(t *testing.T, topic string) string {
	t.Helper()
	line := strings.SplitN(helpPage(t, topic, 0), "\n", 2)[0]
	usage, ok := strings.CutPrefix(line, "Usage: ")
	if !ok || !strings.HasPrefix(usage, "ccmorph ") {
		t.Fatalf("help %q does not open with a Usage line: %q", topic, line)
	}
	return usage
}

// gridRowLine reports whether line is the row grid prints for exactly this
// label. grid pads the label to its 32-column field and separates it from the
// description with one space, so a row's label is followed by 33-len(label)
// spaces before anything else; a label longer than the field stands alone on
// its row, description under the column. Requiring that gap — not just any
// space — is what stops a shorter label from being witnessed inside a longer
// one: "ccmorph list" must not be found in a row whose label became
// "ccmorph list [--json]" (GH-1034, review).
func gridRowLine(line, label string) bool {
	if len(label) > 32 {
		return line == "  "+label
	}
	return strings.HasPrefix(line, "  "+label+strings.Repeat(" ", 33-len(label)))
}

// overviewHasRow reports whether page carries label as a whole row label: the
// label alone on its row (its description sits under the column) or the label
// followed by nothing but grid spacing and the description.
func overviewHasRow(page, label string) bool {
	for _, line := range strings.Split(page, "\n") {
		if gridRowLine(line, label) {
			return true
		}
	}
	return false
}

// overviewRows is the overview's command rows: every line opening with the
// grid's two-space label column.
func overviewRows(page string) []string {
	var rows []string
	for _, line := range strings.Split(page, "\n") {
		if strings.HasPrefix(line, "  ccmorph ") {
			rows = append(rows, line)
		}
	}
	return rows
}

// rowVerbs is the sorted set of verbs the overview rows teach.
func rowVerbs(page string) []string {
	seen := map[string]bool{}
	var verbs []string
	for _, line := range overviewRows(page) {
		fields := strings.Fields(line)
		if len(fields) < 2 || seen[fields[1]] {
			continue
		}
		seen[fields[1]] = true
		verbs = append(verbs, fields[1])
	}
	sort.Strings(verbs)
	return verbs
}

// overviewRowText is one row's label and full description as one
// whitespace-normalized string: a description that sits under a too-long
// label, or wraps into continuation lines at the column the grid reserves
// (2 + 32 + 1), reads back as the sentence the row teaches.
func overviewRowText(t *testing.T, page, label string, width int) string {
	t.Helper()
	lines := strings.Split(page, "\n")
	for i, line := range lines {
		if !gridRowLine(line, label) {
			continue
		}
		text := line
		for j := i + 1; j < len(lines) && strings.HasPrefix(lines[j], strings.Repeat(" ", 2+32+1)); j++ {
			text += " " + lines[j]
		}
		return strings.Join(strings.Fields(text), " ")
	}
	t.Fatalf("no %q row on the %d-column overview:\n%s", label, width, page)
	return ""
}
