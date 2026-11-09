package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util"
)

var nativeModelFlags = map[string]bool{
	"--model": true, "--effort": true, "--dangerously-skip-permissions": true, "-r": true, "--resume": true,
	"-c": true, "--continue": true, "--from-pr": true, "-p": true, "--print": true,
	"-h": true, "--help": true, "-v": true, "--version": true, "--bg": true, "--background": true,
}

func runFlag(a string) bool {
	return a == "-a" || a == "--profile" || strings.HasPrefix(a, "--profile=") || a == "-i" || a == "--interactive"
}

// launch is one pass of _ccm_launch: nil when the run is over (rc is its status),
// or the relaunch a hop asked for.
func (l *launcher) launch(name string, interactive, native bool, argv []string) (*relayNext, int) {
	// One /v1/models answer per pass: the zsh driver re-sourced ccmorph.sh between
	// hops, which reset _ccm_models_memo, so a catalogue that changed since the
	// previous pass reaches the next pass's picker and model checks.
	l.memo = map[string]string{}
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
		var table map[string]string
		if need {
			table = l.claudeArity()
		}
		parsed, err := util.ParseLaunchArgv(argv, table)
		if err != nil {
			l.err(err.Error())
			return nil, 2
		}
		if parsed.Profile != "" {
			name = parsed.Profile
		}
		interactive = interactive || parsed.Interactive
		args = parsed.Forward
	}

	if interactive && name == "" {
		picked, ok := l.menuProfile()
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
	typ, usageRoute := "", ""
	l.usageHook = ""
	routed, isProvider, needsAdapter := false, false, false
	if name != "" {
		if !l.exists(name) {
			l.noProfile(name)
			return nil, 1
		}
		f, ok := l.read(name)
		if !ok {
			return nil, 1
		}
		typ, usageRoute = f["type"], f["usage_route"]
		l.usageHook = l.usageHookFor(name, typ, usageRoute)
		if pricing := usagePricing(typ, usageRoute); pricing != "" {
			// A turn's row is priced from the models.dev slice on disk; keep it
			// fresh in the background, never on the turn's own path.
			refreshOwnedPrices(pricing)
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
		// The launch is the last moment to say what the masking setting does
		// before a session sends its first secret (#293). needsAdapter is the
		// route the shim fronts, and the shim is what masks.
		if on, _ := util.MaskOn(f["env.CCMORPH_MASK"]); on {
			if needsAdapter {
				fmt.Fprintf(os.Stderr, "%sccmorph: masking on — secrets reach the endpoint as placeholders%s\n", l.dim, l.reset)
			} else {
				fmt.Fprintf(os.Stderr, "%sccmorph: env.CCMORPH_MASK=%s is set, but nothing is masked on this route — it reaches its backend without ccmorph's shim%s\n",
					l.dim, f["env.CCMORPH_MASK"], l.reset)
			}
		}
		var rc int
		if defaultModel, rc = l.configStore("seed-model-defaults", name, "model"); rc != 0 {
			return nil, 1
		}
	}

	subcommand := os.Getenv("CCMORPH_SUBCOMMAND_LAUNCH") == "1"
	var table map[string]string
	for _, a := range args {
		if a == "--" {
			break
		}
		if len(a) > 1 && a[0] == '-' && !nativeModelFlags[a] && !strings.HasPrefix(a, "--model=") &&
			!strings.HasPrefix(a, "--effort=") && !strings.HasPrefix(a, "--resume=") && !strings.HasPrefix(a, "--from-pr=") && !strings.HasPrefix(a, "--print=") {
			table = l.claudeArity()
			break
		}
	}
	flags, err := util.ParseLaunchFlags(routed, args, table)
	if err != nil {
		l.err(err.Error())
		return nil, 2
	}
	explicit, resume, launchModel := flags.Model, flags.Resume, ""
	userModel, skipModel, askMissing, resumeCarried := flags.UserModel, flags.SkipModel, flags.AskMissingModel, false
	relayArgs := flags.Relay
	interactive = interactive || flags.Interactive
	for _, d := range flags.Drop {
		if d >= 1 && d <= len(args) {
			end := min(d+1, len(args))
			args = append(args[:d-1:d-1], args[end:]...)
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
		picked, ok := l.pickValue("model", name)
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
	// What the person asked to run, before a send id or a saved default replaces
	// it: the unserved-id check (#267) judges the request, not the derived id.
	askedModel := explicit
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
	util.GCStale(l.dir, l.profileDirs, tmpDir())
	if err := os.Remove(relayFile); err != nil && !os.IsNotExist(err) {
		return nil, 1
	}
	l.launched = name
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
		// A named profile runs on its own route and credential: nothing the Remote
		// Control edge set up for its own route reaches it, or its adapter, or claude.
		if l.inEdge() {
			for _, k := range edgeEnv {
				os.Unsetenv(k)
			}
		}
		penv, ok := l.profileEnvironment(name)
		if !ok {
			return fail()
		}
		if !isProvider {
			if k, ok := envGet(penv, "ANTHROPIC_API_KEY"); ok && k != "" {
				// Claude Code asks whether to use ANTHROPIC_API_KEY on a profile's first
				// launch and highlights No; ccmorph records the answer before it starts
				// (#236). A key that was refused earlier stops the launch here, before
				// any request, with the one command that records a new answer.
				if !l.keyApproval(name, k) {
					return fail()
				}
				// A native endpoint has no model picker, so nothing asked Anthropic
				// whether the key works before Claude Code opened a session that dies
				// on its first message (#255). Ask the way doctor does — only a
				// rejection stops the launch, so a network that stays silent or
				// answers anything else keeps out of the way — with the wording a
				// gateway profile already gets. A launch that opens no session — a
				// Claude subcommand, or its version or help printout, told from a
				// flag-shaped option value by the same arity-aware walk the flags
				// get (#334) — passes its arguments through untouched (INTENT
				// promise 6) and has no first message for a rejected key to kill,
				// so it keeps out of the way too.
				if !needsAdapter && !subcommand && !util.OpensNoSession(args, table) {
					base, _ := envGet(penv, "ANTHROPIC_BASE_URL")
					if base == "" {
						base = "https://api.anthropic.com"
					}
					if _, code := util.ProbeListing(base, k, "anthropic-version: 2023-06-01"); code == "401" || code == "403" {
						rejected, _ := strconv.Atoi(code)
						l.err(listingFailure(name, profile.RedactURL(base), rejected))
						return fail()
					}
				}
			}
		}
		l.foldSettings(name)
		l.stopForeignBridge(name, isProvider)
		if isProvider {
			if l.providerActive != name {
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
		// A gateway that answers its listing knows what it serves. Refusing the
		// request here keeps the failure before Claude opens, where the first
		// message would otherwise fail with the gateway's own confusing words
		// (#267). A --model given to `ccmorph run` is judged with or without
		// --resume. `_ccm_launch -a ... --resume` is the relaunch entry point other
		// tools call, whose unserved id still starts Claude Code, and an in-process
		// hop already judged its model in cli/hop.go. A subcommand's --model belongs
		// to claude's own verb.
		if askedModel != "" && needsAdapter && !subcommand && (resume == "" || native) {
			if cf, ok := l.readQuiet(name); ok {
				if listing, ok := l.listing(base, key); ok {
					if msg, refuse := util.ServedRefusal(listing, askedModel, cf["model_map"], name); refuse {
						l.err(msg)
						return fail()
					}
				}
			}
		}
		if listing, ok := l.listing(base, key); ok {
			if unsized, ok := try(l, "listing-has-unsized", func() bool { return util.ListingHasUnsized(l.conf(name), listing) }); !ok || unsized {
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
				model, ok := l.adapterModel(name, penv, explicit, defaultModel)
				if !ok {
					return nil, 1
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
		// The last line before claude starts: the person's first-run answers are
		// carried into this profile (#258), and on a hop that crossed profiles,
		// what could not be carried names the safe answer to the question that
		// still meets the resumed session. Seeding can lose the live lock, so the
		// re-check inside personAnswers gates the start (#185, #205).
		if !l.personAnswers(name, l.relayCrossed) {
			return fail()
		}
		// The saved effort must be the level the session starts at (#356), not a
		// settings key Claude Code ignores for a model it knows: a launch that
		// opens a new conversation carries the profile's own level on the argv.
		// The person's typed --effort already decided, a resume keeps the effort
		// of the conversation it resumes (a hop hands its own over), and a
		// subcommand or a print-and-exit run keeps its arguments untouched
		// (INTENT promise 6). The model whose row decides is the one the session
		// runs at — the model the person typed or picked, send-derived when the
		// route rewrote it, else the saved default the launch carries: a --model
		// leaves launchModel empty (INTENT promise 2). The installed Claude Code
		// must document the flag: a key it ignores is still better than an argv
		// it rejects — so its help is consulted only once a level is actually
		// there to pass.
		effortModel := explicit
		if effortModel == "" {
			effortModel = launchModel
		}
		if !flags.UserEffort && !flags.Resuming && !subcommand && !util.OpensNoSession(args, table) {
			if lvl := l.savedEffort(name, effortModel); lvl != "" {
				if table == nil {
					table = l.claudeArity()
				}
				if table["--effort"] == "value" {
					args = insertOption(args, "--effort", lvl)
				}
			}
		}
		rcClaude = l.runClaude(env, args)
		if l.forwardedSig() != 0 {
			os.Remove(relayFile)
			l.log(fmt.Sprintf("signal %d forwarded to claude; no relay", l.forwardedSig()))
			return nil, rcClaude
		}
		l.log(fmt.Sprintf("exit rc=%d profile=%s relay_file_exists=%s", rcClaude, name, yesNo(isFile(relayFile))))
	} else {
		edge := l.nestedEdgeEnv("CLAUDE_CODE_DISABLE_ADVISOR_TOOL", "CLAUDE_CODE_MODEL_CAPABILITIES")
		l.stopForeignBridge("", false)
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

	rs, err := util.ReadRelay(relayFile)
	if err == util.ErrNoRelay {
		l.log("normal exit (no relay)")
		if name != "" {
			l.resumeHint(name)
		}
		return nil, rcClaude
	} else if err != nil {
		l.err(err.Error())
		return nil, 1
	}
	to := rs.Profile
	if !profile.ValidName(to) || !l.exists(to) || rs.Session == "" {
		l.err("invalid relay signal")
		return nil, 1
	}
	f, ok := l.read(to)
	if !ok {
		return nil, 1
	}
	if !l.setupDir(to, f["type"], liveRefuse) {
		return nil, 1
	}
	next := &relayNext{args: append([]string{}, relayArgs...), profile: to, session: rs.Session}
	if rs.Model != "" {
		next.args = append(next.args, "--model", rs.Model)
	}
	if rs.Effort != "" {
		next.args = append(next.args, "--effort", rs.Effort)
	}
	// Claude Code cannot resume a session whose transcript holds no conversation
	// yet: a hop before the first message would hand it an empty id and end at
	// Claude Code's own "No conversation found" (#288). Start the target on a
	// new conversation instead.
	if transcript, ok := l.sessionFile(rs.Session); ok && util.HasConversation(transcript) {
		next.args = append(next.args, "--resume", rs.Session)
	} else {
		next.fresh = true
	}
	next.crossed = name == "" || !sameFile(l.conf(name), l.conf(to))
	return next, 0
}

// savedEffort is the effort level a new session on this profile starts at:
// the per-model override for the model it runs at — Claude Code consults those
// before the top-level default, so the launch does too — else the profile's
// own default. Empty when neither is saved (#356).
func (l *launcher) savedEffort(name, model string) string {
	if model != "" {
		if v, rc := l.configStore("get", name, "effort."+model); rc == 0 && v != "" {
			return v
		}
	}
	v, _ := l.configStore("get", name, "effort")
	return v
}

// insertOption puts an option and its value on claude's argv before the `--`
// that would end its options, else at the end.
func insertOption(args []string, opt, value string) []string {
	at := len(args)
	if i := slices.Index(args, "--"); i >= 0 {
		at = i
	}
	out := make([]string, 0, len(args)+2)
	out = append(out, args[:at]...)
	out = append(out, opt, value)
	return append(out, args[at:]...)
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

// readEndedSession reads the session id Claude Code's SessionEnd hook recorded
// for the claude that ran as pid, and consumes the record: the hint prints
// once, and a launcher that died leaves reaping to gc-stale, never a re-read.
// The hook writes only for a session a user attended on a profile; a private
// session's claude is a grandchild with another pid, and its transcripts die
// with its view, so nothing of it is ever read here. The file sits in a shared
// TMPDIR anyone can write, so a record that is not a session id reads as no
// session rather than reaching the terminal (#285).
func readEndedSession(pid int) string {
	f := filepath.Join(tmpDir(), "ccmorph-session-end."+strconv.Itoa(pid))
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	os.Remove(f)
	sid := strings.TrimSpace(string(b))
	if !sessionID.MatchString(sid) {
		return ""
	}
	return sid
}

// readEndedTranscript reads the transcript path Claude Code gave the SessionEnd
// hook of the claude that ran as pid — the hook keeps Claude Code's JSON input
// beside the id record — and consumes the file, as readEndedSession does. It
// names where that session's conversation is, or would be had a message been
// sent. "" when there is no input, or it is not Claude Code's JSON (#359).
func readEndedTranscript(pid int) string {
	f := filepath.Join(tmpDir(), "ccmorph-session-end."+strconv.Itoa(pid)+".json")
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	os.Remove(f)
	var in struct {
		TranscriptPath string `json:"transcript_path"`
	}
	if json.Unmarshal(b, &in) != nil {
		return ""
	}
	return in.TranscriptPath
}

// resumeHint prints the command that resumes the session Claude Code just
// ended on the profile it ran under (#285): Claude Code's own exit line names
// a bare `claude --resume <id>`, which reopens the default config folder --
// first-run screens, another account — instead of this profile. After a hop
// the final pass prints its own profile, so the line names where the session
// now runs. A session that recorded nothing — a print run, a background run --
// prints nothing, keeping captured output clean; so does one whose transcript,
// the path Claude Code gave its SessionEnd hook, holds no conversation Claude
// Code can resume — a session ended before its first message, or the id of a
// resume that found none — whose command would only fail with Claude Code's
// own "No conversation found" and be offered again by the failed run (#359).
// A record that names no transcript says nothing against the session, so it
// keeps its line. The relay applies the same check before it hands an id to
// --resume (#288).
func (l *launcher) resumeHint(name string) {
	sid, transcript := l.ended, l.endedTranscript
	l.ended, l.endedTranscript = "", ""
	if sid == "" || name == "" {
		return
	}
	if transcript != "" && !util.HasConversation(transcript) {
		return
	}
	fmt.Printf("\n%sccmorph:%s resume this session with: %sccmorph run %s --resume %s%s\n",
		l.accent, l.reset, l.bold, name, sid, l.reset)
}

// sessionFile is _ccm_session_file: the physical transcript of a session, from
// native Claude Code's history or any profile's.
func (l *launcher) sessionFile(id string) (string, bool) {
	if !sessionID.MatchString(id) {
		return "", false
	}
	stores := []string{filepath.Join(os.Getenv("HOME"), ".claude", "projects")}
	more, _ := filepath.Glob(filepath.Join(globQuote(l.profileDirs), "*", "projects"))
	for _, store := range append(stores, more...) {
		hits, _ := filepath.Glob(filepath.Join(globQuote(store), "*", id+".jsonl"))
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
	m, ok := util.ResumeModel(transcript)
	if !ok || m == "" || strings.ContainsAny(m, "\n\r") || strings.ContainsAny(m, " \t\f\v") ||
		strings.HasPrefix(m, "-") || strings.HasPrefix(m, "@") || m == "?" {
		return "", false
	}
	return m, true
}

// carrySession links session <id> into dst so Claude Code can resume it there;
// false when the id is not a session id or no profile holds it.
func (l *launcher) carrySession(id, dst string) bool {
	if !sessionID.MatchString(id) {
		return false
	}
	hits, _ := filepath.Glob(filepath.Join(globQuote(dst), "*", id+".jsonl"))
	for _, h := range hits {
		if isFile(h) {
			return true
		}
	}
	f, ok := l.sessionFile(id)
	if !ok {
		return false
	}
	slug := filepath.Base(filepath.Dir(f))
	if os.MkdirAll(filepath.Join(dst, slug), 0o777) != nil {
		return false
	}
	link := filepath.Join(dst, slug, id+".jsonl")
	os.Remove(link)
	if os.Symlink(f, link) != nil {
		return false
	}
	side := strings.TrimSuffix(f, ".jsonl")
	if os.MkdirAll(side, 0o777) == nil {
		if _, err := os.Lstat(filepath.Join(dst, slug, id)); os.IsNotExist(err) {
			os.Symlink(side, filepath.Join(dst, slug, id))
		}
	}
	return true
}

// migrateState is _ccm_migrate_state: move legacy context state forward once.
func (l *launcher) migrateState() bool {
	if l.migrated || os.Getenv("CCMORPH_CONTEXT_LOCKED") != "" {
		return true
	}
	l.migrated = l.migrateOnce()
	return l.migrated
}

func (l *launcher) migrateOnce() bool {
	legacy := []string{filepath.Join(l.dir, "model-registry"), filepath.Join(l.dir, "model-registry.conf")}
	g1, _ := filepath.Glob(filepath.Join(globQuote(l.profileDirs), "*", "cache", "context-window"))
	g2, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf.lock"))
	legacy = append(append(legacy, g1...), g2...)
	confs, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf"))
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
	if plain && isDarwin() {
		if err := util.KeychainMigrate(l.dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
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
	fields, ok := try(l, "launch-window", func() []string {
		var f []string
		withEnv(l.windowEnv(name), []string{"CLAUDE_CODE_MAX_CONTEXT_TOKENS"}, func() {
			f = util.LaunchWindow(name, model, l.settingsSources(name))
		})
		return f
	})
	if !ok {
		return []string{""}
	}
	return fields
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
	v, ok := util.LaunchListing(base, key, 8)
	v = strings.TrimRight(v, "\n")
	if !ok {
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
	done, ok := try(l, "project-picker", func() bool {
		return util.ProjectPicker(util.PickerIn{Home: home, Conf: l.conf(name), Own: own, Tag: tag, Profile: name,
			Layer: l.profileLayer(name), Launch: launch, ModelsJSON: body}, os.Stderr)
	})
	return ok && done
}

// profileEnvironment is `ccmorph _env`: the profile's launch env, after
// provisioning its config dir and running the host's prelaunch step.
func (l *launcher) profileEnvironment(name string) ([]string, bool) {
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if err != nil {
		msg := err.Error()
		if msg == "" {
			msg = "could not read profile '" + name + "'"
		}
		l.err(msg)
		return nil, false
	}
	if !l.setupDir(name, typ, liveRefuse) {
		return nil, false
	}
	l.prelaunch(name)
	return env, true
}

// livePolicy is what setupDir does when the profile's live lock cannot be
// taken or made (#217). The refusal #205 added belongs to the paths that start
// claude; the verbs that only set state or provision a profile proceed the way
// they did before 2026-11-06.
type livePolicy uint8

const (
	// liveRefuse: a session follows (a run, _env, a hop's relay), and claude
	// must not start through the window the lock covers — refuse (#205).
	liveRefuse livePolicy = iota
	// liveSilent: the verb only sets state (`use`); the lock gates nothing it
	// does, so it proceeds without a word about locks or sessions.
	liveSilent
	// liveNote: the verb provisions a profile (`add`); it proceeds — the
	// profile it just wrote is never rolled back over a lock — and names the
	// real cause, a stuck holder or a lock file that could not be made.
	liveNote
)

// setupDir is _ccm_setup_dir: provision the profile's config dir unless it is.
func (l *launcher) setupDir(name, typ string, policy livePolicy) bool {
	// #205: a hop runs the next profile in this same process. The hopped-away
	// profile's live lock is released here, or another launch of it reads a
	// live session — and defers its merges — for the rest of this one's life.
	if l.liveProfile != "" && l.liveProfile != name {
		util.ReleaseLive(l.profileDirs, l.liveProfile)
	}
	l.liveProfile = name
	// #185: the live lock covers the session from here until claude exits, so
	// a concurrent setup never merges shared history under this launch. #205: a
	// launch that cannot take it must not start claude through the window the
	// lock covers. #217: a verb that starts no session takes it best effort
	// instead — the merge defers to the next launch, as it did before.
	if !l.holdLive(name, policy) {
		return false
	}
	if util.ProfileOK(l.profileDirs, name) {
		return true
	}
	if !util.SetupProfile(l.profileDirs, name, typ, l.log, l.err) {
		return false
	}
	// The merge releases the lock it could not take back when its upgrade
	// failed (#205); the session must still be covered before claude starts.
	// No session follows the other policies, so there is nothing to re-cover —
	// and no second note either (#217).
	if policy == liveRefuse && !l.holdLive(name, policy) {
		return false
	}
	if file := l.profileLayer(name); isFile(file) {
		if fi, err := os.Lstat(file); err == nil && fi.Mode().IsRegular() {
			util.UnwireStatusline(file, filepath.Join(l.dir, "statusline"), filepath.Join(l.libDir, "statusline"))
		}
	}
	return true
}

// holdLive takes the profile's live lock shared and keeps it for this process's
// session, applying policy when it cannot be taken or made. A launch refuses to
// start: claude would run through the window where another launch's merge may
// swap shared history under it, and the nil HoldLive returns for a stuck holder
// used to be discarded (#205) — and a lock file that could not be made is named
// as what it is, where every failure used to be told a holder (#217). `use`
// proceeds silently; `add` proceeds and names the real cause, so the profile it
// just wrote is not rolled back over a lock that gates only its shared history.
func (l *launcher) holdLive(name string, policy livePolicy) bool {
	f, why := util.HoldLive(l.profileDirs, name, l.log)
	if f != nil {
		return true
	}
	if policy == liveSilent {
		return true
	}
	path := util.LiveLockPath(l.profileDirs, name)
	if policy == liveNote {
		if errors.Is(why, util.ErrLiveHeld) {
			l.err("another launch of '" + name + "' is holding its live lock while it links shared history; the shared history links on a later launch")
		} else {
			l.err("the live lock for '" + name + "' could not be created at " + path + " (" + why.Error() + "); remove what blocks the path — the shared history links on a later launch that can make it")
		}
		return true
	}
	if errors.Is(why, util.ErrLiveHeld) {
		l.err("another launch of '" + name + "' is holding its live lock (" + path + ") while it links shared history and has not let go; nothing started. Try again in a moment; if it stays stuck, `lsof " + path + "` names the holder.")
	} else {
		l.err("the live lock for '" + name + "' could not be created at " + path + " (" + why.Error() + "); nothing started. Remove what blocks the path and try again.")
	}
	return false
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
// It reports whether a fold ran: only when settings.json differs from the bytes
// the last projection saved (content, not timestamps; a missing copy folds).
func (l *launcher) foldSettings(name string) bool {
	dir := filepath.Join(l.profileDirs, name)
	s, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		return false
	}
	if base, err := os.ReadFile(filepath.Join(dir, "cache", "settings.base.json")); err == nil &&
		strings.TrimRight(string(s), "\n") == strings.TrimRight(string(base), "\n") {
		return false
	}
	l.core(nil, "", append([]string{"settings-compose"}, l.settingsArgv("fold", name)...)...)
	return true
}

func (l *launcher) settingsArgv(verb, name string, rest ...string) []string {
	argv := []string{verb, filepath.Join(l.profileDirs, name), "--canonical", l.canonical()}
	for _, k := range l.mapping {
		argv = append(argv, "--mapping="+k)
	}
	if l.usageHook != "" {
		argv = append(argv, "--usage-hook="+l.usageHook)
	}
	return append(argv, rest...)
}

func (l *launcher) route(penv []string, port, key string, adapter bool) ([]string, bool) {
	return util.LaunchEnv(penv, port, key, adapter, l.adapterPort, os.Getenv("CCMORPH_TASK"), os.Getenv("CCMORPH_ATTEMPT")), true
}

// usageHookFor is the Stop hook a launch installs to record its own usage. A
// subscription, long-lived token or provider profile never touches its network
// path: Claude Code records each response's usage in its transcript, and this
// hook reads the new lines. It is empty for every other profile type and for
// `usage_route=direct`, which is also how a launch removes a hook an earlier
// one installed. It only builds the command: the price refresh is the launch's
// (usagePricing plus refreshOwnedPrices), never a caller's side effect. The
// command is frozen into the profile's settings for the sessions that outlive
// this launch, so under Homebrew it names brew's stable opt path, not the
// versioned Cellar dir this process runs from (stableCore, #287); the next
// launch's materialize replaces whatever hook it finds with this one.
func (l *launcher) usageHookFor(name, typ, usageRoute string) string {
	pricing := usagePricing(typ, usageRoute)
	if pricing == "" {
		return ""
	}
	return shQuote(stableCore(l.libexec)) + " usage-row --profile " + shQuote(name) + " --pricing " + shQuote(pricing)
}

// brewCellarRE is Homebrew's versioned install prefix, <prefix>/Cellar/ccmorph/
// <version>: where the running binary sits under brew. An upgrade moves it and
// a cleanup deletes it, so a path frozen under it at launch time is stale
// against the sessions that still run.
var brewCellarRE = regexp.MustCompile(`^(.+)/Cellar/ccmorph/[^/]+(/.*)?$`)

// stableCore is the ccmorph-core a usage hook names. Under Homebrew that is the
// stable <prefix>/opt/ccmorph path, the symlink brew re-points at every
// upgrade, so the hook survives `brew upgrade` and `brew cleanup`; anywhere
// else — a clone, install.sh's config-dir layout — it is the core beside the
// ccmorph that is running, as before. The opt path is taken only when a core
// sits behind it: a Cellar-shaped path without brew's link is not a brew
// install.
func stableCore(libexec string) string {
	core := filepath.Join(libexec, "ccmorph-core")
	m := brewCellarRE.FindStringSubmatch(libexec)
	if m == nil {
		return core
	}
	stable := filepath.Clean(m[1] + "/opt/ccmorph" + m[2])
	if !isFile(filepath.Join(stable, "ccmorph-core")) {
		return core
	}
	return filepath.Join(stable, "ccmorph-core")
}

// usagePricing is the models.dev provider slice a launch's usage rows are priced
// against, or "" when the profile installs no usage hook at all.
func usagePricing(typ, usageRoute string) string {
	if usageRoute == "direct" {
		return ""
	}
	switch typ {
	case "sub", "token":
		return "anthropic"
	case "provider":
		return "any"
	}
	return ""
}

// refreshOwnedPrices keeps the models.dev slice on disk fresh for a later hook.
// The launch calls it in the background, never on a turn's own path; it is a
// variable so a test can prove the hook builder starts no fetch.
var refreshOwnedPrices = func(pricing string) { go shim.RefreshOwnedPrices(pricing) }

var loopbackProxy = regexp.MustCompile(`^http://127\.0\.0\.1:([0-9]+)/?$`)

// edgeEnv is what ccmorph-rc's session_env sets for the edge's route: its proxy and
// CA, the route's thread setting and the record a nested launch reads.
var edgeEnv = []string{"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", "CLAUDE_CODE_TETHER_LIVE", "CCMORPH_EDGE_ROUTE_ENV"}

// loopbackHTTPSProxy: HTTPS_PROXY (or https_proxy) is http://127.0.0.1:<port>.
func loopbackHTTPSProxy() bool {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	m := loopbackProxy.FindStringSubmatch(proxy)
	if m == nil {
		return false
	}
	p, ok := posInt(m[1])
	return ok && p <= 65535
}

// inEdge: this process runs inside a Remote Control bridge session, whose add-on
// sets CCMORPH_EDGE_ROUTE_ENV beside its loopback proxy. A loopback proxy alone may
// be the user's own, which a launch must keep.
func (l *launcher) inEdge() bool {
	return os.Getenv("CCMORPH_EDGE_ROUTE_ENV") != "" && loopbackHTTPSProxy()
}

// nestedEdgeEnv is _ccm_nested_edge_env: inside the Remote Control edge, the
// route keys it named for a native launch.
func (l *launcher) nestedEdgeEnv(keys ...string) []string {
	if !loopbackHTTPSProxy() {
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

// providerStartRefusal is why a profile's bridge cannot start at all, in the
// words its own launch would say; "" when the prerequisites are there. hop asks
// before it ends the session it runs in (#286).
func (l *launcher) providerStartRefusal(name string) string {
	if _, err := exec.LookPath("cliproxyapi"); err != nil {
		return "cliproxyapi not found — install it: brew install cliproxyapi"
	}
	root := filepath.Join(l.dir, "auth")
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "auth directory root is a symlink, refusing: " + root
	}
	auth := filepath.Join(root, name)
	if fi, err := os.Lstat(auth); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "auth directory for '" + name + "' is a symlink, refusing: " + auth
	}
	if fi, err := os.Stat(auth); err != nil || !fi.IsDir() {
		return "no auth directory for '" + name + "' — run: ccmorph login " + name
	}
	return ""
}

// providerStart starts or joins the profile's shared bridge, recording the
// handle every session of the profile shares. A route env joins it too, and a
// holder process keeps the route's use of it until the route retires (#300).
func (l *launcher) providerStart(name string) bool {
	if why := l.providerStartRefusal(name); why != "" {
		l.err(why)
		return false
	}
	bin, _ := exec.LookPath("cliproxyapi")
	root := filepath.Join(l.dir, "auth")
	auth := filepath.Join(root, name)
	l.logInit()
	start := func() (pid, port, key, tmp string, ok bool) {
		cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), "util", "bridge-start", name, bin, auth, l.logOrNull())
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", "", "", "", false
		}
		for _, line := range strings.Split(string(out), "\n") {
			k, v, _ := strings.Cut(line, "=")
			switch k {
			case "PID":
				pid = v
			case "PORT":
				port = v
			case "KEY":
				key = v
			case "TMPDIR":
				tmp = v
			}
		}
		return pid, port, key, tmp, true
	}
	b, ok, err := util.AcquireBridge(root, name, start)
	if err != nil && !errors.Is(err, util.ErrBridgeNotStarted) {
		l.err(err.Error())
	}
	if !ok {
		return false
	}
	l.providerBridge = b
	l.providerPID, l.providerPort, l.providerKey, l.providerTD = b.PID, b.Port, b.Key, b.TmpDir
	l.providerAuth, l.providerActive = auth, name
	return true
}

// stopForeignBridge is the zsh driver's EXIT teardown, once per pass: a pass
// keeps the bridge only for the provider it is about to run, so a hop from a
// provider to any other profile starts its session with the bridge gone.
func (l *launcher) stopForeignBridge(name string, isProvider bool) {
	if !isProvider || l.providerActive != name {
		l.providerStop()
	}
}

// routeHold spawns the holder that owns a route's use of the shared bridge it
// runs on: a `util route-hold` process holding the users lock until the route
// retires it. The route env process exits with its env, so without a holder
// nothing owns the route's use — the last session out, or a sibling route's
// retirement, would stop the bridge under a live route (#300). Ok is false
// when the hold never lands: the route must then refuse, not run unprotected.
func (l *launcher) routeHold(root, name string) (string, bool) {
	users := util.BridgeFiles(root, name)[2]
	pr, pw, err := os.Pipe()
	if err != nil {
		return "", false
	}
	defer pr.Close()
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), "util", "route-hold", users)
	// The holder outlives this process by design, so it keeps none of the
	// caller's streams: a caller capturing _route_env's output (Remote Control,
	// probe) reads until every writer is gone, and an inherited stderr would
	// hold that read open past the env's exit — until a retirement registered
	// only once the read returns (#300). Its one word is the readiness byte on
	// its own pipe; its stdin and stderr are /dev/null.
	cmd.Stdout = pw
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // outlive the env process and its terminal
	if err := cmd.Start(); err != nil {
		pw.Close()
		return "", false
	}
	go cmd.Wait() // reap it; a holder this process leaves behind reparents to launchd
	pw.Close()
	one := make([]byte, 1)
	pr.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(pr, one); err != nil || one[0] != '1' {
		cmd.Process.Kill()
		return "", false
	}
	return strconv.Itoa(cmd.Process.Pid), true
}

func (l *launcher) providerStop() {
	if l.providerBridge != nil {
		l.providerBridge.Release(l.providerAuth)
		l.providerBridge = nil
	} else if l.providerPID+l.providerTD+l.providerAuth != "" {
		// No SharedBridge: the handle came from a route (_route_stop). Retire
		// it — ending the route's hold first — without stopping a bridge a
		// session of the profile still uses (#300).
		util.RetireRoute(l.providerAuth, l.providerPID, l.providerTD, l.providerHolder)
	}
	l.providerPID, l.providerActive, l.providerPort, l.providerKey, l.providerTD, l.providerAuth, l.providerHolder = "", "", "", "", "", "", ""
}

func (l *launcher) logOrNull() string {
	if l.logPath == "" {
		return "/dev/null"
	}
	return l.logPath
}

// routeRefusal is a definite reason the profile's endpoint cannot serve a
// session, in doctor's words; "" when the route answered or has nothing to
// probe. Only sure causes refuse — nothing answered, or the key was rejected —
// because a hop must not strand a session over a guess (#286).
func (l *launcher) routeRefusal(name string) string {
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, func(string) {})
	if err != nil || typ != "apikey" {
		return ""
	}
	base, _ := envGet(env, "ANTHROPIC_BASE_URL")
	if util.NativeBase(base) {
		// A native endpoint's launch now asks Anthropic whether the key works
		// before a session opens (#255), so the hop asks the same question, with
		// the same header, before it ends one (#286). Only a rejection refuses:
		// silence keeps out of the way, exactly as at launch.
		if base == "" {
			base = "https://api.anthropic.com"
		}
		if _, code := util.ProbeListing(base, authKey(env), "anthropic-version: 2023-06-01"); code == "401" || code == "403" {
			return "Anthropic rejected the API key (HTTP " + code + ")"
		}
		return ""
	}
	_, code := util.ProbeListing(base, authKey(env))
	switch {
	case code == "" || code == "000":
		return "the endpoint " + profile.RedactURL(base) + " did not answer"
	case code == "401" || code == "403":
		return "the endpoint rejected the API key (HTTP " + code + ")"
	}
	return ""
}

// adapterModel is the gateway id the adapter serves: the explicit or default
// model, a tier resolved through the profile's mapping or its saved alias.
func (l *launcher) adapterModel(name string, penv []string, explicit, defaultModel string) (string, bool) {
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
				return "", false
			}
			model = v
		}
	}
	return model, true
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
		// Plain words first when the cause is the route itself (#286), then the
		// one command that diagnoses the whole route.
		if why := l.routeRefusal(name); why != "" {
			fmt.Fprintln(os.Stderr, "  "+why+".")
		}
		fmt.Fprintln(os.Stderr, "  Check the route: ccmorph doctor "+name)
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
	envFile, err := l.writeSessionEnv(env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccmorph: "+err.Error())
		return 1
	}
	defer os.Remove(envFile) // the helper removes it on read; this is the backstop
	argv := append([]string{"util", "claude-session", strings.Join(l.tuning, " "), strings.Join(l.profileEnv, " "),
		strings.Join(l.launchEnv, " "), strings.Join(l.backend, " "), envFile}, "--")
	argv = append(argv, args...)
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), argv...)
	// The helper's own key names the run/ root the env file above already used; the
	// private view falls back there when TMPDIR cannot be written (#140). The helper
	// unsets it, so neither claude nor its children ever see it.
	cmd.Env = append(os.Environ(), "CCMORPH_NOTICE="+l.notice, "_CCMORPH_RUN_DIR="+filepath.Join(l.dir, "run"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	l.mu.Lock()
	l.forwarded = 0
	if err := cmd.Start(); err != nil {
		l.mu.Unlock()
		fmt.Fprintln(os.Stderr, "ccmorph: "+err.Error())
		return 127
	}
	l.ranClaude = true
	l.child = cmd.Process
	l.mu.Unlock()
	err = cmd.Wait()
	l.mu.Lock()
	l.child = nil
	l.mu.Unlock()
	l.ended = readEndedSession(cmd.Process.Pid)
	l.endedTranscript = readEndedTranscript(cmd.Process.Pid)
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

// writeEnvFile writes the NUL-joined launch env to a fresh 0600 file in dir
// (dir "" is TMPDIR). A value can span lines (ANTHROPIC_CUSTOM_HEADERS).
func writeEnvFile(dir string, env []string) (string, error) {
	f, err := os.CreateTemp(dir, "ccmorph-session-env.")
	if err != nil {
		return "", err
	}
	for _, e := range env {
		if _, err = f.WriteString(e + "\x00"); err != nil {
			break
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

// writeSessionEnv writes the launch env to a 0600 file in TMPDIR, or under the
// state dir's run/ (0700) when TMPDIR cannot be written: a launch must still
// start, and the env must still stay off the argument vector (#112).
func (l *launcher) writeSessionEnv(env []string) (string, error) {
	path, err := writeEnvFile("", env)
	if err == nil || l.dir == "" {
		return path, err
	}
	run := filepath.Join(l.dir, "run")
	if mkErr := os.MkdirAll(run, 0o700); mkErr != nil {
		return "", err
	}
	return writeEnvFile(run, env)
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
	f, ok := l.readQuiet(name)
	if !ok || profile.Validate(name, f) != nil {
		return 2
	}
	return int(util.ModelServed(want, f["type"], f["base_url"], f["model_map"], f["api_key"]))
}

// listingKnown is modelKnown for a read that promises discovery (`ccmorph
// context`): a key with no Base URL talks to api.anthropic.com, so it asks that
// listing — the one `ccmorph models` reads — instead of the launch check that
// calls every claude-* id served there. 0 served, 1 not served, 2 cannot tell.
func (l *launcher) listingKnown(name, want string) int {
	f, ok := l.readQuiet(name)
	if ok && f["type"] == "apikey" && f["base_url"] == "" && profile.Validate(name, f) == nil {
		return int(util.ModelServed(want, "apikey", "https://api.anthropic.com", "", f["api_key"]))
	}
	return l.modelKnown(name, want)
}
