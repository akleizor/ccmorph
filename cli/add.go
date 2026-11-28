package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// add is `ccmorph add [name]`: a sign-in method, its credential, and a saved
// profile, rolled back whole on any failure.
func (l *launcher) add(args []string) int {
	defer l.keepCtrlC()() // Ctrl-C cancels every prompt and wait this wizard advertises (GH-1028)
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" && isTTY(0) {
		v, rc := uiInput("New profile name: ", false)
		if rc != 0 {
			return 1
		}
		name = v
	}
	if name == "" {
		l.err("usage: " + usageLine("add"))
		return 1
	}
	if !profile.ValidName(name) {
		return l.badName("add", name)
	}
	if l.exists(name) {
		// A broken conf reads as no type here; existsMsg's default still answers
		// with config, and readQuiet keeps its own refusal off the screen.
		f, _ := l.readQuiet(name)
		for _, line := range existsMsg(name, f["type"]) {
			l.err(line)
		}
		return 1
	}
	if twin := l.caseTwin(name); twin != "" {
		l.err(caseClashMsg(name, twin))
		return 1
	}
	if paths.Lexists(filepath.Join(l.profileDirs, name)) || paths.Lexists(filepath.Join(l.dir, "auth", name)) {
		l.err("leftover private state for '" + name + "'; run ccmorph rm " + name + " to review and remove it, or choose another name")
		return 1
	}
	providers, haveBridge := addProviders()
	rows := "sub\tClaude account\tSign in with your browser\ntoken\tClaude token\tFor scripts/CI; quota cannot read plan usage\napikey\tAPI key\tAnthropic or a gateway"
	for _, p := range providers {
		rows += "\n" + p + "\t" + providerLabel(p) + "\t" + providerDetail(p)
	}
	if !haveBridge {
		// Issue #283: a person without cliproxyapi still learns the browser
		// logins exist and the prerequisite that turns them on; where the row
		// must cut, it ends on a whole word (#369), and the answer below
		// names the machine's own install step.
		rows += "\nmore\tMore sign-ins\t" + browserLogins + " (cliproxyapi)"
	}
	choice, ok := l.menu("native", "Sign-in method — "+name, rows, func(v string) (string, string, bool) {
		if _, _, ok := methodAction(v, providers, haveBridge); !ok {
			return "", "not a sign-in method — enter a number from the list or a name", false
		}
		if !haveBridge && v == "4" {
			v = "more"
		}
		return v, "", true
	})
	if !ok {
		return 1
	}
	kind, provider, _ := methodAction(choice, providers, haveBridge) // the menu guaranteed an answer
	var rc int
	switch kind {
	case "sub":
		rc = l.addSub(name)
	case "token":
		rc = l.addToken(name)
	case "apikey":
		rc = l.addAPIKey(name)
	case "more":
		fmt.Println("Browser sign-in to " + browserLogins + " needs cliproxyapi.")
		fmt.Println("Install it: " + util.ClipInstallStep() + ", then run: ccmorph add " + name)
		return 1
	default:
		rc = l.addProvider(name, provider)
	}
	if rc != 0 {
		return rc
	}
	if active := filepath.Join(l.dir, "active"); !paths.Lexists(active) {
		if atomicWrite(active, name+"\n") == nil {
			fmt.Println(l.dim + "Default profile: " + name + " (first profile)" + l.reset)
		}
	}
	fmt.Println("Next: ccmorph run " + name)
	return 0
}

// menuTryLimit is how many wrong answers a piped menu re-asks for before it
// gives up as cancelled: enough to recover from one answer typed from instinct,
// few enough that a stream of wrong lines still ends the wizard.
const menuTryLimit = 3

// menu is a wizard menu: a native picker on a terminal, which cannot be
// answered wrongly; on a pipe, the list and a loop (#727). Before, the piped
// menu read one line and shared its exit with cancel, so the issue's typed
// gateway URL ended the whole wizard. Now an answer that matches no row re-asks
// — naming the limit it states — so every answer already given stands; the
// deliberate ways out stay: EOF, and q, the word the numbered menu names.
func (l *launcher) menu(mode, title, rows string, match func(string) (string, string, bool)) (string, bool) {
	if isTTY(0) {
		v, rc := uiSelect(mode, title, rows, "")
		return v, rc == 0
	}
	themeFor(2).list(title, splitRows(rows), "labels")
	for wrong := 1; ; wrong++ {
		v, rc := uiInput("> ", false)
		if rc != 0 {
			return "", false
		}
		if v == "q" || v == "Q" {
			l.err("cancelled")
			return "", false
		}
		answer, hint, ok := match(v)
		if ok {
			return answer, true
		}
		if wrong >= menuTryLimit {
			l.err(fmt.Sprintf("too many wrong answers at the menu (%d) — cancelled", menuTryLimit))
			return "", false
		}
		if hint == "" {
			hint = "not a menu answer — enter a number from the list or a name"
		}
		l.err(fmt.Sprintf("%s (try %d of %d)", hint, wrong, menuTryLimit))
	}
}

// methodAction is what a sign-in menu answer chooses: the row's kind and, for
// a browser login, its provider. The old inline switch's order is kept whole —
// the fixed rows answer first, then a provider number, then a provider name,
// then a "sub…" prefix — so a name a provider could also answer with keeps
// resolving to the row it always did. Only the rows the menu prints are
// answers: the More sign-ins row exists without the bridge alone, so `more`
// with the bridge on PATH re-asks like any unlisted word (#727).
func methodAction(choice string, providers []string, haveBridge bool) (kind, provider string, ok bool) {
	if !haveBridge && choice == "4" {
		choice = "more"
	}
	switch choice {
	case "1", "sub", "subscription":
		return "sub", "", true
	case "2", "token", "oauth_token", "oauth-token":
		return "token", "", true
	case "3", "apikey", "api_key", "api-key":
		return "apikey", "", true
	case "more":
		if haveBridge {
			return "", "", false
		}
		return "more", "", true
	}
	if n, err := strconv.Atoi(choice); err == nil && n >= 4 && n < 4+len(providers) {
		return "provider", providers[n-4], true
	}
	for _, p := range providers {
		if strings.EqualFold(choice, p) {
			return "provider", p, true
		}
	}
	if strings.HasPrefix(choice, "sub") {
		return "sub", "", true
	}
	return "", "", false
}

// urlHostShape is an address typed without its scheme: a dotted host, an
// optional port and path — the way a gateway reads on a first read of the
// question (#727). A preset id has no dot, so nothing a row answers with
// carries this shape.
var urlHostShape = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+(:[0-9]{1,5})?(/\S*)?$`)

// looksLikeURL reports an answer shaped like an endpoint a person meant to
// give: it carries a scheme, or it reads as a dotted host (#727).
func looksLikeURL(s string) bool {
	return strings.Contains(s, "://") || urlHostShape.MatchString(s)
}

// rollback removes a half-made profile: private state, its live lock and conf.
// The lock is the profile's own cover (GH-185): a profile that is being taken
// back owns no session, and a lock file left behind is state no verb shows
// (GH-837). A lock another launch holds survives the rollback.
func (l *launcher) rollback(name string) {
	l.removeProfileDir(name)
	_ = util.RemoveLive(l.profileDirs, name)
	os.Remove(l.conf(name))
	delete(l.confReads, name) // what was half-made must not be read back (GH-561)
}

func (l *launcher) claudeAuth(dir string, args ...string) *exec.Cmd {
	return l.claudeAuthCtx(context.Background(), dir, args...)
}

// claudeInstallStep is the one command that installs Claude Code where none
// is: the native installer the setup page the refusal sentence links opens
// with as its first step, needing no package manager of its own.
const claudeInstallStep = "Install Claude Code with: curl -fsSL https://claude.ai/install.sh | bash"

// requireClaude is the look the account sign-in does before it starts (#789):
// every step it takes execs `claude`, so on a machine without one each step
// answered only "logout failed" or "login failed" — naming neither the
// missing program nor the fix. The refusal is the one sentence run and doctor
// already give for a missing claude, and recovery names the install step a
// person can run from it, or the sign-in method that needs no claude at all:
// a first-time user meets a blocked default with a way on, never one line
// that names nothing. The methods that exec no claude (token, apikey) are
// untouched: it guards the account flow, which is all it can fail.
func (l *launcher) requireClaude(recovery string) bool {
	if _, err := exec.LookPath("claude"); err != nil {
		l.err(util.ClaudeMissing)
		l.err(recovery)
		return false
	}
	return true
}

// claudeAuthCtx is claudeAuth bound to ctx, for the one caller that must not
// wait forever (doctor's sign-in check); a browser login keeps no deadline.
func (l *launcher) claudeAuthCtx(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "claude", append([]string{"auth"}, args...)...)
	env := os.Environ()
	for _, k := range append(append([]string{}, l.backend...), "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		env = envDrop(env, k)
	}
	cmd.Env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	return cmd
}

var emailRE = regexp.MustCompile(`"email": "([^"]*)"`)

// claudeStatusRE reads `claude auth status`'s JSON: whether Claude Code holds
// a sign-in for the config dir it was pointed at.
var claudeStatusRE = regexp.MustCompile(`"loggedIn":\s*(true|false)`)

// handSignInSignals keeps SIGHUP, SIGTERM and SIGINT from killing this process
// while a browser sign-in waits (GH-649, GH-837). Closing a terminal or
// dropping SSH sends HUP and TERM to the whole foreground process group, and
// Ctrl-C sends INT; a parent that dies mid-wait never reaches the rollback that
// makes an interrupted sign-in clean up the way a cancelled one does. While the
// returned release is pending, those signals go on to the sign-in child — the
// child owns the wait and a terminal sends it the signals too — so the child
// exits, the caller's failure path runs, and Ctrl-C cancels setup as the help
// promises instead of leaving a half-made profile and its lock behind (GH-837).
func handSignInSignals(child *os.Process) (release func()) {
	release, _ = handSignInSignalsWait(child)
	return release
}

// handSignInSignalsWait is handSignInSignals with the record of whether a
// signal was forwarded: a child may catch it and exit 0, and that normal exit
// is still the user cancelling, never permission to go on and save (GH-1028).
func handSignInSignalsWait(child *os.Process) (release func(), interrupted *atomic.Bool) {
	handoffs.Add(1) // the command-wide Ctrl-C owner stands down while this holds (GH-1028)
	interrupted = &atomic.Bool{}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sigs {
			if sig, ok := s.(syscall.Signal); ok {
				interrupted.Store(true)
				_ = child.Signal(sig) // the child may already be gone
			}
		}
	}()
	return func() {
		handoffs.Add(-1)
		signal.Stop(sigs) // returns once no more signals can arrive
		close(sigs)
	}, interrupted
}

// runHanded runs cmd with the sign-in handoff active: while it waits, SIGHUP,
// SIGTERM and SIGINT belong to the child, which dies of the signal and lets the
// caller's failure path run (GH-649, GH-837) — for every child of the add and
// login flows alike, so Ctrl-C cancels each wait these commands advertise
// (GH-1028).
func runHanded(cmd *exec.Cmd) error {
	return runHandedThen(cmd, nil)
}

// runHandedThen runs cmd with the handoff active, and calls ready once the
// handoff holds the signals. A line that promises "Ctrl-C to cancel" is printed
// from ready: from the instant it is readable the signal belongs to the child,
// so the advertised escape never lands in the unprotected start window
// (GH-1028).
func runHandedThen(cmd *exec.Cmd, ready func()) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Between Start and the handoff there is a sliver in which the child exists
	// but no forwarding does; the command-wide cancel must still stop it, or a
	// signal at that instant would leave a sign-in child behind (GH-1028).
	releaseChild := util.TrackChild(cmd.Process)
	defer releaseChild()
	release, interrupted := handSignInSignalsWait(cmd.Process)
	defer release()
	if ready != nil {
		ready()
	}
	err := cmd.Wait()
	if err == nil && interrupted.Load() {
		// The child answered the signal by exiting normally. The user still
		// cancelled; the caller's failure path, not its success path, runs.
		return errSignInInterrupted
	}
	return err
}

// errSignInInterrupted is a wait the user cancelled even though the child
// exited cleanly; callers only need "did not complete".
var errSignInInterrupted = errors.New("sign-in interrupted")

// runHandedRC is runHanded's exit status: a lock-wrapped verb is one process
// between the command and the sign-in it runs, and Ctrl-C must reach it so it
// releases the lock and stops its own child (GH-1028).
func runHandedRC(cmd *exec.Cmd) int {
	if err := runHanded(cmd); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		return 127
	}
	return 0
}

// runHandedOutput is runHanded for a child whose output the caller reads: the
// auth status readback blocks like any sign-in wait and must answer Ctrl-C the
// same way (GH-1028).
func runHandedOutput(cmd *exec.Cmd) ([]byte, error) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := runHanded(cmd)
	return buf.Bytes(), err
}

// subBrowserLogin runs Claude Code's browser sign-in against the profile's
// config dir and returns the signed-in email. add and login both put a Claude
// account profile through it; the readback is what names the profile.
func (l *launcher) subBrowserLogin(name string) (string, bool) {
	dir := filepath.Join(l.profileDirs, name)
	login := l.claudeAuth(dir, "login")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := runHandedThen(login, func() {
		fmt.Println(l.dim + "Opening your browser to sign in — Ctrl-C to cancel." + l.reset)
	})
	if err != nil {
		l.err("login failed")
		return "", false
	}
	out, err := runHandedOutput(l.claudeAuth(dir, "status"))
	if err != nil {
		l.err("could not read authentication status")
		return "", false
	}
	email := ""
	if m := emailRE.FindSubmatch(out); m != nil {
		email = string(m[1])
	}
	if email == "" || strings.ContainsAny(email, "\r\n") {
		l.err("no Claude account profile found after login — nothing was saved")
		return "", false
	}
	return email, true
}

// addSub signs in before it saves (GH-649): the conf appears only with its
// email, the way addProvider signs in before writing type=provider. An early
// `type=sub` stub made every abnormal exit — a closed terminal, a dropped SSH —
// a self-contradictory wall: list read the stub as a broken profile and told
// the user to re-create it while add called the same name taken and pointed at
// login. A profile that was never signed in is not a profile, and no cleanup
// can promise what a save that comes last simply never owes.
func (l *launcher) addSub(name string) int {
	if !l.requireClaude(claudeInstallStep +
		" — or pick another sign-in method: 2) Claude token, 3) API key") {
		// Before setupDir: a machine with no claude can complete no part of
		// this flow, so it leaves nothing that needs the rollback below.
		return 1
	}
	if !l.setupDir(name, "sub", liveNote) {
		l.rollback(name)
		return 1
	}
	// From the first private file to the conf that makes the profile real, a
	// signal that ends the command must take the half-made profile with it
	// (GH-1028). The normal exits below already roll back; this covers the
	// window where the command-wide owner exits before they run.
	release := util.OnCancel(func() { l.rollback(name) })
	defer release()
	dir := filepath.Join(l.profileDirs, name)
	if runHanded(l.claudeAuth(dir, "logout")) != nil {
		l.err("logout failed")
		l.rollback(name)
		return 1
	}
	email, ok := l.subBrowserLogin(name)
	if !ok {
		l.rollback(name)
		return 1
	}
	if l.writeConf(name, "type=sub\nemail="+email+"\n") != nil {
		l.err("could not save profile '" + name + "'")
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (Claude account: " + email + ")")
	return 0
}

// cleanCredential strips what a paste picks up: whitespace around the value,
// one layer of matching quotes, then whitespace again. A typed credential has
// none of these and comes back unchanged.
func cleanCredential(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[0] == s[len(s)-1] {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// credentialSpace reports whitespace that cleaning cannot remove: it sits
// inside the credential, and a credential is one piece.
func credentialSpace(s string) bool {
	return strings.IndexFunc(s, unicode.IsSpace) >= 0
}

// askCredential reads one credential at prompt, cleaning paste artifacts and
// refusing what cannot be cleaned at the prompt itself — never after later
// questions. An empty answer or EOF cancels the whole add.
func (l *launcher) askCredential(prompt, what string) (string, bool) {
	for {
		v, rc := uiInput(prompt, true)
		if rc != 0 {
			return "", false
		}
		if v = cleanCredential(v); v == "" {
			l.err("empty " + what)
			return "", false
		}
		if credentialSpace(v) {
			l.err(what + " contains whitespace — copy it again as one piece")
			continue
		}
		return v, true
	}
}

// subLogin is `ccmorph login` on a Claude account profile: the browser sign-in
// add performs, run on the profile's existing home, so a signed-out profile
// signs in again without being re-created. Only the email line changes; the
// sign-in itself lives in the profile home, where Claude Code keeps it.
func (l *launcher) subLogin(name string, f map[string]string) int {
	if !l.requireClaude(claudeInstallStep + ", then run: ccmorph login " + name) {
		return 1
	}
	if !l.setupDir(name, "sub", liveSilent) {
		return 1
	}
	email, ok := l.subBrowserLogin(name)
	if !ok {
		return 1
	}
	if f["email"] != email {
		if err := l.setEmail(name, email); err != nil {
			l.err("could not save profile '" + name + "'")
			return 1
		}
	}
	fmt.Println("Signed in to Claude as " + email + ". Start it: ccmorph run " + name)
	return 0
}

// setEmail rewrites the profile conf's email line, leaving every other line as
// it is, so a renewed sign-in does not discard what config wrote around it.
func (l *launcher) setEmail(name, email string) error {
	raw, err := os.ReadFile(l.conf(name))
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "email=") {
			lines[i] = "email=" + email
			return l.writeConf(name, strings.Join(lines, "\n"))
		}
	}
	// GH-649: the line can be absent, not just stale — that is what an
	// interrupted sign-in used to leave behind, and silently reporting success
	// over a conf it never repaired is the lie this verb may not tell. What a
	// completed sign-in knows and the conf lacks, the conf gets.
	return l.writeConf(name, strings.TrimSuffix(string(raw), "\n")+"\nemail="+email+"\n")
}

func (l *launcher) addToken(name string) int {
	fmt.Println(l.dim + "Make one with claude setup-token; a valid one starts with sk-ant-oat01-." + l.reset)
	// A Claude token has one shape, so a paste without it is refused at the
	// prompt. A shaped one is offered to Anthropic here — Bearer-only, like the
	// launch that will spend it — and only Anthropic can judge it: a refusal is
	// reported and the token asked for again before anything is saved; no
	// answer is not a verdict, so it is said as not-checked and the add saves.
	for {
		token, ok := l.askCredential("Paste token for "+name+" (hidden): ", "token")
		if !ok {
			return 1
		}
		if !util.TokenShaped(token) {
			l.err("that is not a Claude token's shape — copy the whole token again")
			continue
		}
		switch code := util.ProbeToken(token); probeVerdict(code) {
		case probeAccepted:
			fmt.Println(l.dim + "Anthropic accepted the token (HTTP " + code + ")" + l.reset)
		case probeRejected:
			l.err("Anthropic rejected the token (HTTP " + code + ") — paste it again")
			continue
		case probeSilent:
			fmt.Println(l.dim + "Anthropic did not answer, so the token was not checked" + l.reset)
		default:
			fmt.Println(l.dim + "Anthropic answered HTTP " + code + ", so the token was not checked" + l.reset)
		}
		if l.writeConf(name, "type=token\ntoken="+token+"\n") != nil {
			l.err("could not save profile '" + name + "'")
			return 1
		}
		if _, ok := l.readRaw(name); !ok || !l.setupDir(name, "token", liveNote) {
			l.rollback(name)
			return 1
		}
		fmt.Println(l.accent + "Added" + l.reset + " " + name + " (Claude token)")
		return 0
	}
}

// presets reads share/provider-presets.tsv: id, label, base URL, three tier models.
func (l *launcher) presets() [][]string {
	f, err := os.Open(filepath.Join(filepath.Dir(l.libDir), "share", "provider-presets.tsv"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, strings.Split(line, "\t"))
		}
	}
	return out
}

// tierColumns maps a preset's three tier columns to the env lines the wizard
// writes, each key built from its word in util.TierWords. The words are the
// ones after fable: no gateway column maps fable, which is left to `ccmorph
// config set`, not given a fourth column here. add_test.go pins the three
// columns to opus, sonnet and haiku by name, so reordering TierWords cannot
// silently point a column at another tier (#449).
func tierColumns(tiers []string) []string {
	var lines []string
	for i, tier := range util.TierWords[1:] {
		if i >= len(tiers) {
			break
		}
		lines = append(lines, "env."+util.TierEnvKey(tier)+"="+tiers[i])
	}
	return lines
}

func (l *launcher) addAPIKey(name string) int {
	presets := l.presets()
	// Anthropic's own API leads the list: the sign-in menu promises "Anthropic or
	// a gateway", and a blank base_url is Claude Code's default endpoint, so the
	// row needs no address and no tier mapping (#278).
	rows := []string{"anthropic\tAnthropic\tapi.anthropic.com"}
	for _, p := range presets {
		if len(p) >= 3 {
			rows = append(rows, p[0]+"\t"+p[1]+"\t"+strings.TrimPrefix(p[2], "https://"))
		}
	}
	rows = append(rows, "custom\tCustom endpoint\tAnthropic or any gateway URL")
	pick, ok := l.menu("search", "Provider — "+name, strings.Join(rows, "\n"), func(v string) (string, string, bool) {
		p := v
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(rows) {
			p = rowID(rows[n-1])
		}
		if p == "anthropic" || p == "custom" {
			return p, "", true
		}
		for _, pr := range presets {
			if pr[0] == p {
				return p, "", true
			}
		}
		hint := "not a provider — enter a number from the list or a provider id"
		if looksLikeURL(v) {
			// The question's own wording invites the URL; the row that takes
			// one is pointed at by the number it is printed under (#727).
			hint = fmt.Sprintf("unknown provider '%s' — for your own URL pick %d) Custom endpoint", v, len(rows))
		}
		return "", hint, false
	})
	if !ok {
		return 1
	}
	label, url := "", ""
	var tiers []string
	switch pick {
	case "anthropic":
		label = "Anthropic"
	case "custom":
	default:
		// Reached with an id the list carries: the terminal picker answers with
		// row IDs, and the pipe's loop only returns a matched one (#727).
		for _, p := range presets {
			if p[0] == pick {
				p = append(p, "", "", "", "", "")
				label, url, tiers = p[1], p[2], p[3:6]
			}
		}
	}
	prompt := "API key for " + name
	if label != "" {
		prompt += " — " + label
	} else {
		// Custom endpoint names its address before the key, so the key prompt
		// can name the service the key is for (#278). The dash, not brackets:
		// labels like "Qwen (Alibaba Cloud, China)" bring their own (#361).
		// The answer's receipt and the header below both name the service
		// through the same masking rule every endpoint line uses (GH-1038):
		// what was just typed folds away with its login hidden, while the
		// value saved stays exactly what was typed; a blank answer reads
		// as the default.
		v, rc := uiInputShown("Endpoint (blank = api.anthropic.com): ", false, shownEndpoint)
		if rc != 0 {
			return 1
		}
		url = cleanCredential(v)
		prompt += " — " + shownEndpoint(url)
	}
	// The endpoint is asked the listing question doctor asks, in this step: a
	// key it refuses is named here and asked for again, not saved as Added and
	// discovered at launch. The endpoint was answered once, before the key; a
	// re-entered key is checked against the same endpoint.
	var key string
	for {
		var ok bool
		key, ok = l.askCredential(prompt+" (hidden): ", "api key")
		if !ok {
			return 1
		}
		if strings.ContainsAny(key+url, "\r\n") {
			l.err("apikey values cannot contain CR/LF")
			return 1
		}
		if !l.checkKey(url, key) {
			continue
		}
		break
	}
	conf := "type=apikey\napi_key=" + key + "\n"
	if url != "" {
		conf += "base_url=" + url + "\n"
	}
	// The presets' three tier columns are written as the env keys the store
	// reads back through tierColumns. A word the list gains beyond the columns
	// stays unwritten here rather than hand-spelled (#539).
	if len(tiers) == 3 && tiers[0] != "" {
		for _, line := range tierColumns(tiers) {
			conf += line + "\n"
		}
	}
	if l.writeConf(name, conf) != nil {
		l.err("could not save profile '" + name + "'")
		return 1
	}
	if _, ok := l.read(name); !ok || !l.setupDir(name, "apikey", liveNote) {
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (API key → " + shownEndpoint(url) + ")")
	if url != "" {
		l.probeNudge(name)
	}
	// A gateway is asked which model at every launch until a default is saved;
	// the command that ends the question is named here, while the person is
	// still reading (#360). Custom can also name Anthropic's own endpoint,
	// which launches native — nothing asks there — so the line is gated on the
	// same predicate inspect's model row uses, not on a non-empty URL alone.
	if util.Maskable("apikey", url) {
		fmt.Println(l.dim + "Each launch asks which model until one is saved:  ccmorph set " + name + " model <model-id>" + l.reset)
	}
	// Claude Code asks whether to use a profile's key on its first launch, and
	// highlights No. The answer it would store is recorded here instead, so the
	// question never appears. A file ccmorph cannot safely rewrite leaves the
	// question in place, and the person is told which answer to give.
	if err := util.ApproveKey(l.profileDirs, name, key); err != nil {
		if errors.Is(err, util.ErrProfileInUse) && util.SaveApproval(l.profileDirs, name, key) == nil {
			fmt.Println(l.dim + "A session or launch of '" + name + "' is still open, so your answer to Claude Code's API key question is saved; the next launch records it." + l.reset)
		} else {
			fmt.Println(l.dim + keyQuestionHint + l.reset)
		}
	}
	return 0
}

// shownEndpoint is an endpoint as every human-facing line prints it — add's
// own, the profile menu's, list's, inspect's and config's (GH-1038). Without
// @, ? or # there is no login, query or fragment to hide, so show the endpoint
// as stored without changing the host's case or the path's spelling. With any
// of those markers, use RedactURL if there is a scheme and redact the whole
// value otherwise (INTENT promise 5). Blank means the default endpoint.
// Scripted reads (get base_url, get --json) return the stored value instead.
func shownEndpoint(url string) string {
	switch {
	case url == "":
		return "api.anthropic.com"
	case !strings.ContainsAny(url, "@?#"):
		return url
	case strings.Contains(url, "://"):
		return profile.RedactURL(url)
	}
	return "<redacted>"
}

// checkKey asks the endpoint the listing question doctor asks — the only
// request that can vouch for a key before a session spends it — and reports
// the answer in the add's own step. A 2xx accepts; 401 refuses and returns
// false so the key is asked for again. 403, 404 and 405 answer the question
// that was asked — the listing — and not the key: a gateway that restricts
// listing but serves chat answers 403 while taking the same key on chat, and
// one without a listing endpoint answers 404/405; the add says the listing is
// not allowed and continues unchecked (#835) — a 403 on an actual inference
// probe would refuse, but the add makes no inference probe. Anything else is
// no verdict on the key either, so it is said as not-checked and the add
// continues. The endpoint is named through shownEndpoint, never as typed: a
// URL can carry a login or a key in its query, and a note that echoes it back
// is a credential in the output (INTENT promise 5). The key reaches curl
// through its stdin config, never a command line.
func (l *launcher) checkKey(url, key string) bool {
	base, native := nativeBase(url)
	shown := shownEndpoint(url)
	var headers []string
	if native {
		headers = []string{"anthropic-version: 2023-06-01"}
	}
	_, code := util.ProbeListing(base, key, headers...)
	switch code {
	case "403", "404", "405":
		// The listing itself was refused, by rule or by absence: no word
		// here is a verdict on the key, so the save proceeds unchecked.
		fmt.Println(l.dim + shown + ": listing not allowed (HTTP " + code + ") — the key was not checked" + l.reset)
		return true
	}
	switch probeVerdict(code) {
	case probeAccepted:
		fmt.Println(l.dim + shown + " accepted the API key (HTTP " + code + ")" + l.reset)
	case probeRejected:
		l.err(shown + " rejected the API key (HTTP " + code + ") — paste it again")
		return false
	case probeSilent:
		fmt.Println(l.dim + shown + " did not answer, so the API key was not checked" + l.reset)
	default:
		fmt.Println(l.dim + shown + " answered HTTP " + code + ", so the API key was not checked" + l.reset)
	}
	return true
}

func (l *launcher) addProvider(name, provider string) int {
	// A fresh provider add never owns a credential until providerLogin
	// succeeds: anything the sign-in staged is this add's to remove, conf or
	// no conf (GH-1028). The existing-profile path is `login`, which does not
	// come through here.
	auth := filepath.Join(l.dir, "auth", name)
	release := util.OnCancel(func() {
		os.RemoveAll(auth)
		l.rollback(name)
	})
	defer release()
	if !l.providerLogin(name, provider) {
		os.RemoveAll(auth)
		return 1
	}
	if l.writeConf(name, "type=provider\nprovider="+provider+"\n") != nil {
		l.err("could not save profile '" + name + "'")
		os.RemoveAll(filepath.Join(l.dir, "auth", name))
		return 1
	}
	if _, ok := l.readRaw(name); !ok || !l.setupDir(name, "provider", liveNote) {
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (" + providerLabel(provider) + ")")
	fmt.Println(l.dim + "Set a default model:  ccmorph set " + name + " model <model-id>" + l.reset)
	fmt.Println(l.dim + "Configure defaults: ccmorph config " + name + l.reset)
	return 0
}

// addProviders is the provider logins the add menu offers, other than Claude:
// each one needs a provider bridge (cliproxyapi) on PATH. ok is false when the
// bridge is missing, so the menu can offer the step that turns them on (#283).
func addProviders() ([]string, bool) {
	var out []string
	list, ok := providerList()
	if ok {
		for _, p := range list {
			if p != "claude" {
				out = append(out, p)
			}
		}
	}
	return out, ok
}

// browserLogins is what the provider sign-ins sign in to, in menu order. Both
// Kimi services are named — with the names the provider list uses — so the
// list names one login per row, never one Kimi for two (#366).
const browserLogins = "Antigravity, Codex, Devin, Kimi For Coding (global), " +
	"Kimi For Coding (China), Meta and xAI"

// providerList is _ccm_provider_list: the -<name>-login flags cliproxyapi offers.
func providerList() ([]string, bool) {
	bin, err := exec.LookPath("cliproxyapi")
	if err != nil {
		return nil, false
	}
	// The bridge's help is a wait before the first prompt; a command-wide
	// cancel takes it down with the add (GH-1028).
	cmd := exec.Command(bin, "--help")
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	_ = util.RunTracked(cmd)
	var list []string
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.Contains(line, "-login") {
			continue
		}
		line = strings.TrimLeft(line, " \t")
		line = strings.TrimPrefix(line, "-")
		// The leading dash the trim removes can itself be the row's only
		// -login marker (a bridge flag like -login-proxy string, #456): with
		// none left the row is not a provider login, never a slice at -1.
		at := strings.Index(line, "-login")
		if at < 0 {
			continue
		}
		line = line[:at]
		if line != "" && !strings.Contains(line, "device") {
			list = append(list, line)
		}
	}
	return list, true
}

// providerDetail is the sign-in row's second column: the product or plan the
// browser login is for, so two rows with the same label still differ (#283),
// and the bridge the login goes through stays named (#265).
func providerDetail(p string) string {
	switch p {
	case "antigravity":
		return "Browser sign-in to Google Antigravity, through cliproxyapi"
	case "codex":
		return "Browser sign-in to ChatGPT (Codex), through cliproxyapi"
	case "devin":
		return "Browser sign-in to Devin, through cliproxyapi"
	case "kimi-ai":
		return "Browser sign-in to a kimi.ai account, through cliproxyapi"
	case "kimi":
		return "Browser sign-in to a kimi.com account, through cliproxyapi"
	case "meta":
		return "Browser sign-in to Meta AI, through cliproxyapi"
	case "xai":
		return "Browser sign-in to xAI (Grok), through cliproxyapi"
	}
	return "Sign in to " + providerLabel(p) + " with your browser, through cliproxyapi"
}

// providerLogin is _ccm_provider_login: cliproxyapi's browser login into auth/<name>.
func (l *launcher) providerLogin(name, provider string) bool {
	bin, err := exec.LookPath("cliproxyapi")
	if err != nil {
		l.err("cliproxyapi not found — install it: " + util.ClipInstallStep())
		return false
	}
	root := filepath.Join(l.dir, "auth")
	auth := filepath.Join(root, name)
	if paths.IsSymlink(root) {
		l.err("auth directory root is a symlink, refusing: " + root)
		return false
	}
	if paths.IsSymlink(auth) {
		l.err("auth directory for '" + name + "' is a symlink, refusing: " + auth)
		return false
	}
	if os.MkdirAll(root, 0o700) != nil {
		return false
	}
	os.Chmod(root, 0o700)
	if os.MkdirAll(auth, 0o700) != nil || os.Chmod(auth, 0o700) != nil {
		l.err("could not create auth directory")
		return false
	}
	// What the profile already holds is not this attempt's to remove: a
	// cancelled re-login keeps the stored credential and drops only what the
	// attempt staged beside it (GH-1028).
	before := authEntries(auth)
	tmp, err := os.MkdirTemp(tmpDir(), fmt.Sprintf("ccmorph-login-%d-", os.Getuid()))
	if err != nil {
		return false
	}
	cred := ""
	cleanup := func() {
		os.RemoveAll(tmp)
		if cred == "" {
			removeNewAuthEntries(auth, before)
			os.Remove(auth) // only when the attempt left it empty
		}
	}
	defer cleanup()
	// A signal that ends the command during this window has no handoff yet to
	// run the deferred cleanup above (os.Exit skips it); register the same
	// cleanup for the command-wide owner (GH-1028).
	release := util.OnCancel(cleanup)
	defer release()
	conf := filepath.Join(tmp, "config.yaml")
	yaml := "host: \"127.0.0.1\"\nport: 0\nauth-dir: \"" + auth + "\"\napi-keys:\n  - \"unused\"\n"
	if atomicWrite(conf, yaml) != nil {
		return false
	}
	cmd := exec.Command(bin, "-"+provider+"-login", "-config", conf)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// The browser wait hands its signals to the bridge the way the Claude
	// sign-in already does: the notice says "Ctrl-C to cancel", and it is
	// printed only after the handoff holds the signals, so a signal sent the
	// instant it appears reaches the bridge and ends the wait through this
	// failure path — never the parent ahead of the cleanup (GH-1028).
	if runHandedThen(cmd, func() {
		fmt.Println(l.dim + "Opening your browser to sign in to " + l.bold + providerLabel(provider) + l.reset + l.dim + " — Ctrl-C to cancel." + l.reset)
	}) != nil {
		l.err(providerLabel(provider) + " login failed")
		return false
	}
	for _, pat := range []string{"*.json", "*/*.json"} {
		if m, _ := filepath.Glob(filepath.Join(paths.GlobQuote(auth), pat)); len(m) > 0 && cred == "" {
			for _, f := range m {
				if paths.IsFile(f) {
					cred = f
					break
				}
			}
		}
	}
	if cred == "" {
		l.err("no credential appeared after login — check your browser")
		return false
	}
	for _, pat := range []string{"*.json", "*/*.json", "*/*/*.json"} {
		m, _ := filepath.Glob(filepath.Join(paths.GlobQuote(auth), pat))
		for _, f := range m {
			os.Chmod(f, 0o600)
		}
	}
	return true
}

// authEntries is every path under the profile's auth dir before a sign-in
// attempt, relative to it.
func authEntries(dir string) map[string]bool {
	entries := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		if rel, err := filepath.Rel(dir, path); err == nil {
			entries[rel] = true
		}
		return nil
	})
	return entries
}

// removeNewAuthEntries removes what a sign-in attempt added under the auth dir,
// leaving every entry that was there before it untouched.
func removeNewAuthEntries(dir string, before map[string]bool) {
	var added []string
	_ = filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		if rel, err := filepath.Rel(dir, path); err == nil && !before[rel] {
			added = append(added, path)
		}
		return nil
	})
	// Deepest first, so a new directory's children are removed with it once.
	sort.Slice(added, func(i, j int) bool { return len(added[i]) > len(added[j]) })
	for _, path := range added {
		os.RemoveAll(path)
	}
}

// login is `ccmorph login [profile]`: sign a profile in again — a Claude
// account profile with its browser login, a provider-backed profile with its
// own, or an API-key profile by letting Claude Code use the key it refused.
func (l *launcher) login(args []string) int {
	defer l.keepCtrlC()() // the sign-in waits this verb prints all name Ctrl-C (GH-1028)
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if !l.requireProfile(&name, "login", "sub", "provider", "apikey") {
		return 1
	}
	// readRaw's reading, printed exactly as it prints it — with one exception
	// (GH-649): the sub conf missing its email is login's own patient. A
	// sign-in interrupted before that line left a refusal here that pointed
	// away from this command, while `add` pointed at it — two repairs for one
	// conf, and the profile was one sign-in from whole.
	f, err := l.readProfile(name, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if err != nil {
		if f["type"] != "sub" {
			l.err(err.Error())
			return 1
		}
		if rc, done := l.locked(lockKey(name), []string{"login", name}); done {
			return rc
		}
		return l.subLogin(name, f)
	}
	if f["type"] == "sub" {
		// The sign-in rewrites the conf's email line, so it takes the profile
		// context lock config takes for the same write.
		if rc, done := l.locked(lockKey(name), []string{"login", name}); done {
			return rc
		}
		return l.subLogin(name, f)
	}
	if f["type"] == "apikey" {
		// The profile's key is already stored; what was refused is Claude
		// Code's answer to using it. Record the answer again.
		if f["api_key"] == "" {
			l.err("'" + name + "' has no api_key — replace it: ccmorph config " + name)
			return 1
		}
		if !l.setupDir(name, "apikey", liveSilent) {
			return 1
		}
		if err := util.ApproveKey(l.profileDirs, name, f["api_key"]); err != nil {
			if errors.Is(err, util.ErrProfileInUse) {
				// A session or launch of the profile is open and rewrites the
				// file itself; the answer is kept for the next launch to record
				// (#299) instead of racing it.
				if serr := util.SaveApproval(l.profileDirs, name, f["api_key"]); serr != nil {
					l.err("could not save the key approval for '" + name + "' while it is in use: " + serr.Error())
					return 1
				}
				fmt.Println("A session or launch of '" + name + "' is still open, so Claude Code's answer cannot be written into its config now; it is saved, and the next launch records it once it ends. Then start it: ccmorph run " + name)
				return 0
			}
			l.err("could not record the key approval for '" + name + "': " + err.Error())
			return 1
		}
		util.DropApproval(l.profileDirs, name, f["api_key"])
		fmt.Println("Claude Code will use the API key of '" + name + "' again. Start it: ccmorph run " + name)
		return 0
	}
	if f["type"] != "provider" {
		// Only a token profile is left, and its credential is replaced, not
		// renewed: name the command that does that.
		l.err("'" + name + "' is " + profile.KindPhrase(f["type"], f["provider"]) + " profile — replace its credential: ccmorph config " + name)
		return 1
	}
	if !l.providerLogin(name, f["provider"]) {
		return 1
	}
	return 0
}
