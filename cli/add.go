package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// add is `ccmorph add [name]`: a sign-in method, its credential, and a saved
// profile, rolled back whole on any failure.
func (l *launcher) add(args []string) int {
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
	if present(filepath.Join(l.profileDirs, name)) || present(filepath.Join(l.dir, "auth", name)) {
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
	choice, ok := l.choose("Sign-in method — "+name, rows)
	if !ok {
		return 1
	}
	if !haveBridge && choice == "4" {
		choice = "more"
	}
	var rc int
	switch choice {
	case "1", "sub", "subscription":
		rc = l.addSub(name)
	case "2", "token", "oauth_token", "oauth-token":
		rc = l.addToken(name)
	case "3", "apikey", "api_key", "api-key":
		rc = l.addAPIKey(name)
	case "more":
		fmt.Println("Browser sign-in to " + browserLogins + " needs cliproxyapi.")
		fmt.Println("Install it: " + util.ClipInstallStep() + ", then run: ccmorph add " + name)
		return 1
	default:
		matched := ""
		if n, err := strconv.Atoi(choice); err == nil && n >= 4 && n < 4+len(providers) {
			matched = providers[n-4]
		} else {
			for _, p := range providers {
				if strings.EqualFold(choice, p) {
					matched = p
				}
			}
		}
		if matched == "" && strings.HasPrefix(choice, "sub") {
			rc = l.addSub(name)
			break
		}
		if matched == "" {
			l.err("cancelled")
			return 1
		}
		rc = l.addProvider(name, matched)
	}
	if rc != 0 {
		return rc
	}
	if active := filepath.Join(l.dir, "active"); !present(active) {
		if atomicWrite(active, name+"\n") == nil {
			fmt.Println(l.dim + "Default profile: " + name + " (first profile)" + l.reset)
		}
	}
	fmt.Println("Next: ccmorph run " + name)
	return 0
}

// choose is a native menu on a terminal; piped input picks by number or ID.
func (l *launcher) choose(title, rows string) (string, bool) {
	if isTTY(0) {
		v, rc := uiSelect("native", title, rows, "")
		return v, rc == 0
	}
	themeFor(2).list(title, splitRows(rows), "labels")
	v, rc := uiInput("> ", false)
	return v, rc == 0
}

// rollback removes a half-made profile: private state and conf.
func (l *launcher) rollback(name string) {
	l.removeProfileDir(name)
	os.Remove(l.conf(name))
}

func (l *launcher) claudeAuth(dir string, args ...string) *exec.Cmd {
	return l.claudeAuthCtx(context.Background(), dir, args...)
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

// subBrowserLogin runs Claude Code's browser sign-in against the profile's
// config dir and returns the signed-in email. add and login both put a Claude
// account profile through it; the readback is what names the profile.
func (l *launcher) subBrowserLogin(name string) (string, bool) {
	dir := filepath.Join(l.profileDirs, name)
	fmt.Println(l.dim + "Opening your browser to sign in — Ctrl-C to cancel." + l.reset)
	login := l.claudeAuth(dir, "login")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	if login.Run() != nil {
		l.err("login failed")
		return "", false
	}
	out, err := l.claudeAuth(dir, "status").CombinedOutput()
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

func (l *launcher) addSub(name string) int {
	if atomicWrite(l.conf(name), "type=sub\n") != nil {
		l.err("could not save profile '" + name + "'")
		return 1
	}
	if !l.setupDir(name, "sub", liveNote) {
		l.rollback(name)
		return 1
	}
	dir := filepath.Join(l.profileDirs, name)
	if l.claudeAuth(dir, "logout").Run() != nil {
		l.err("logout failed")
		l.rollback(name)
		return 1
	}
	email, ok := l.subBrowserLogin(name)
	if !ok {
		l.rollback(name)
		return 1
	}
	if atomicWrite(l.conf(name), "type=sub\nemail="+email+"\n") != nil {
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
			return atomicWrite(l.conf(name), strings.Join(lines, "\n"))
		}
	}
	return nil
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
		if atomicWrite(l.conf(name), "type=token\ntoken="+token+"\n") != nil {
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
	var pick string
	if isTTY(0) {
		v, rc := uiSelect("search", "Provider — "+name, strings.Join(rows, "\n"), "")
		if rc != 0 {
			return 1
		}
		pick = v
	} else {
		themeFor(2).list("Provider — "+name, rows, "labels")
		v, rc := uiInput("> ", false)
		if rc != 0 {
			return 1
		}
		pick = v
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(rows) {
			pick = rowID(rows[n-1])
		}
	}
	label, url := "", ""
	var tiers []string
	switch pick {
	case "anthropic":
		label = "Anthropic"
	case "custom":
	default:
		found := false
		for _, p := range presets {
			if p[0] == pick {
				found = true
				p = append(p, "", "", "", "", "")
				label, url, tiers = p[1], p[2], p[3:6]
			}
		}
		if !found {
			l.err("unknown provider '" + pick + "'")
			return 1
		}
	}
	prompt := "API key for " + name
	if label != "" {
		prompt += " — " + label
	} else {
		// Custom endpoint names its address before the key, so the key prompt
		// can name the service the key is for (#278). The dash, not brackets:
		// labels like "Qwen (Alibaba Cloud, China)" bring their own (#361).
		v, rc := uiInput("Endpoint (blank = api.anthropic.com): ", false)
		if rc != 0 {
			return 1
		}
		url = cleanCredential(v)
		service := url
		if service == "" {
			service = "api.anthropic.com"
		}
		prompt += " — " + service
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
	if len(tiers) == 3 && tiers[0] != "" {
		conf += "env.ANTHROPIC_DEFAULT_OPUS_MODEL=" + tiers[0] + "\nenv.ANTHROPIC_DEFAULT_SONNET_MODEL=" + tiers[1] +
			"\nenv.ANTHROPIC_DEFAULT_HAIKU_MODEL=" + tiers[2] + "\n"
	}
	if atomicWrite(l.conf(name), conf) != nil {
		l.err("could not save profile '" + name + "'")
		return 1
	}
	if _, ok := l.read(name); !ok || !l.setupDir(name, "apikey", liveNote) {
		l.rollback(name)
		return 1
	}
	shown := url
	if shown == "" {
		shown = "api.anthropic.com"
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (API key → " + shown + ")")
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
	if err := util.ApproveKey(filepath.Join(l.profileDirs, name), key); err != nil {
		fmt.Println(l.dim + keyQuestionHint + l.reset)
	}
	return 0
}

// checkKey asks the endpoint the listing question doctor asks — the only
// request that can vouch for a key before a session spends it — and reports
// the answer in the add's own step. A 2xx accepts; 401/403 refuses and returns
// false so the key is asked for again; anything else is not a verdict on the
// key, so it is said as not-checked and the add continues. The key reaches curl
// through its stdin config, never a command line.
func (l *launcher) checkKey(url, key string) bool {
	base, native := nativeBase(url)
	shown := url
	if url == "" {
		// The defaulted endpoint is shown as its host, the way the add's own
		// endpoint question spells it.
		shown = "api.anthropic.com"
	}
	var headers []string
	if native {
		headers = []string{"anthropic-version: 2023-06-01"}
	}
	_, code := util.ProbeListing(base, key, headers...)
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
	if !l.providerLogin(name, provider) {
		os.RemoveAll(filepath.Join(l.dir, "auth", name))
		return 1
	}
	if atomicWrite(l.conf(name), "type=provider\nprovider="+provider+"\n") != nil {
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
	out, _ := exec.Command(bin, "--help").CombinedOutput()
	var list []string
	for _, line := range strings.Split(string(out), "\n") {
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
	if isLink(root) {
		l.err("auth directory root is a symlink, refusing: " + root)
		return false
	}
	if isLink(auth) {
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
	fmt.Println(l.dim + "Opening your browser to sign in to " + l.bold + providerLabel(provider) + l.reset + l.dim + " — Ctrl-C to cancel." + l.reset)
	tmp, err := os.MkdirTemp(tmpDir(), fmt.Sprintf("ccmorph-login-%d-", os.Getuid()))
	if err != nil {
		return false
	}
	cred := ""
	defer func() {
		os.RemoveAll(tmp)
		if cred == "" {
			os.Remove(auth) // only when empty: a real credential is never touched
		}
	}()
	conf := filepath.Join(tmp, "config.yaml")
	yaml := "host: \"127.0.0.1\"\nport: 0\nauth-dir: \"" + auth + "\"\napi-keys:\n  - \"unused\"\n"
	if atomicWrite(conf, yaml) != nil {
		return false
	}
	cmd := exec.Command(bin, "-"+provider+"-login", "-config", conf)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if cmd.Run() != nil {
		l.err(providerLabel(provider) + " login failed")
		return false
	}
	for _, pat := range []string{"*.json", "*/*.json"} {
		if m, _ := filepath.Glob(filepath.Join(auth, pat)); len(m) > 0 && cred == "" {
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
		m, _ := filepath.Glob(filepath.Join(auth, pat))
		for _, f := range m {
			os.Chmod(f, 0o600)
		}
	}
	return true
}

// login is `ccmorph login [profile]`: sign a profile in again — a Claude
// account profile with its browser login, a provider-backed profile with its
// own, or an API-key profile by letting Claude Code use the key it refused.
func (l *launcher) login(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" || interactive(name) {
		picked, ok := l.menuProfile("sub", "provider", "apikey")
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		return l.badName("login", name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		return 1
	}
	if f["type"] == "sub" {
		// The sign-in rewrites the conf's email line, so it takes the profile
		// context lock config takes for the same write.
		if rc, done := l.locked(name, []string{"login", name}); done {
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
		if err := util.ApproveKey(filepath.Join(l.profileDirs, name), f["api_key"]); err != nil {
			l.err("could not record the key approval for '" + name + "': " + err.Error())
			return 1
		}
		fmt.Println("Claude Code will use the API key of '" + name + "' again. Start it: ccmorph run " + name)
		return 0
	}
	if f["type"] != "provider" {
		// Only a token profile is left, and its credential is replaced, not
		// renewed: name the command that does that.
		l.err("'" + name + "' is a " + profile.KindLabel(f["type"], f["provider"]) + " profile — replace its credential: ccmorph config " + name)
		return 1
	}
	if !l.providerLogin(name, f["provider"]) {
		return 1
	}
	return 0
}
