package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var goos = runtime.GOOS

var routeDefaults = []string{
	"CLAUDE_CODE_MODEL_CAPABILITIES=-mid_conv_system,-mid_conv_tool_change,-per_turn_effort",
	"CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1",
	"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1",
}

func (l *launcher) engineInt(v string) string {
	out, _ := l.utilQ(nil, "", "engine-int", v)
	return out
}

func (l *launcher) clampACW(v string) string {
	if out, rc := l.util(nil, "", "clamp-acw", v); rc == 0 {
		return out
	}
	return v
}

// triggerEnv is _ccm_trigger_env: the env's compaction percentage, output
// reserve and 1M kill switch, read as the engine reads them.
func (l *launcher) triggerEnv(penv []string) (pct, maxout, no1m string) {
	for _, e := range penv {
		k, v, _ := strings.Cut(e, "=")
		switch k {
		case "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE":
			pct = v
		case "CLAUDE_CODE_MAX_OUTPUT_TOKENS":
			maxout = l.engineInt(v)
		case "CLAUDE_CODE_DISABLE_1M_CONTEXT":
			no1m = v
		}
	}
	if no1m != "" {
		if _, rc := l.util(nil, "", "engine-bool", no1m); rc == 0 {
			no1m = "1"
		} else {
			no1m = ""
		}
	}
	return
}

func (l *launcher) compactionPercent() (string, bool) {
	if _, err := os.Stat(l.policy); err != nil {
		return "75", true
	}
	out, rc := l.utilQ(nil, "", "policy-pct", l.policy)
	if rc != 0 {
		l.err("invalid fleet context policy: " + l.policy)
		return "", false
	}
	return out, true
}

func (l *launcher) hazardSet(name, budget, ceiling, settings string, penv []string) string {
	sources := []string{settings}
	if settings == "" {
		sources = l.settingsSources(name)
	}
	any := false
	for _, s := range sources {
		any = any || isFile(s)
	}
	if !any || !isFile(l.conf(name)) {
		return ""
	}
	pct, maxout, no1m := l.triggerEnv(penv)
	inEnv := ""
	if envHas(penv, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE") {
		inEnv = "1"
	}
	out, rc := l.util([]string{"_CCM_SOURCES=" + strings.Join(sources, "\n"), "_CCM_PROFILE_CONF=" + l.conf(name),
		"_CCM_BUDGET=" + budget, "_CCM_CEILING=" + ceiling, "_CCM_PCT=" + pct, "_CCM_PROFILE=" + name,
		"_CCM_MAXOUT=" + maxout, "_CCM_NO1M=" + no1m, "_CCM_PCT_IN_ENV=" + inEnv}, "", "hazard-set")
	if rc != 0 {
		return ""
	}
	return out
}

func withheld() string {
	if v := os.Getenv("CCMORPH_WITHHOLD_FATAL"); v != "" {
		return v
	}
	return "0"
}

// contextEnv is the window, compaction and settings half of a launch: the env
// it adds, the projection of the profile's settings, and its advisories.
func (l *launcher) contextEnv(name string, penv []string, explicit, defaultModel, launchModel string,
	resumeCarried, needsAdapter, isProvider, subcommand bool) ([]string, bool) {
	derived, pceil, pmodel := "", "", ""
	refresh, plwRan := false, false
	var plw []string
	if !envHas(penv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS") {
		pbase, pkey := "", ""
		alias := map[string]string{}
		for _, e := range penv {
			k, v, _ := strings.Cut(e, "=")
			switch k {
			case "ANTHROPIC_BASE_URL":
				pbase = v
			case "ANTHROPIC_API_KEY":
				pkey = v
			case "ANTHROPIC_AUTH_TOKEN":
				if pkey == "" {
					pkey = v
				}
			case "ANTHROPIC_MODEL":
				pmodel = v
			case "ANTHROPIC_DEFAULT_OPUS_MODEL":
				alias["opus"] = v
			case "ANTHROPIC_DEFAULT_SONNET_MODEL":
				alias["sonnet"] = v
			case "ANTHROPIC_DEFAULT_HAIKU_MODEL":
				alias["haiku"] = v
			case "ANTHROPIC_DEFAULT_FABLE_MODEL":
				alias["fable"] = v
			}
		}
		_ = pkey
		if explicit != "" {
			pmodel = explicit
		}
		if pmodel == "" {
			pmodel = defaultModel
		}
		bare := strings.SplitN(pmodel, "[", 2)[0]
		if alias[bare] == "" && isTier(bare) {
			if v, ok := l.tierAlias(name, bare); ok {
				alias[bare] = v
			}
		}
		if alias[bare] != "" {
			l.log("alias resolved profile=" + name + " alias=" + pmodel + " model=" + alias[bare])
			pmodel = alias[bare]
		}
		pmodelEnv := pmodel
		if strings.HasPrefix(pmodelEnv, "claude-") {
			pmodelEnv = ""
		}
		plw = l.launchWindow(name, strings.SplitN(pmodel, "[", 2)[0])
		plwRan = true
		pwin := ""
		if pmodel != "" {
			if _, ok := posInt(field(plw, 1)); ok {
				if field(plw, 2) == "catalogue-bundled" && !isProvider {
					refresh = true
				}
				if needsAdapter || isProvider || field(plw, 2) == "conf-manual" {
					pwin = field(plw, 1)
				}
			}
		}
		pseed := ""
		if pmodelEnv != "" {
			pseed = pwin
		}
		pceil = field(plw, 5)
		if digits.MatchString(pseed) {
			s, _ := posInt(pseed)
			c, _ := posInt(pceil)
			if s > c {
				pceil = pseed
			}
		}
		if _, ok := posInt(pceil); ok {
			penv = append(penv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+pceil)
			l.log("window ceiling profile=" + name + " tokens=" + pceil)
		}
		if _, ok := posInt(pwin); ok {
			if !envHas(penv, "CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
				derived = l.clampACW(pwin)
			}
			l.log("window derived profile=" + name + " model=" + pmodel + " tokens=" + pwin)
		} else if (needsAdapter || isProvider) && pmodel != "" {
			if !isProvider && pbase != "" {
				refresh = true
			}
			pdef := ""
			if field(plw, 1) == "" && digits.MatchString(field(plw, 3)) {
				pdef = field(plw, 3)
			}
			if pdef != "" && !envHas(penv, "CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
				derived = l.clampACW(pdef)
				l.log("window fallback profile=" + name + " model=" + pmodel + " tokens=" + pdef + " (unprobed)")
			}
			fb := pdef
			if fb == "" {
				fb = "none"
			}
			l.log("window unknown profile=" + name + " model=" + strings.SplitN(pmodel, "[", 2)[0] + " fallback=" + fb)
		}
	}
	if pceil == "" {
		for _, e := range penv {
			if v, ok := strings.CutPrefix(e, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="); ok {
				pceil = l.engineInt(v)
			}
		}
	}
	saved := ""
	if plwRan {
		saved = field(plw, 6)
	} else {
		saved = field(l.launchWindow(name, ""), 6)
	}
	envACW, _ := envGet(penv, "CLAUDE_CODE_AUTO_COMPACT_WINDOW")
	userACW, _ := l.util(nil, "", "effective-acw", envACW, saved, "")
	pacw := userACW
	if pacw == "" {
		pacw = derived
	}

	var defaults, routeKeys []string
	if needsAdapter {
		defaults = routeDefaults
	}
	for _, d := range defaults {
		if k := strings.SplitN(d, "=", 2)[0]; !envHas(penv, k) {
			routeKeys = append(routeKeys, k)
		}
	}
	pct, maxout, no1m := l.triggerEnv(penv)
	pctInEnv := envHas(penv, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE")
	hdrKey := ""
	if envHas(penv, "ANTHROPIC_CUSTOM_HEADERS") {
		hdrKey = "ANTHROPIC_CUSTOM_HEADERS"
	}
	policyPct := ""
	if _, err := os.Stat(l.policy); err != nil {
		policyPct = "75"
	}
	settings := filepath.Join(l.profileDirs, name, "settings.json")
	if fi, err := os.Lstat(settings); err != nil || !fi.Mode().IsRegular() || !isFile(l.conf(name)) {
		settings = ""
	}
	diagArgs := append(append(append([]string{"launch-diagnostics"}, routeKeys...), l.tuning...), "--")
	diagArgs = append(diagArgs, l.settingsSources(name)...)
	inEnv := "0"
	if pctInEnv {
		inEnv = "1"
	}
	diag, _ := l.utilQ([]string{"_CCM_SETTINGS=" + settings, "_CCM_PROFILE_CONF=" + l.conf(name),
		"_CCM_PCT_IN_ENV=" + inEnv, "_CCM_ENV_PCT=" + pct, "_CCM_MAXOUT=" + maxout, "_CCM_NO1M=" + no1m,
		"_CCM_BUDGET=" + pacw, "_CCM_POLICY=" + l.policy, "_CCM_POLICY_PCT=" + policyPct, "_CCM_PROFILE=" + name,
		"_CCM_HDR_KEY=" + hdrKey, "_CCM_HAZARD=1", "_CCM_CEILING=" + pceil, "_CCM_WITHHELD=" + withheld()}, "", diagArgs...)
	pctSet, hazardDone := false, false
	hazardMsg, userHdrs := "", ""
	l.chosen = map[string]bool{}
	var invariant []string
	for _, rec := range strings.Split(diag, "\x00") {
		kind, text, _ := strings.Cut(rec, "\t")
		switch {
		case rec == "pct-set":
			pctSet = true
		case kind == "policy":
			policyPct = text
		case kind == "chosen":
			l.chosen[text] = true
		case kind == "userset":
			userHdrs = text
		case kind == "hazard":
			hazardDone, hazardMsg = true, text
		case kind == "disguised" || kind == "unsafe":
			invariant = append(invariant, rec)
		}
	}
	if !pctInEnv && !pctSet {
		if policyPct == "" {
			var ok bool
			if policyPct, ok = l.compactionPercent(); !ok {
				return nil, false
			}
		}
		penv = append(penv, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE="+policyPct)
	}
	for _, d := range defaults {
		k := strings.SplitN(d, "=", 2)[0]
		if !envHas(penv, k) && !l.chosen[k] {
			penv = append(penv, d)
		}
	}
	if userHdrs != "" {
		for i, e := range penv {
			if v, ok := strings.CutPrefix(e, "ANTHROPIC_CUSTOM_HEADERS="); ok {
				penv[i] = "ANTHROPIC_CUSTOM_HEADERS=" + userHdrs + "\n" + v
			}
		}
	}
	var advisories []string
	if hazardDone {
		if hazardMsg != "" {
			advisories = append(advisories, hazardMsg)
		}
	} else if rows := l.hazardSet(name, pacw, pceil, "", penv); rows != "" {
		if msg, rc := l.utilQ([]string{"_CCM_ROWS=" + rows, "_CCM_WITHHELD=" + withheld()}, "", "hazard-msg"); rc == 0 && msg != "" {
			advisories = append(advisories, msg)
		}
	}
	for _, rec := range invariant {
		kind, text, _ := strings.Cut(rec, "\t")
		if kind == "disguised" {
			l.err("warning: '" + name + "' is on a disguised model id, which caps the window at 200,000 — select " + text + " for the full window")
			continue
		}
		for _, line := range strings.Split(text, "\n") {
			if line != "" {
				l.err("warning: '" + name + "' offers a model that will refuse before it compacts — " + line)
			}
		}
	}

	launchEnv := [][2]string{}
	if launchModel != "" {
		launchEnv = append(launchEnv, [2]string{"ANTHROPIC_MODEL", launchModel})
	}
	acwArg := []string{"--acw", derived}
	if derived == "" {
		acwArg[1] = "none"
	}
	if explicit != "" || resumeCarried {
		acwArg = nil
		if derived != "" && userACW == "" {
			launchEnv = append(launchEnv, [2]string{"CLAUDE_CODE_AUTO_COMPACT_WINDOW", derived})
		}
	}
	if len(advisories) > 0 {
		adv, _ := l.util([]string{"_CCM_ADVISE=ccmorph: " + strings.Join(advisories, "; ")}, "", "advise-json")
		launchEnv = append(launchEnv, [2]string{"CCMORPH_ADVISE", adv})
	}
	for _, kv := range launchEnv {
		if !contains(l.launchEnv, kv[0]) {
			l.err("undeclared launch key " + kv[0])
			return nil, false
		}
		penv = append(envDrop(penv, kv[0]), kv[0]+"="+kv[1])
	}

	var rc int
	if !subcommand {
		rc = l.prepareAndProject(name, acwArg, penv)
		if rc == 10 {
			return nil, false
		}
	} else {
		saved, src := l.configStore("model-settings", name, "")
		if src != 0 {
			return nil, false
		}
		rc = l.projectSettings(name, saved, acwArg, penv)
	}
	if rc != 0 {
		if rc == 3 {
			l.err("'" + name + "' will not start without its settings policy (" + filepath.Join(l.profileDirs, name, "settings.policy.json") + ")")
			return nil, false
		}
		l.err("warning: '" + name + "' starts with its previously generated settings")
	}
	if os.Getenv("CCMORPH_WITHHOLD_FATAL") == "1" {
		gen := filepath.Join(l.profileDirs, name, "settings.json")
		if isFile(gen) {
			if rows := l.hazardSet(name, pacw, pceil, gen, penv); rows != "" {
				if lines := strings.Split(rows, "\n")[1:]; len(lines) > 0 {
					l.utilQ([]string{"_CCM_SETTINGS=" + gen, "_CCM_ROWS=" + strings.Join(lines, "\n"), "_CCM_LAUNCHED=" + pmodel}, "", "withhold-fatal")
				}
			}
		}
	}
	if refresh {
		l.probeNudge(name)
	}
	if (needsAdapter || isProvider) && isTTY(2) {
		os.Stderr.WriteString("ccmorph: Gateway ready; starting Claude Code…\n")
	}
	return penv, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// projectionArgs is _ccm_projection_args: the launch's options, then a --strip
// for every key the launch env sets.
func projectionArgs(opts, penv []string) []string {
	out := append([]string{}, opts...)
	for _, e := range penv {
		if k, _, ok := strings.Cut(e, "="); ok {
			out = append(out, "--strip", k)
		}
	}
	return out
}

func (l *launcher) prepareAndProject(name string, acwArg, penv []string) int {
	argv := l.settingsArgv("materialize", name, projectionArgs(acwArg, penv)...)
	for _, item := range argv {
		if strings.Contains(item, "\n") {
			saved, rc := l.configStore("prepare-launch", name, "", "_CCM_MATERIALIZE=")
			if rc != 0 {
				return 10
			}
			return l.projectSettings(name, saved, acwArg, penv)
		}
	}
	_, rc := l.configStore("prepare-launch", name, "", "_CCM_MATERIALIZE="+strings.Join(argv, "\n"))
	switch rc {
	case 0:
		return 0
	case 11, 12, 13:
		l.err("could not generate settings for '" + name + "'")
		return rc - 10
	}
	return 10
}

func (l *launcher) projectSettings(name, saved string, acwArg, penv []string) int {
	argv := l.settingsArgv("materialize", name, append([]string{"--saved", saved}, projectionArgs(acwArg, penv)...)...)
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), append([]string{"settings-compose"}, argv...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		l.err("could not generate settings for '" + name + "'")
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() > 0 {
			return ee.ExitCode()
		}
		return 1
	}
	return 0
}

// probeNudge sizes the profile's unsized models in the background, logged.
func (l *launcher) probeNudge(name string) {
	bin := filepath.Join(l.libexec, "ccmorph-core")
	if fi, err := os.Stat(bin); err != nil || fi.Mode().Perm()&0o111 == 0 {
		return
	}
	l.logInit()
	f, err := os.OpenFile(l.logOrNull(), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	cmd := exec.Command(bin, "context-probe", "--declared-only", "--profile", name, "--deadline", "30")
	cmd.Stdout, cmd.Stderr = f, f
	if cmd.Start() == nil {
		go cmd.Wait()
	}
	f.Close()
}
