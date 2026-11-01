package cli

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/profile"
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
	if !profile.ValidName(name) {
		l.err("invalid name — use alphanumeric/underscore")
		return 1
	}
	if l.exists(name) {
		l.err("'" + name + "' already exists. Remove first: ccmorph rm " + name)
		return 1
	}
	if present(filepath.Join(l.profileDirs, name)) || present(filepath.Join(l.dir, "auth", name)) {
		l.err("leftover private state for '" + name + "'; run ccmorph rm " + name + " to review and remove it, or choose another name")
		return 1
	}
	var providers []string
	if list, ok := providerList(); ok {
		for _, p := range list {
			if p != "claude" {
				providers = append(providers, p)
			}
		}
	}
	rows := "sub\tClaude account\tSign in with your browser\ntoken\tClaude token\tPaste an existing OAuth token\napikey\tAPI key\tAnthropic or a gateway"
	for _, p := range providers {
		rows += "\n" + p + "\t" + providerLabel(p) + "\tSign in with your browser"
	}
	choice, ok := l.choose("Sign-in method — "+name, rows)
	if !ok {
		return 1
	}
	var rc int
	switch choice {
	case "1", "sub", "subscription":
		rc = l.addSub(name)
	case "2", "token", "oauth_token", "oauth-token":
		rc = l.addToken(name)
	case "3", "apikey", "api_key", "api-key":
		rc = l.addAPIKey(name)
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
	cmd := exec.Command("claude", append([]string{"auth"}, args...)...)
	env := os.Environ()
	for _, k := range append(append([]string{}, l.backend...), "CLAUDE_SECURESTORAGE_CONFIG_DIR") {
		env = envDrop(env, k)
	}
	cmd.Env = append(env, "CLAUDE_CONFIG_DIR="+dir)
	return cmd
}

var emailRE = regexp.MustCompile(`"email": "([^"]*)"`)

func (l *launcher) addSub(name string) int {
	if atomicWrite(l.conf(name), "type=sub\n") != nil {
		l.err("could not save profile '" + name + "'")
		return 1
	}
	if !l.setupDir(name, "sub") {
		l.rollback(name)
		return 1
	}
	dir := filepath.Join(l.profileDirs, name)
	if l.claudeAuth(dir, "logout").Run() != nil {
		l.err("logout failed")
		l.rollback(name)
		return 1
	}
	fmt.Println(l.dim + "Opening your browser to sign in — Ctrl-C to cancel." + l.reset)
	login := l.claudeAuth(dir, "login")
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	if login.Run() != nil {
		l.err("login failed")
		l.rollback(name)
		return 1
	}
	out, err := l.claudeAuth(dir, "status").CombinedOutput()
	if err != nil {
		l.err("could not read authentication status")
		l.rollback(name)
		return 1
	}
	email := ""
	if m := emailRE.FindSubmatch(out); m != nil {
		email = string(m[1])
	}
	if email == "" || strings.ContainsAny(email, "\r\n") {
		l.err("no subscription profile found after login — nothing was saved")
		l.rollback(name)
		return 1
	}
	if atomicWrite(l.conf(name), "type=sub\nemail="+email+"\n") != nil {
		l.err("could not save profile '" + name + "'")
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (sub: " + email + ")")
	return 0
}

func (l *launcher) addToken(name string) int {
	token, rc := uiInput("Paste token for "+name+" (hidden): ", true)
	if rc != 0 {
		return 1
	}
	if token == "" {
		l.err("empty token")
		return 1
	}
	if strings.ContainsAny(token, "\r\n") {
		l.err("token contains CR/LF")
		return 1
	}
	if atomicWrite(l.conf(name), "type=token\ntoken="+token+"\n") != nil {
		l.err("could not save profile '" + name + "'")
		return 1
	}
	if _, ok := l.readRaw(name); !ok || !l.setupDir(name, "token") {
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (token)")
	return 0
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
	var rows []string
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
	if pick != "custom" {
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
		prompt += " (" + label + ")"
	}
	key, rc := uiInput(prompt+" (hidden): ", true)
	if rc != 0 {
		return 1
	}
	if key == "" {
		l.err("empty api key")
		return 1
	}
	if label == "" {
		v, rc := uiInput("Base URL (blank = api.anthropic.com): ", false)
		if rc != 0 {
			return 1
		}
		url = v
	}
	if strings.ContainsAny(key+url, "\r\n") {
		l.err("apikey values cannot contain CR/LF")
		return 1
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
	if _, ok := l.read(name); !ok || !l.setupDir(name, "apikey") {
		l.rollback(name)
		return 1
	}
	shown, kind := url, label
	if shown == "" {
		shown = "api.anthropic.com"
	}
	if kind == "" {
		kind = "apikey"
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (" + kind + " → " + shown + ")")
	if url != "" {
		l.probeNudge(name)
	}
	fmt.Println(l.dim + "Claude Code may ask you to approve this key on first launch." + l.reset)
	return 0
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
	if _, ok := l.readRaw(name); !ok || !l.setupDir(name, "provider") {
		l.rollback(name)
		return 1
	}
	fmt.Println(l.accent + "Added" + l.reset + " " + name + " (provider: " + provider + ")")
	fmt.Println(l.dim + "Set a default model:  ccmorph set " + name + " model <model-id>" + l.reset)
	fmt.Println(l.dim + "Configure defaults: ccmorph config " + name + l.reset)
	return 0
}

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
		line = line[:strings.Index(line, "-login")]
		if line != "" && !strings.Contains(line, "device") {
			list = append(list, line)
		}
	}
	return list, true
}

// providerLogin is _ccm_provider_login: cliproxyapi's browser login into auth/<name>.
func (l *launcher) providerLogin(name, provider string) bool {
	bin, err := exec.LookPath("cliproxyapi")
	if err != nil {
		l.err("cliproxyapi not found — install it: brew install cliproxyapi")
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
	fmt.Println(l.dim + "Opening your browser to sign in to " + l.bold + provider + l.reset + l.dim + " — Ctrl-C to cancel." + l.reset)
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
		l.err(provider + " login failed")
		return false
	}
	for _, pat := range []string{"*.json", "*/*.json"} {
		if m, _ := filepath.Glob(filepath.Join(auth, pat)); len(m) > 0 && cred == "" {
			for _, f := range m {
				if isFile(f) {
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

// login is `ccmorph login [profile]`: sign a provider profile in again.
func (l *launcher) login(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" || interactive(name) {
		picked, ok := l.menuProfile("provider")
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		l.err("usage: ccmorph login <name>")
		return 1
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'")
		l.accountsHint()
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		return 1
	}
	if f["type"] != "provider" {
		l.err("'" + name + "' is not a provider profile — login is only for provider profiles")
		return 1
	}
	if !l.providerLogin(name, f["provider"]) {
		return 1
	}
	return 0
}
