package cli

import (
	"fmt"
	"strconv"

	"github.com/akleizor/ccmorph/util"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

var goos = runtime.GOOS

var routeDefaults = []string{
	"CLAUDE_CODE_MODEL_CAPABILITIES=-mid_conv_system,-mid_conv_tool_change,-per_turn_effort",
	"CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1",
	"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT=1",
}

func engineInt(v string) string {
	if n, ok := util.EngineInt(v); ok {
		return strconv.FormatInt(n, 10)
	}
	return ""
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
			maxout = engineInt(v)
		case "CLAUDE_CODE_DISABLE_1M_CONTEXT":
			no1m = v
		}
	}
	if no1m != "" {
		raw := no1m
		no1m = ""
		if util.EngineBool(raw) {
			no1m = "1"
		}
	}
	return
}

func (l *launcher) compactionPercent() (string, bool) {
	if _, err := os.Stat(l.policy); err != nil {
		return "75", true
	}
	n, ok := util.PolicyPct(l.policy)
	if !ok {
		l.err("invalid fleet context policy: " + l.policy)
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}

func (l *launcher) hazardSet(name, budget, ceiling, settings string, penv []string) []string {
	sources := []string{settings}
	if settings == "" {
		sources = l.settingsSources(name)
	}
	any := false
	for _, s := range sources {
		any = any || isFile(s)
	}
	if !any || !isFile(l.conf(name)) {
		return nil
	}
	pct, maxout, no1m := l.triggerEnv(penv)
	rows, err := util.HazardSet(util.HazardIn{Profile: name, Budget: budget, Ceiling: ceiling, Sources: sources,
		PctRaw: pct, RawOut: strings.TrimSpace(maxout), No1m: no1m != "", Conf: l.conf(name),
		PctInEnv: envHas(penv, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return nil
	}
	return rows
}

func withheld() string {
	if v := os.Getenv("CCMORPH_WITHHOLD_FATAL"); v != "" {
		return v
	}
	return "0"
}

// claudeID is the prefix test the window chain turns on (ARCHITECTURE.md §5):
// a `claude-` id is one Claude Code resolves from its own catalogue, the one
// kind CLAUDE_CODE_MAX_CONTEXT_TOKENS never binds. Launch's derivation blanks
// it, and the context read and inspect's window row ask the same (#447), so
// every reader of the var answers the window the session really runs at.
func claudeID(model string) bool { return strings.HasPrefix(model, "claude-") }

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
		if claudeID(pmodelEnv) {
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
		// A non-claude id gets its ceiling from this env alone, so a sized launch model
		// carries its own window: the route's largest would let the user's
		// autoCompactWindow and percentage trigger past a smaller model's wall. A switch
		// up mid-session then runs capped at this window, which wastes room but never
		// fails. An unsized model keeps the route ceiling and its conservative budget.
		pceil = field(plw, 5)
		if digits.MatchString(pseed) {
			pceil = pseed
		}
		if _, ok := posInt(pceil); ok {
			penv = append(penv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+pceil)
			l.log("window ceiling profile=" + name + " tokens=" + pceil)
		}
		if _, ok := posInt(pwin); ok {
			if !envHas(penv, "CLAUDE_CODE_AUTO_COMPACT_WINDOW") {
				derived = util.ClampACW(pwin)
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
				derived = util.ClampACW(pdef)
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
				pceil = engineInt(v)
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
	userACW := util.EffectiveACW(envACW, saved, "")
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
	diag := l.diagnostics(name, pctInEnv, pct, maxout, no1m, pacw, policyPct, hdrKey, true, pceil, routeKeys)
	pctSet, hazardDone := false, false
	hazardMsg, userHdrs := "", ""
	l.chosen = map[string]bool{}
	var invariant []util.Record
	for _, rec := range diag {
		switch rec.Kind {
		case "pct-set":
			pctSet = true
		case "policy":
			policyPct = rec.Text
		case "chosen":
			l.chosen[rec.Text] = true
		case "userset":
			userHdrs = rec.Text
		case "hazard":
			hazardDone, hazardMsg = true, rec.Text
		case "disguised", "unsafe":
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
	} else if rows := l.hazardSet(name, pacw, pceil, "", penv); len(rows) > 0 {
		if msg := util.HazardMessage(rows, withheld() == "1"); msg != "" {
			advisories = append(advisories, msg)
		}
	}
	l.reportInvariant(name, invariant)

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
		launchEnv = append(launchEnv, [2]string{"CCMORPH_ADVISE", util.Advise("ccmorph: " + strings.Join(advisories, "; "))})
	}
	for _, kv := range launchEnv {
		if !slices.Contains(l.launchEnv, kv[0]) {
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
			if rows := l.hazardSet(name, pacw, pceil, gen, penv); len(rows) > 1 {
				util.WithholdFatal(gen, rows[1:], pmodel)
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
			saved, rc := l.configStore("prepare-launch", name, "")
			if rc != 0 {
				return 10
			}
			return l.projectSettings(name, saved, acwArg, penv)
		}
	}
	prepare := l.storeArgs("prepare-launch", name, "")
	prepare.Materialize = strings.Join(argv, "\n")
	_, rc := l.runStore(prepare, os.Stderr)
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

// diagnostics answers every read-only question a launch asks of the merged
// settings layers and the profile's files in one core call: NUL-framed
// `<kind>\t<text>` records. The invariant half reads only a regular
// settings.json beside an existing conf.
func (l *launcher) diagnostics(name string, pctInEnv bool, pct, maxout, no1m, budget, policyPct, hdrKey string, hazard bool, ceiling string, routeKeys []string) []util.Record {
	settings := filepath.Join(l.profileDirs, name, "settings.json")
	if fi, err := os.Lstat(settings); err != nil || !fi.Mode().IsRegular() || !isFile(l.conf(name)) {
		settings = ""
	}
	return util.LaunchDiagnostics(util.DiagIn{
		Keys: append(append([]string{}, routeKeys...), l.tuning...), Paths: l.settingsSources(name),
		Settings: settings, ProfileConf: l.conf(name), Profile: name, EnvPct: pct, MaxOut: maxout, Budget: budget,
		Policy: l.policy, PolicyPct: policyPct, HdrKey: hdrKey, Ceiling: ceiling,
		PctInEnv: pctInEnv, No1m: no1m != "", Hazard: hazard, Withheld: withheld() == "1"})
}

// reportInvariant prints the disguised-id and refuse-before-compaction warnings,
// in the order the diagnostics produced them. A warning, never a refusal.
func (l *launcher) reportInvariant(name string, records []util.Record) {
	for _, rec := range records {
		kind, text := rec.Kind, rec.Text
		switch kind {
		case "disguised":
			l.err("warning: '" + name + "' is on a disguised model id, which caps the window at 200,000 — select " + text + " for the full window")
		case "unsafe":
			for _, line := range strings.Split(text, "\n") {
				if line != "" {
					l.err("warning: '" + name + "' offers a model that will refuse before it compacts — " + line)
				}
			}
		}
	}
}
