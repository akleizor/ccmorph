package launch

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/profile"
)

var nativeModelFlags = map[string]bool{
	"--model": true, "--dangerously-skip-permissions": true, "-r": true, "--resume": true,
	"-c": true, "--continue": true, "--from-pr": true, "-p": true, "--print": true,
	"-h": true, "--help": true, "-v": true, "--version": true, "--bg": true, "--background": true,
}

func runFlag(a string) bool {
	return a == "-a" || a == "--profile" || strings.HasPrefix(a, "--profile=") || a == "-i" || a == "--interactive"
}

// launch is one pass of _ccm_launch: nil when the run is over (rc is its status),
// or the relaunch a hop asked for.
func (l *launcher) launch(name string, interactive, native bool, argv []string) (*relayNext, int) {
	var args []string
	if native {
		args = argv
	} else if len(argv) > 0 {
		need := false
		for i, a := range argv {
			if a == "--" {
				break
			}
			if len(a) > 1 && a[0] == '-' && !runFlag(a) && flagAhead(argv[i+1:]) {
				need = true
				break
			}
		}
		table := ""
		if need {
			table = l.claudeArity()
		}
		out, errOut, rc := l.core(nil, table, append([]string{"util", "launch-argv"}, argv...)...)
		if rc != 0 {
			l.err(strings.TrimSpace(out + errOut))
			return nil, 2
		}
		lines := strings.SplitN(out, "\n", 3)
		for _, line := range lines[:min(2, len(lines))] {
			switch {
			case strings.HasPrefix(line, "profile="):
				if v := strings.TrimPrefix(line, "profile="); v != "" {
					name = v
				}
			case line == "interactive=1":
				interactive = true
			}
		}
		if len(lines) == 3 {
			args = splitNUL(lines[2])
		}
	}

	if interactive && name == "" {
		picked, ok := l.zshUI("_ccm_menu_profile")
		if !ok {
			return nil, 1
		}
		if picked == "" {
			l.err("no profile chosen")
			return nil, 1
		}
		name = picked
	}
	if name != "" {
		if !profile.ValidName(name) {
			l.err("invalid profile name '" + name + "'")
			return nil, 2
		}
	} else if active, ok := l.readActive(); ok {
		name = active
	}

	defaultModel, tag := "", ""
	routed, isProvider, needsAdapter := false, false, false
	if name != "" {
		if !l.exists(name) {
			l.err("no profile '" + name + "'")
			return nil, 1
		}
		f, ok := l.read(name)
		if !ok {
			return nil, 1
		}
		switch f["type"] {
		case "apikey":
			routed, tag = true, f["tag"]
			switch f["base_url"] {
			case "", "https://api.anthropic.com", "https://api.anthropic.com/":
			default:
				needsAdapter = true
			}
		case "provider":
			routed, isProvider = true, true
			tag = f["tag"]
			if tag == "" {
				tag = f["provider"]
			}
		}
		var rc int
		if defaultModel, rc = l.configStore("seed-model-defaults", name, "model"); rc != 0 {
			return nil, 1
		}
	}

	subcommand := os.Getenv("CCMORPH_SUBCOMMAND_LAUNCH") == "1"
	table := ""
	for _, a := range args {
		if a == "--" {
			break
		}
		if len(a) > 1 && a[0] == '-' && !nativeModelFlags[a] && !strings.HasPrefix(a, "--model=") &&
			!strings.HasPrefix(a, "--resume=") && !strings.HasPrefix(a, "--from-pr=") && !strings.HasPrefix(a, "--print=") {
			table = l.claudeArity()
			break
		}
	}
	routedArg := "0"
	if routed {
		routedArg = "1"
	}
	out, errOut, rc := l.core(nil, table, append([]string{"util", "launch-flags", routedArg}, args...)...)
	if rc != 0 {
		l.err(strings.TrimSpace(out + errOut))
		return nil, 2
	}
	explicit, resume, launchModel := "", "", ""
	userModel, skipModel, askMissing, resumeCarried := false, false, true, false
	var relayArgs []string
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "model":
			explicit = v
		case "user_model":
			userModel = v == "1"
		case "skip_model":
			skipModel = v == "1"
		case "ask_missing_model":
			askMissing = v == "1"
		case "resume":
			resume = v
		case "interactive":
			if v == "1" {
				interactive = true
			}
		case "relay":
			relayArgs = append(relayArgs, v)
		case "drop":
			var d int
			fmt.Sscan(v, &d)
			if d >= 1 && d <= len(args) {
				end := min(d+1, len(args))
				args = append(args[:d-1:d-1], args[end:]...)
			}
		}
	}

	if needsAdapter && askMissing && !subcommand && explicit == "" && defaultModel == "" && isTTY(0) && isTTY(2) {
		interactive = true
	}
	if interactive && explicit == "" && routed {
		if name == "" {
			l.err("-i needs an profile: name one, or set a default with ccmorph use <profile>")
			return nil, 2
		}
		picked, ok := l.zshUI(`ccmorph _menu "$1"`, name)
		if !ok {
			return nil, 1
		}
		if picked == "" {
			l.err("no model chosen")
			return nil, 1
		}
		explicit, launchModel, skipModel = picked, picked, true
	}
	if routed && explicit == "" && resume != "" {
		if m, ok := l.resumeModel(resume); ok {
			if l.modelKnown(name, m) == 1 {
				fmt.Printf("%sccmorph:%s %s%s%s is not served by %s%s%s — resuming on its default model.\n",
					l.yellow, l.reset, l.bold, m, l.reset, l.bold, name, l.reset)
			} else {
				defaultModel, resumeCarried = m, true
			}
		}
	}
	target := defaultModel
	if explicit != "" {
		target = explicit
	}
	if strings.HasPrefix(target, "claude-") && (needsAdapter || isProvider) {
		rec := strings.Split(l.modelWindowFields(name, target), "\t")
		if send := field(rec, 4); send != "" && send != target {
			l.log("send_id derived profile=" + name + " from=" + target + " to=" + send)
			switch {
			case userModel:
				fmt.Fprintf(os.Stderr, "%sccmorph: this route serves %s; --model %s runs at Claude Code's own window for that id%s\n", l.dim, send, explicit, l.reset)
			case explicit != "":
				explicit, launchModel = send, send
			default:
				defaultModel = send
			}
		}
	}
	if !skipModel && defaultModel != "" {
		launchModel = defaultModel
	}

	now := time.Now()
	relayID := fmt.Sprintf("%d.%d_%06d.%d", l.launcherPID, now.Unix(), now.Nanosecond()/1000, rand.IntN(32768))
	relayFile := filepath.Join(l.dir, ".reswitch."+relayID)
	l.utilQ(nil, "", "gc-stale", l.dir, l.profileDirs, tmpDir())
	if err := os.Remove(relayFile); err != nil && !os.IsNotExist(err) {
		return nil, 1
	}
	if name != "" {
		l.log("launch profile=" + name + " relay_id=" + relayID)
	} else {
		l.log("launch profile=<none> relay_id=" + relayID)
	}
	fail := func() (*relayNext, int) { os.Remove(relayFile); return nil, 1 }

	common := []string{
		"CCMORPH_RELAY_ID=" + relayID,
		"CCMORPH_LIB_DIR=" + l.libDir,
		"CLAUDE_CODE_PLUGIN_DIRS=" + prefixed(os.Getenv("CLAUDE_CODE_PLUGIN_DIRS")) + filepath.Join(filepath.Dir(l.libDir), "share", "claude-plugin"),
	}
	var rcClaude int
	if name != "" {
		penv, ok := l.profileEnvironment(name)
		if !ok {
			return fail()
		}
		l.foldSettings(name)
		if isProvider {
			if l.providerActive != name {
				l.providerStop()
				if !l.providerStart(name) {
					return fail()
				}
			}
			if penv, ok = l.route(penv, l.providerPort, l.providerKey, false); !ok {
				return fail()
			}
		}
		base, _ := envGet(penv, "ANTHROPIC_BASE_URL")
		key := authKey(penv)
		if listing, ok := l.listing(base, key); ok {
			if _, rc := l.util([]string{"CCMORPH_STATE_DIR=" + l.dir}, listing, "listing-has-unsized", name); rc == 0 {
				l.logInit()
				l.core([]string{"CCMORPH_STATE_DIR=" + l.dir, "ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_API_KEY=" + key},
					listing+"\n", "context-probe", "--listing-stdin", "--profile", name)
			}
		}
		if !l.projectPicker(name, base, key, defaultModel, tag, explicit) {
			return nil, 1
		}
		if needsAdapter {
			if l.adapterProfile != name {
				l.adapterStop()
				model := explicit
				if model == "" {
					model = defaultModel
				}
				if bare := strings.SplitN(model, "[", 2)[0]; isTier(bare) {
					if v, ok := envGet(penv, "ANTHROPIC_DEFAULT_"+strings.ToUpper(bare)+"_MODEL"); ok {
						model = v
					}
					if strings.SplitN(model, "[", 2)[0] == bare {
						v, ok := l.tierAlias(name, bare)
						if !ok {
							l.err("no gateway model is mapped to tier '" + bare + "'; choose an explicit --model id")
							return nil, 1
						}
						model = v
					}
				}
				if !l.adapterStart(name, model) {
					return fail()
				}
			}
			if penv, ok = l.route(penv, "", "", true); !ok {
				return fail()
			}
		} else {
			l.adapterStop()
		}
		if len(penv) == 0 {
			l.err("profile '" + name + "' produced no environment")
			return fail()
		}
		if !isProvider && !needsAdapter {
			for _, line := range l.nestedEdgeEnv("CLAUDE_CODE_DISABLE_ADVISOR_TOOL", "CLAUDE_CODE_MODEL_CAPABILITIES") {
				penv = append(envDrop(penv, strings.SplitN(line, "=", 2)[0]), line)
			}
		}
		var ok2 bool
		if penv, ok2 = l.contextEnv(name, penv, explicit, defaultModel, launchModel, resumeCarried, needsAdapter, isProvider, subcommand); !ok2 {
			return nil, 1
		}
		if resume != "" {
			l.carrySession(resume, filepath.Join(l.profileDirs, name, "projects"))
		}
		setLayers := []string{}
		for _, k := range l.tuning {
			if l.chosen[k] {
				setLayers = append(setLayers, k)
			}
		}
		env := append(append(penv, "CCMORPH_SET_ENV="+strings.Join(setLayers, " ")), common...)
		rcClaude = l.runClaude(env, args)
		if l.forwardedSig() != 0 {
			os.Remove(relayFile)
			l.log(fmt.Sprintf("signal %d forwarded to claude; no relay", l.forwardedSig()))
			return nil, rcClaude
		}
		l.log(fmt.Sprintf("exit rc=%d profile=%s relay_file_exists=%s", rcClaude, name, yesNo(isFile(relayFile))))
	} else {
		edge := l.nestedEdgeEnv("CLAUDE_CODE_DISABLE_ADVISOR_TOOL", "CLAUDE_CODE_MODEL_CAPABILITIES")
		if resume != "" {
			l.carrySession(resume, filepath.Join(os.Getenv("HOME"), ".claude", "projects"))
		}
		rcClaude = l.runClaude(append(edge, common...), args)
		if l.forwardedSig() != 0 {
			os.Remove(relayFile)
			l.log(fmt.Sprintf("signal %d forwarded to claude; no relay", l.forwardedSig()))
			return nil, rcClaude
		}
		l.log(fmt.Sprintf("exit rc=%d profile=<none> relay_file_exists=%s", rcClaude, yesNo(isFile(relayFile))))
	}

	out, errOut, rc = l.core(nil, "", "util", "relay-read", relayFile)
	if rc == 3 {
		l.log("normal exit (no relay)")
		return nil, rcClaude
	} else if rc != 0 {
		msg := strings.TrimSpace(out + errOut)
		if msg == "" {
			msg = "invalid relay signal"
		}
		l.err(msg)
		return nil, 1
	}
	rs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			rs[k] = v
		}
	}
	to := rs["profile"]
	if !profile.ValidName(to) || !l.exists(to) || rs["session"] == "" {
		l.err("invalid relay signal")
		return nil, 1
	}
	f, ok := l.read(to)
	if !ok {
		return nil, 1
	}
	if !l.setupDir(to, f["type"]) {
		return nil, 1
	}
	next := &relayNext{args: append([]string{}, relayArgs...), profile: to, session: rs["session"]}
	if rs["model"] != "" {
		next.args = append(next.args, "--model", rs["model"])
	}
	if rs["effort"] != "" {
		next.args = append(next.args, "--effort", rs["effort"])
	}
	next.args = append(next.args, "--resume", rs["session"])
	next.crossed = name == "" || !sameFile(l.conf(name), l.conf(to))
	return next, 0
}

func flagAhead(rest []string) bool {
	for _, a := range rest {
		if a == "--" {
			return false
		}
		if runFlag(a) {
			return true
		}
	}
	return false
}

func field(rec []string, n int) string {
	if n-1 < len(rec) {
		return rec[n-1]
	}
	return ""
}

func isTier(s string) bool { return s == "opus" || s == "sonnet" || s == "haiku" || s == "fable" }

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func prefixed(s string) string {
	if s == "" {
		return ""
	}
	return s + ":"
}

func tmpDir() string {
	if t := os.Getenv("TMPDIR"); t != "" {
		return t
	}
	return "/tmp"
}

func sameFile(a, b string) bool {
	fa, err1 := os.Stat(a)
	fb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(fa, fb)
}

func authKey(env []string) string {
	if v, ok := envGet(env, "ANTHROPIC_API_KEY"); ok {
		return v
	}
	v, _ := envGet(env, "ANTHROPIC_AUTH_TOKEN")
	return v
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// sessionFile is _ccm_session_file: the physical transcript of a session, from
// native Claude Code's history or any profile's.
func (l *launcher) sessionFile(id string) (string, bool) {
	if !sessionID.MatchString(id) {
		return "", false
	}
	stores := []string{filepath.Join(os.Getenv("HOME"), ".claude", "projects")}
	more, _ := filepath.Glob(filepath.Join(l.profileDirs, "*", "projects"))
	for _, store := range append(stores, more...) {
		hits, _ := filepath.Glob(filepath.Join(store, "*", id+".jsonl"))
		for _, h := range hits {
			if isFile(h) {
				if real, err := filepath.EvalSymlinks(h); err == nil {
					abs, _ := filepath.Abs(real)
					return abs, true
				}
			}
		}
	}
	return "", false
}

func (l *launcher) resumeModel(id string) (string, bool) {
	if strings.ContainsAny(id, "\n\r") {
		return "", false
	}
	transcript, ok := l.sessionFile(id)
	if !ok {
		return "", false
	}
	m, rc := l.util(nil, "", "resume-model", transcript)
	if rc != 0 || m == "" || strings.ContainsAny(m, "\n\r") || strings.ContainsAny(m, " \t\f\v") ||
		strings.HasPrefix(m, "-") || strings.HasPrefix(m, "@") || m == "?" {
		return "", false
	}
	return m, true
}

// carrySession is _ccm_carry_session: link a session from another store into dst.
func (l *launcher) carrySession(id, dst string) {
	if !sessionID.MatchString(id) {
		return
	}
	hits, _ := filepath.Glob(filepath.Join(dst, "*", id+".jsonl"))
	for _, h := range hits {
		if isFile(h) {
			return
		}
	}
	f, ok := l.sessionFile(id)
	if !ok {
		return
	}
	slug := filepath.Base(filepath.Dir(f))
	if os.MkdirAll(filepath.Join(dst, slug), 0o777) != nil {
		return
	}
	link := filepath.Join(dst, slug, id+".jsonl")
	os.Remove(link)
	if os.Symlink(f, link) != nil {
		return
	}
	side := strings.TrimSuffix(f, ".jsonl")
	if os.MkdirAll(side, 0o777) == nil {
		if _, err := os.Lstat(filepath.Join(dst, slug, id)); os.IsNotExist(err) {
			os.Symlink(side, filepath.Join(dst, slug, id))
		}
	}
}

// migrateState is _ccm_migrate_state: move legacy context state forward once.
func (l *launcher) migrateState() bool {
	if os.Getenv("CCMORPH_CONTEXT_LOCKED") != "" {
		return true
	}
	legacy := []string{filepath.Join(l.dir, "model-registry"), filepath.Join(l.dir, "model-registry.conf")}
	g1, _ := filepath.Glob(filepath.Join(l.profileDirs, "*", "cache", "context-window"))
	g2, _ := filepath.Glob(filepath.Join(l.dir, "*.conf.lock"))
	legacy = append(append(legacy, g1...), g2...)
	confs, _ := filepath.Glob(filepath.Join(l.dir, "*.conf"))
	inline, plain := false, false
	for _, c := range confs {
		b, err := os.ReadFile(c)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "context_discovered=") {
				inline = true
			}
			if (strings.HasPrefix(line, "api_key=") || strings.HasPrefix(line, "token=")) &&
				len(line) > strings.Index(line, "=")+1 && !strings.Contains(line, "=keychain:") {
				plain = true
			}
		}
	}
	if inline {
		legacy = append(legacy, l.dir)
	}
	if plain && isDarwin() && exec.Command("security", "default-keychain").Run() == nil {
		l.util(nil, "", "keychain-migrate", l.dir)
	}
	for _, p := range legacy {
		if _, err := os.Lstat(p); err == nil {
			if _, _, rc := l.core([]string{"CCMORPH_STATE_DIR=" + l.dir}, "", "model-window", "--migrate"); rc != 0 {
				l.err("context migration incomplete; original data kept for retry")
				return false
			}
			return true
		}
	}
	return true
}

func isDarwin() bool { return strings.HasPrefix(goos, "darwin") }

func (l *launcher) windowEnv(name string) []string {
	return []string{"CCMORPH_STATE_DIR=" + l.dir, "MODEL_WINDOW_PROFILE_CONF=" + l.conf(name)}
}

func (l *launcher) coreNoCeiling(env []string, args ...string) string {
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), args...)
	cmd.Env = append(envDrop(os.Environ(), "CLAUDE_CODE_MAX_CONTEXT_TOKENS"), env...)
	out, _ := cmd.Output()
	return strings.TrimRight(string(out), "\n")
}

func (l *launcher) modelWindowFields(name, model string) string {
	if !l.migrateState() {
		return ""
	}
	return l.coreNoCeiling(l.windowEnv(name), "model-window", "--fields", name, model)
}

func (l *launcher) launchWindow(name, model string) []string {
	if model != "" && !l.migrateState() {
		model = ""
	}
	args := append([]string{"util", "launch-window", name, model}, l.settingsSources(name)...)
	return strings.Split(l.coreNoCeiling(l.windowEnv(name), args...), "\t")
}

func (l *launcher) tierAlias(name, tier string) (string, bool) {
	v, rc := l.configStore("get", name, "env.ANTHROPIC_DEFAULT_"+strings.ToUpper(tier)+"_MODEL")
	return v, rc == 0 && v != ""
}

// listing is _ccm_models_listing: one /v1/models answer per (base, key) per run.
func (l *launcher) listing(base, key string) (string, bool) {
	if base == "" || key == "" {
		return "", false
	}
	mk := base + "\x1f" + key
	if v, ok := l.memo[mk]; ok && v != "" {
		return v, true
	}
	v, rc := l.utilQ(nil, key+"\n", "fetch-listing", base, "launch", "8")
	if rc != 0 {
		return "", false
	}
	l.memo[mk] = v
	return v, true
}

func (l *launcher) projectPicker(name, base, key, own, tag, launch string) bool {
	home := filepath.Join(l.profileDirs, name)
	if fi, err := os.Lstat(filepath.Join(home, ".claude.json")); err != nil || !fi.Mode().IsRegular() {
		return true
	}
	if base == "" || key == "" {
		return true
	}
	body, _ := l.listing(base, key)
	if tag == "" {
		tag = "Gateway"
	}
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), "util", "project-picker", home, l.conf(name),
		own, tag, name, l.profileLayer(name), launch)
	cmd.Env = append(os.Environ(), "_CCM_MODELS_JSON="+body)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run() == nil
}

// profileEnvironment is `ccmorph _env`: the profile's launch env, after
// provisioning its config dir and running the host's prelaunch step.
func (l *launcher) profileEnvironment(name string) ([]string, bool) {
	out, errOut, rc := l.core(nil, "", "util", "profile-env", l.dir, l.profileDirs, name,
		strings.Join(l.profileEnv, ","), strings.Join(l.mapping, ","))
	if errOut != "" {
		fmt.Fprintln(os.Stderr, errOut)
	}
	if rc != 0 {
		if out == "" {
			out = "could not read profile '" + name + "'"
		}
		l.err(out)
		return nil, false
	}
	first, rest, _ := strings.Cut(out, "\n")
	if !l.setupDir(name, strings.TrimPrefix(first, "type=")) {
		return nil, false
	}
	l.prelaunch(name)
	if rest == "" {
		return nil, true
	}
	return strings.Split(rest, "\n"), true
}

// setupDir is _ccm_setup_dir: provision the profile's config dir unless it is.
func (l *launcher) setupDir(name, typ string) bool {
	if _, rc := l.util(nil, "", "profile-ok", l.profileDirs, name); rc == 0 {
		return true
	}
	out, rc := l.util(nil, "", "setup-profile", l.profileDirs, name, typ)
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "err "):
			l.err(line[4:])
		case strings.HasPrefix(line, "log "):
			l.log(line[4:])
		}
	}
	if rc != 0 {
		return false
	}
	if file := l.profileLayer(name); isFile(file) {
		if fi, err := os.Lstat(file); err == nil && fi.Mode().IsRegular() {
			l.utilQ([]string{"_CCM_SL_OWNED=" + filepath.Join(l.dir, "statusline") + "\n" + filepath.Join(l.libDir, "statusline")},
				"", "unwire-statusline", file)
		}
	}
	return true
}

// prelaunch runs CCMORPH_PRELAUNCH as `<exe> prelaunch <cwd>`: silent, 10s, best effort.
func (l *launcher) prelaunch(name string) {
	exe := os.Getenv("CCMORPH_PRELAUNCH")
	if exe == "" {
		return
	}
	if !strings.Contains(exe, "/") {
		p, err := exec.LookPath(exe)
		if err != nil {
			return
		}
		exe = p
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	cwd, _ := os.Getwd()
	cmd := exec.Command(exe, "prelaunch", cwd)
	cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+filepath.Join(l.profileDirs, name))
	if cmd.Start() != nil {
		return
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Signal(syscall.SIGALRM)
		<-done
	}
}

// foldSettings is _ccm_fold_settings: fold Claude Code's own writes to the
// generated settings back into the profile layer.
func (l *launcher) foldSettings(name string) {
	dir := filepath.Join(l.profileDirs, name)
	s, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return
	}
	if base, err := os.ReadFile(filepath.Join(dir, "cache", "settings.base.json")); err == nil &&
		strings.TrimRight(string(s), "\n") == strings.TrimRight(string(base), "\n") {
		return
	}
	l.core(nil, "", append([]string{"settings-compose"}, l.settingsArgv("fold", name)...)...)
}

func (l *launcher) settingsArgv(verb, name string, rest ...string) []string {
	argv := []string{verb, filepath.Join(l.profileDirs, name), "--canonical", l.canonical()}
	for _, k := range l.mapping {
		argv = append(argv, "--mapping="+k)
	}
	return append(argv, rest...)
}

func (l *launcher) route(penv []string, port, key string, adapter bool) ([]string, bool) {
	a := "0"
	if adapter {
		a = "1"
	}
	out, rc := l.util(nil, strings.Join(penv, "\n")+"\n", "launch-env", port, key, a, l.adapterPort,
		os.Getenv("CCMORPH_TASK"), os.Getenv("CCMORPH_ATTEMPT"))
	if rc != 0 {
		return nil, false
	}
	return splitNUL(out), true
}

var loopbackProxy = regexp.MustCompile(`^http://127\.0\.0\.1:([0-9]+)/?$`)

// nestedEdgeEnv is _ccm_nested_edge_env: inside the Remote Control edge, the
// route keys it named for a native launch.
func (l *launcher) nestedEdgeEnv(keys ...string) []string {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	m := loopbackProxy.FindStringSubmatch(proxy)
	if m == nil {
		return nil
	}
	if p, ok := posInt(m[1]); !ok || p > 65535 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(os.Getenv("CCMORPH_EDGE_ROUTE_ENV"), "\n") {
		k, _, ok := strings.Cut(line, "=")
		if ok {
			for _, want := range keys {
				if k == want {
					out = append(out, line)
				}
			}
		}
	}
	return out
}

func (l *launcher) providerStart(name string) bool {
	bin, err := exec.LookPath("cliproxyapi")
	if err != nil {
		l.err("cliproxyapi not found — install it: brew install cliproxyapi")
		return false
	}
	root := filepath.Join(l.dir, "auth")
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		l.err("auth directory root is a symlink, refusing: " + root)
		return false
	}
	auth := filepath.Join(root, name)
	if fi, err := os.Lstat(auth); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		l.err("auth directory for '" + name + "' is a symlink, refusing: " + auth)
		return false
	}
	if fi, err := os.Stat(auth); err != nil || !fi.IsDir() {
		l.err("no auth directory for '" + name + "' — run: ccmorph login " + name)
		return false
	}
	l.logInit()
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), "util", "bridge-start", name, bin, auth, l.logOrNull())
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "PID":
			l.providerPID = v
		case "PORT":
			l.providerPort = v
		case "KEY":
			l.providerKey = v
		case "TMPDIR":
			l.providerTD = v
		}
	}
	l.providerAuth, l.providerActive = auth, name
	return true
}

func (l *launcher) providerStop() {
	if l.providerPID+l.providerTD+l.providerAuth != "" {
		pid := l.providerPID
		if pid == "" {
			pid = "0"
		}
		l.util(nil, "", "bridge-stop", pid, l.providerTD, l.providerAuth)
	}
	l.providerPID, l.providerActive, l.providerPort, l.providerKey, l.providerTD, l.providerAuth = "", "", "", "", "", ""
}

func (l *launcher) logOrNull() string {
	if l.logPath == "" {
		return "/dev/null"
	}
	return l.logPath
}

func (l *launcher) adapterStart(name, model string) bool {
	bin := filepath.Join(l.libexec, "ccmorph-core")
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm()&0o111 == 0 {
		l.err("profile '" + name + "' needs an adapter but " + bin + " is not executable")
		return false
	}
	l.logInit()
	cmd := exec.Command(bin, "util", "adapter-start", bin, name, model, fmt.Sprint(l.launcherPID), l.logOrNull())
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		fmt.Fprintln(os.Stderr, "  Check configuration: ccmorph inspect "+name)
		if l.logPath != "" && fileExists(l.logPath) {
			fmt.Fprintln(os.Stderr, "  Log: "+l.logPath)
		}
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch {
		case k == "PID":
			fmt.Sscan(v, &l.adapterPID)
		case k == "PORT":
			l.adapterPort = v
		case line == "NATIVE=1":
			l.adapterStop()
			l.adapterProfile, l.adapterPort = name, ""
			return true
		}
	}
	l.adapterProfile = name
	return true
}

// adapterStop is _ccm_adapter_stop: TERM, 12s for the adapter to stop its own
// child, then KILL.
func (l *launcher) adapterStop() {
	if pid := l.adapterPID; pid > 0 {
		syscall.Kill(pid, syscall.SIGTERM)
		alive := func() bool { return syscall.Kill(pid, 0) == nil }
		for i := 0; alive() && i < 240; i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if alive() {
			syscall.Kill(pid, syscall.SIGKILL)
			for i := 0; alive() && i < 40; i++ {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
	l.adapterPID, l.adapterProfile, l.adapterPort = 0, "", ""
}

// runClaude is _ccm_run_claude: Claude Code as this launcher's child, through the
// core's session setup, its status as a shell would report it.
func (l *launcher) runClaude(env, args []string) int {
	argv := append([]string{"util", "claude-session", strings.Join(l.tuning, " "), strings.Join(l.profileEnv, " "),
		strings.Join(l.launchEnv, " "), strings.Join(l.backend, " ")}, env...)
	argv = append(append(argv, "--"), args...)
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), argv...)
	cmd.Env = append(os.Environ(), "CCMORPH_NOTICE="+l.notice)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	l.mu.Lock()
	l.forwarded = 0
	if err := cmd.Start(); err != nil {
		l.mu.Unlock()
		fmt.Fprintln(os.Stderr, "ccmorph: "+err.Error())
		return 127
	}
	l.child = cmd.Process
	l.mu.Unlock()
	err := cmd.Wait()
	l.mu.Lock()
	l.child = nil
	l.mu.Unlock()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 1
}

func (l *launcher) forwardedSig() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.forwarded
}

// modelKnown is _ccm_model_known: 0 served, 1 not served, 2 cannot tell.
func (l *launcher) modelKnown(name, want string) int {
	if name == "" || want == "" {
		return 2
	}
	out, _, rc := l.core(nil, "", "util", "profile-read", l.dir, name, "1",
		strings.Join(l.profileEnv, ","), strings.Join(l.mapping, ","))
	if rc != 0 {
		return 2
	}
	f := map[string]string{}
	parts := strings.Split(strings.TrimSuffix(out, "."), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		f[parts[i]] = parts[i+1]
	}
	if f["type"] == "apikey" {
		var in strings.Builder
		for _, k := range []string{"type", "api_key", "base_url", "adapter"} {
			in.WriteString(k + "\x00" + f[k] + "\x00")
		}
		if _, rc := l.utilQ(nil, in.String(), "profile-validate", name); rc != 0 {
			return 2
		}
	}
	_, rc = l.utilQ(nil, f["api_key"]+"\n", "model-served", want, f["type"], f["base_url"], f["model_map"])
	return rc
}
