package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akleizor/ccmorph/router"
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

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/settingscompose"
	"github.com/akleizor/ccmorph/shim"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

var nativeModelFlags = map[string]bool{
	"--model": true, "--effort": true, "--dangerously-skip-permissions": true, "-r": true, "--resume": true,
	"-c": true, "--continue": true, "--from-pr": true, "-p": true, "--print": true,
	"-h": true, "--help": true, "-v": true, "--version": true, "--bg": true, "--background": true,
}

func runFlag(a string) bool {
	return a == "-a" || a == "--profile" || strings.HasPrefix(a, "--profile=") || a == "-i" || a == "--interactive"
}

// probeListing is the launch's key pre-flight probe, a package var so a test can
// answer a listing without a network.
var probeListing = util.ProbeListing

// reachListing is the launch's reachability probe (#260), a package var so a
// test can answer it without a network.
var reachListing = util.ReachListing

// probeStatus is util.ProbeStatus, and probeVerdict is util.ProbeVerdict: the
// one status switch every probe of a credential reads (#488 spelled it once in
// cli; #516 moved it down so doctor's four route checks read the same one).
// These names keep this package's six call sites — and the table test that
// pins the verdict — on that one definition.
type probeStatus = util.ProbeStatus

const (
	probeAccepted = util.ProbeAccepted
	probeRejected = util.ProbeRejected
	probeSilent   = util.ProbeSilent
	probeOther    = util.ProbeOther
)

func probeVerdict(code string) probeStatus { return util.ProbeVerdict(code) }

// nativeBase is the one rule for Anthropic's own endpoint: the URL an empty
// base_url means, and the spellings that count as that endpoint — the bare URL
// and the trailing-slash form, util.NativeBase's list, which doctor and masking
// already read — so the defaults launch, models, quota and config fill in and
// the native tests run and inspect make cannot drift apart (#488). b is the
// URL to talk to, a typed base untouched; native is whether base is
// Anthropic's own.
func nativeBase(base string) (b string, native bool) {
	if base == "" {
		return "https://api.anthropic.com", true
	}
	return base, util.NativeBase(base)
}

// launch is one pass of _ccm_launch: nil when the run is over (rc is its status),
// or the relaunch a hop asked for. Resolving the pass and running it are different
// jobs, so launch only joins them.
func (l *launcher) launch(name string, interactive, native bool, argv []string) (*relayNext, int) {
	// Whatever the pass started, the pass joins before it returns: the sizing
	// probe runs beside the session, but no launch leaves a process behind
	// (#983).
	defer l.awaitProbes()
	plan, rc := l.resolveLaunch(name, interactive, native, argv)
	if plan == nil {
		return nil, rc
	}
	return l.runLaunch(plan)
}

// launchPlan is what resolveLaunch settles before any helper or claude starts: the
// profile this pass runs on, the argv claude receives, and every judgement the
// process half would otherwise repeat. It crosses the seam between the two halves.
type launchPlan struct {
	name       string // the profile this pass runs on, "" for a bare launch
	native     bool   // argv already came from a native re-entry
	args       []string
	arity      map[string]string // the option arity table, nil until something needs it
	flags      util.LaunchFlags
	subcommand bool

	explicit      string // --model as the person typed it
	resume        string // the session id --resume names
	launchModel   string // the model this launch carries; "" when the argv names it
	defaultModel  string // the profile's saved default
	askedModel    string // what the person asked to run, before a send id replaced it
	resumeCarried bool   // the resumed session's model became the default

	needsAdapter bool // the route is a gateway the adapter fronts
	isProvider   bool // the profile's route is a provider bridge
	tag          string

	opensSession bool // the argv opens a session: neither a Claude subcommand nor a print-and-exit printout

	reachWhy string // why the gateway could not be reached, in plain words; "" when it answered or was not asked (#260)

	relayFile string   // the hop relay this pass owns and must clean up
	common    []string // env every pass carries
	penv      []string // the profile's launch env
}

// resolveLaunch is the pre-flight half of one pass: it walks argv, settles the
// profile, model and effort, and probes the route's key. A nil plan ends the pass
// with rc; otherwise runLaunch consumes it.
func (l *launcher) resolveLaunch(name string, interactive, native bool, argv []string) (*launchPlan, int) {
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
			return nil, l.badName("run", name)
		}
	} else if active, ok := l.readActive(); ok {
		name = active
	}

	defaultModel, tag := "", ""
	routeBase, routeKey, routeVia := "", "", "" // a gateway's endpoint, key and profile proxy, for the reach probe (#260)
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
			if _, native := nativeBase(f["base_url"]); !native {
				needsAdapter = true
				routeBase, routeKey = f["base_url"], f["api_key"]
				// The session's own route knob: the adapter's probes and the
				// shim read the conf's spelling alone (adapter environWith).
				routeVia = f["env.CCMORPH_UPSTREAM_PROXY"]
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
		// route the shim fronts, and the shim is what masks — and refuses a
		// value that is neither on nor off, so the notice names a refused value
		// instead of reading it as off (#411).
		if on, bad := util.MaskOn(f["env.CCMORPH_MASK"]); on || bad {
			switch {
			case bad && needsAdapter:
				fmt.Fprintf(os.Stderr, "%sccmorph: env.CCMORPH_MASK=%s is refused (must be 1 or 0) — a launch that starts the shim stops there%s\n",
					l.dim, f["env.CCMORPH_MASK"], l.reset)
			case bad:
				fmt.Fprintf(os.Stderr, "%sccmorph: env.CCMORPH_MASK=%s is not 1 or 0; this route starts no shim, so nothing is masked%s\n",
					l.dim, f["env.CCMORPH_MASK"], l.reset)
			case needsAdapter:
				fmt.Fprintf(os.Stderr, "%sccmorph: masking on — secrets reach the endpoint as placeholders%s\n", l.dim, l.reset)
			default:
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
	interactive = interactive || flags.Interactive
	for _, d := range flags.Drop {
		if d >= 1 && d <= len(args) {
			end := min(d+1, len(args))
			args = append(args[:d-1:d-1], args[end:]...)
		}
	}
	// The one decision every route-facing step reads: does this argv open a
	// session at all? A Claude subcommand or a print-and-exit printout opens
	// none, so it judges no route, starts no route process, and passes its
	// arguments through untouched (INTENT promise 6, #650); the arity-aware
	// walk (#334) tells both from a flag-shaped option value.
	opensSession := !subcommand && !util.OpensNoSession(args, table)

	// A gateway launch asks its endpoint first, bounded the way doctor is, so
	// nothing after it can wait on a gateway in silence (#260). Silence ends
	// the launch here; a cause named at once lets it go on as it always has.
	reachWhy := ""
	if needsAdapter && opensSession && l.adapterProfile != name {
		var down bool
		if reachWhy, down = l.reachGateway(name, routeBase, routeKey, routeVia); down {
			return nil, util.RCGatewayDown
		}
	}
	// The picker could only fail on a gateway just named unreachable, and only
	// a terminal opens it: skipping it keeps the terminal launch on the path
	// the no-terminal one takes, so both end on the same words (#260).
	// A key Claude Code was told to refuse stops the launch at keyApproval; no
	// picker may fetch with that key first and bury the login repair (#862).
	keyRefused := name != "" && routeKey != "" &&
		util.KeyResponse(filepath.Join(l.profileDirs, name), routeKey) == util.KeyRefused
	// A saved alias this gateway maps to no model stops at adapterModel; in a
	// terminal the picker opens instead, saying why, and saves nothing (#862).
	unserved := ""
	if needsAdapter && explicit == "" && defaultModel != "" && l.unmappedTier(name, defaultModel) != "" {
		unserved = defaultModel
	}
	if needsAdapter && askMissing && !subcommand && explicit == "" && (defaultModel == "" || unserved != "") && reachWhy == "" && !keyRefused && isTTY(0) && isTTY(2) {
		if unserved != "" {
			l.err(util.UnservedTier(name, util.TierWord(unserved), l.modelOrigin(name), nil))
		}
		interactive = true
	}
	if interactive && explicit == "" && routed && !keyRefused {
		if name == "" {
			l.err("-i needs an profile: name one, or set a default with ccmorph use <profile>")
			return nil, 2
		}
		// The gateway was just named unreachable and has no list to pick from:
		// the launch ends on that cause and the route check, never a second copy
		// of the sentence and never the saved model -i was asked to replace (#260).
		if reachWhy != "" {
			fmt.Fprintf(os.Stderr, "  Check the route: ccmorph doctor %s\n", name)
			return nil, 1
		}
		keep := defaultModel
		if unserved != "" {
			keep = "" // a Keep row would offer the alias that cannot start (#862)
		}
		picked, ok := l.pickLaunchModel(name, keep, unserved)
		if !ok {
			return nil, 1
		}
		if picked == "" {
			l.err("no model chosen")
			return nil, 1
		}
		explicit, launchModel, skipModel = picked, picked, true
		// The pick answers this launch only; without the keep command the next
		// one asks again, and a person cannot guess why (#360). The id is quoted
		// as a shell would need it: a listing can serve ids like vendor[beta],
		// which a pasted bare word would glob away in zsh. Keeping the saved
		// model is not a pick to keep: the note would send a person to save what
		// is already saved (#377).
		if picked != defaultModel {
			fmt.Fprintf(os.Stderr, "%sccmorph: %s runs this session only — keep it with: ccmorph set %s model %s%s\n",
				l.dim, picked, name, shQuote(picked), l.reset)
		}
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
	fail := func() (*launchPlan, int) { os.Remove(relayFile); return nil, 1 }

	common := []string{
		"CCMORPH_RELAY_ID=" + relayID,
		"CCMORPH_LIB_DIR=" + l.libDir,
		"CLAUDE_CODE_PLUGIN_DIRS=" + pluginDirs(l.libDir),
	}
	plan := &launchPlan{
		name: name, native: native, args: args, arity: table,
		flags: flags, subcommand: subcommand,
		explicit: explicit, resume: resume, launchModel: launchModel,
		defaultModel: defaultModel, askedModel: askedModel, resumeCarried: resumeCarried,
		needsAdapter: needsAdapter, isProvider: isProvider, tag: tag,
		opensSession: opensSession, reachWhy: reachWhy,
		relayFile: relayFile, common: common,
	}
	if name != "" {
		// A named profile runs on its own route and credential: nothing the Remote
		// Control edge set up for its own route reaches it, or its adapter, or claude.
		if l.inEdge() {
			for _, k := range edgeEnv {
				os.Unsetenv(k)
			}
		}
		penv, ok := l.profileEnvironment(name, func(line string) { fmt.Fprintln(os.Stderr, line) })
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
				// on its first message (#255). A gateway's listing is asked anyway —
				// for served ids and context — but with the status thrown away, so a
				// key the gateway rejected read as no listing at all and the launch
				// waited on an adapter whose every request repeated the rejection
				// (#374). Both ask the way doctor does — only a rejection stops the
				// launch, so a network that stays silent or answers anything else
				// keeps out of the way — with the wording a gateway profile already
				// gets. A launch that opens no session — a Claude subcommand, or its
				// version or help printout, told from a flag-shaped option value by
				// the same arity-aware walk the flags get (#334) — passes its
				// arguments through untouched (INTENT promise 6) and has no first
				// message for a rejected key to kill, so it keeps out of the way too.
				if opensSession {
					base, _ := envGet(penv, "ANTHROPIC_BASE_URL")
					if !needsAdapter {
						base, _ = nativeBase(base)
						if _, code := probeListing(base, k, "anthropic-version: 2023-06-01"); probeVerdict(code) == probeRejected {
							rejected, _ := strconv.Atoi(code)
							l.err(listingFailure(name, profile.RedactURL(base), rejected))
							return fail()
						}
					} else {
						// Doctor's own request for a gateway — no anthropic-version
						// header — so what doctor calls a rejection, launch does too.
						body, code := probeListing(base, k)
						switch probeVerdict(code) {
						case probeRejected:
							rejected, _ := strconv.Atoi(code)
							l.err(listingFailure(name, profile.RedactURL(base), rejected))
							return fail()
						case probeAccepted:
							// The listing this pass was about to fetch anyway is
							// already in hand, so the happy path still asks the
							// route once.
							l.memo[base+"\x1f"+k] = strings.TrimRight(body, "\n")
						}
					}
				}
			}
		}
		plan.penv = penv
	}
	return plan, 0
}

// runLaunch is the process half of one pass: it folds the profile's settings,
// starts or stops the adapters and bridges the plan names, runs claude, and reads
// the relay file a hop left behind.
func (l *launcher) runLaunch(p *launchPlan) (*relayNext, int) {
	name, args := p.name, p.args
	table := p.arity
	flags, subcommand := p.flags, p.subcommand
	explicit, resume, launchModel := p.explicit, p.resume, p.launchModel
	defaultModel, askedModel, resumeCarried := p.defaultModel, p.askedModel, p.resumeCarried
	needsAdapter, isProvider, native := p.needsAdapter, p.isProvider, p.native
	opensSession := p.opensSession
	tag, common, relayFile := p.tag, p.common, p.relayFile
	relayArgs := flags.Relay
	fail := func() (*relayNext, int) { os.Remove(relayFile); return nil, 1 }
	var rcClaude int
	if name != "" {
		penv := p.penv
		var ok bool
		l.foldSettings(name)
		l.stopForeignBridge(name, isProvider)
		// A launch that opens no session starts no route process: the bridge and
		// the adapter exist to serve the session, and there is none (#650). Its
		// arguments reach claude on the profile's own environment.
		if isProvider && opensSession {
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
		// to claude's own verb, as does a printout's (#650).
		if askedModel != "" && needsAdapter && opensSession && (resume == "" || native) {
			if cf, ok := l.readQuiet(name); ok {
				if listing, ok := l.listing(base, key); ok {
					if msg, refuse := util.ServedRefusal(listing, askedModel, cf["model_map"], name); refuse {
						l.err(msg)
						return fail()
					}
				}
			}
		}
		// The listing fetch, its unsized probe and the picker all speak to the
		// session about to open: a launch that opens none asks the route nothing,
		// so a dead endpoint cannot stall or refuse it (#650).
		if opensSession {
			if listing, ok := l.listing(base, key); ok {
				if unsized, ok := try(l, "listing-has-unsized", func() bool { return util.ListingHasUnsized(l.conf(name), listing) }); !ok || unsized {
					l.logInit()
					// The sizing save waits on the profile lock, which a person at
					// `ccmorph rm`'s confirm can hold for as long as they think:
					// the deadline keeps that wait from holding the launch with no
					// output (#757). It matches the probe's own default, so the
					// launch ceiling holds even if the verb forgets it.
					l.core([]string{"CCMORPH_STATE_DIR=" + l.dir, "ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_API_KEY=" + key},
						listing+"\n", "context-probe", "--listing-stdin", "--profile", name, "--deadline", "30")
					// The probe's save rewrites a conf that still carries its
					// discovered record inline, so the next read must not be the
					// memoized one (GH-561).
					delete(l.confReads, name)
				}
			}
			switch l.projectPicker(name, base, key, defaultModel, tag, explicit) {
			case pickerUnreached:
				// A gateway that answers nothing empties the picker only for the
				// length of its silence (#723): the listing is waited on, and if the
				// route never speaks the pass ends naming it, in the class a caller
				// can tell from a crash — never with the picker's config message.
				if !l.awaitListing(name, base, key) {
					l.err(listingFailureWhy(name, profile.RedactURL(base), 0, p.reachWhy))
					fmt.Fprintf(os.Stderr, "  Check the route: ccmorph doctor %s\n", name)
					return nil, util.RCGatewayDown
				}
				if l.projectPicker(name, base, key, defaultModel, tag, explicit) != pickerDone {
					return nil, 1
				}
			case pickerRefused:
				return nil, 1
			}
		}
		if needsAdapter && opensSession {
			if l.adapterProfile != name {
				l.adapterStop()
				// No model given or saved, a listing that names no chat model, and
				// no cached protocol: the adapter has nothing to start. Say so in
				// the picker's and doctor's words before it is asked (#862).
				if explicit == "" && defaultModel == "" {
					if listed := l.listed(base, key); util.NoModelToPick(l.conf(name), listed) {
						if _, known := adapter.TranslatorVerdict(base, l.profileDirs, "", listed); !known {
							l.err(util.NoUsableModel(name))
							return nil, 1
						}
					}
				}
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
		} else if !needsAdapter {
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
		// The route stands up beside the session, so only a launch that opens one
		// may say it is ready (#650).
		if opensSession && (needsAdapter || isProvider) && isTTY(2) {
			os.Stderr.WriteString("ccmorph: Gateway ready; starting Claude Code…\n")
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
		if opensSession {
			penv = l.agentProxyEnv(penv)
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
		if !flags.UserEffort && !flags.Resuming && opensSession {
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
		l.log(fmt.Sprintf("exit rc=%d profile=%s relay_file_exists=%s", rcClaude, name, yesNo(paths.IsFile(relayFile))))
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
		l.log(fmt.Sprintf("exit rc=%d profile=<none> relay_file_exists=%s", rcClaude, yesNo(paths.IsFile(relayFile))))
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
	next.crossed = name == "" || !paths.SameFile(l.conf(name), l.conf(to))
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

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// pluginDirs is the session's plugin path: ccmorph's own plugin, and the
// router's hooks and delegate tool only while it is on and never in its own
// workers. A worker inherits the parent session's value, which names the
// router plugin there, so the inherited entries are filtered first: the guard
// below owns that entry alone.
func pluginDirs(libDir string) string {
	share := filepath.Join(filepath.Dir(libDir), "share")
	dirs := prefixed(withoutOwn(os.Getenv("CLAUDE_CODE_PLUGIN_DIRS"), share)) + filepath.Join(share, "claude-plugin")
	if os.Getenv(router.WorkerEnv) != "1" && router.Enabled() {
		dirs += ":" + filepath.Join(share, "claude-plugin-router")
	}
	return dirs
}

// withoutOwn drops ccmorph's own plugin entries from an inherited plugin path,
// keeping every other entry: a nested session re-adds them below, so they are
// never duplicated, and the router's entry is the guard's alone.
func withoutOwn(value, share string) string {
	own := map[string]bool{filepath.Join(share, "claude-plugin"): true, filepath.Join(share, "claude-plugin-router"): true}
	kept := []string{}
	for _, d := range strings.Split(value, ":") {
		if d != "" && !own[d] {
			kept = append(kept, d)
		}
	}
	return strings.Join(kept, ":")
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

func authKey(env []string) string {
	if v, ok := envGet(env, "ANTHROPIC_API_KEY"); ok {
		return v
	}
	v, _ := envGet(env, "ANTHROPIC_AUTH_TOKEN")
	return v
}

var sessionID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// endedRecordMax bounds a session-end record read; a real one is a session id
// or Claude Code's small JSON input, so anything larger is not a record.
const endedRecordMax = 64 << 10

// recordOwner is whose private file a session-end record must be: the account
// the hook wrote it as. The names sit in a shared TMPDIR, where another account
// can write first, and a planted record can name any transcript it likes, so
// the bytes are trusted only from a file this account owns (#410). A variable
// so a test can pose as the other account.
var recordOwner = os.Geteuid

// readEndedSession reads the session id Claude Code's SessionEnd hook recorded
// for the claude that ran as pid, and consumes the record: the hint prints
// once, and a launcher that died leaves reaping to gc-stale, never a re-read.
// The hook writes only for a session a user attended on a profile; a private
// session's claude is a grandchild with another pid, and its transcripts die
// with its view, so nothing of it is ever read here. The file sits in a shared
// TMPDIR anyone can write, so a record that is not a session id reads as no
// session rather than reaching the terminal (#285); a planted link, FIFO or
// device at its name is refused without being followed or waited on, and so is
// a file another account wrote or group and others can write (#410).
func readEndedSession(pid int) string {
	f := filepath.Join(tmpDir(), "ccmorph-session-end."+strconv.Itoa(pid))
	b, ok := util.ReadPrivateRegular(f, endedRecordMax, recordOwner())
	if !ok {
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
// sent. "" when there is no input, it is not Claude Code's JSON (#359), it is
// not this account's own private file at the name the hook is expected to write
// (#410), or it names a path outside the stores Claude Code writes this
// account's sessions to (#410): the input is untrusted, so the one file it may
// steer a read to is a transcript inside those stores, whatever its size.
// profileDirs is where the profiles keep their config dirs, whose own projects
// stores count with Claude Code's; configDirs are the session's effective
// CLAUDE_CONFIG_DIR entries, when the caller knows them (a profile's env may
// override the store Claude Code writes to).
func readEndedTranscript(pid int, profileDirs string, configDirs ...string) string {
	f := filepath.Join(tmpDir(), "ccmorph-session-end."+strconv.Itoa(pid)+".json")
	b, ok := util.ReadPrivateRegular(f, endedRecordMax, recordOwner())
	if !ok {
		return ""
	}
	os.Remove(f)
	var in struct {
		TranscriptPath string `json:"transcript_path"`
	}
	if json.Unmarshal(b, &in) != nil {
		return ""
	}
	p, ok := transcriptStore(in.TranscriptPath, profileDirs, configDirs...)
	if !ok {
		return ""
	}
	return p
}

// transcriptStore returns the path to read for a transcript path the
// session-end input named, and whether it lies inside a projects store Claude
// Code writes this account's sessions to: its own history, the session's
// effective config dir (a profile's env may override it), or any profile's
// under profileDirs. The input arrives at a name in a shared TMPDIR, so the one
// path it may name is one of these. The path must be spelled inside a store and
// must also lead into one once its links are resolved: carrySession's link
// resolves into another store, but a link to anywhere else must not steer the
// read, however large the file (#410). The resolved path is returned, so the
// later read has no link left to follow. A relative CLAUDE_CONFIG_DIR, or a
// transcript path Claude Code built from one, is resolved against this
// process's working directory, which is the child's: runClaude leaves cmd.Dir
// unset.
func transcriptStore(p, profileDirs string, configDirs ...string) (string, bool) {
	if p == "" {
		return "", false
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", false
	}
	roots := []string{filepath.Join(os.Getenv("HOME"), ".claude", "projects")}
	for _, dir := range configDirs {
		if dir != "" {
			roots = append(roots, filepath.Join(dir, "projects"))
		}
	}
	if profileDirs != "" {
		more, _ := filepath.Glob(filepath.Join(globQuote(profileDirs), "*", "projects"))
		roots = append(roots, more...)
	}
	var spelled, resolved []string
	for _, root := range roots {
		r, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		spelled = append(spelled, r)
		if real, ok := resolveExisting(r); ok {
			resolved = append(resolved, real)
		}
	}
	if !underAny(abs, spelled) {
		return "", false
	}
	real, ok := resolveExisting(abs)
	if !ok || !underAny(real, resolved) {
		return "", false
	}
	return real, true
}

// resolveExisting resolves every link along p's longest existing prefix and
// keeps the rest as spelled: nothing below that prefix exists yet, so no link
// there can lead a later read elsewhere. A name that exists but does not
// resolve — a dangling or looping link — is refused, since what it leads to
// could appear after the check (#410).
func resolveExisting(p string) (string, bool) {
	tail := ""
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(real, tail), true
		}
		if _, lerr := os.Lstat(p); !os.IsNotExist(lerr) {
			return "", false
		}
		parent := filepath.Dir(p)
		if parent == p {
			return "", false
		}
		tail = filepath.Join(filepath.Base(p), tail)
		p = parent
	}
}

// underAny reports whether p lies strictly inside one of roots; every path is
// absolute and clean.
func underAny(p string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, p)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
			if paths.IsFile(h) {
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
		if paths.IsFile(h) {
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
	// Migration can rewrite the confs — a keychain move rewrites them
	// in-process — so the memoized reads read the store's files again (#478)
	// and the conf's (GH-561).
	l.storeReads = nil
	l.confReads = nil
	return l.migrated
}

func (l *launcher) migrateOnce() bool {
	legacy, plain := l.legacyContextState()
	if plain && isDarwin() {
		if err := util.KeychainMigrate(l.dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	return l.migrateContextPaths(legacy)
}

// migrateContextState is the context half of migrateState, for a caller that
// must not also move credentials: doctor stays read-only outside the effort
// row's context read (#556), so it runs the same model-window --migrate the
// launch does and never util.KeychainMigrate, which rewrites every profile's
// plain credentials. The migrate can still rewrite a conf that carries its
// discovered record inline, so the conf's memoized reads drop (GH-561).
func (l *launcher) migrateContextState() bool {
	if os.Getenv("CCMORPH_CONTEXT_LOCKED") != "" {
		return true
	}
	legacy, _ := l.legacyContextState()
	ok := l.migrateContextPaths(legacy)
	l.confReads = nil
	return ok
}

// legacyContextState lists the paths whose presence means legacy context state
// still needs migration, and reports whether a conf also holds a plaintext
// credential that secureDirs moves to the Keychain.
func (l *launcher) legacyContextState() (legacy []string, plain bool) {
	legacy = []string{filepath.Join(l.dir, "model-registry"), filepath.Join(l.dir, "model-registry.conf")}
	g1, _ := filepath.Glob(filepath.Join(globQuote(l.profileDirs), "*", "cache", "context-window"))
	g2, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf.lock"))
	legacy = append(append(legacy, g1...), g2...)
	confs, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf"))
	inline := false
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
	return legacy, plain
}

// migrateContextPaths runs the context migration when any legacy path remains.
func (l *launcher) migrateContextPaths(legacy []string) bool {
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
	v, rc := l.configStore("get", name, "env."+util.TierEnvKey(tier))
	return v, rc == 0 && v != ""
}

// listingKey is the memo's one key for a route's listing.
func listingKey(base, key string) string { return base + "\x1f" + key }

// listed is the listing this pass already holds for (base, key); "" when none. It sends no request.
func (l *launcher) listed(base, key string) string { return l.memo[listingKey(base, key)] }

// listing is _ccm_models_listing: one /v1/models answer per (base, key) per run.
func (l *launcher) listing(base, key string) (string, bool) {
	if base == "" || key == "" {
		return "", false
	}
	mk := listingKey(base, key)
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

// forgetListing drops the memoized listing for (base, key). A 200 that is not a
// listing is no answer, so a retry must reach the gateway again instead of
// re-reading the malformed body the first ask stored (#747).
func (l *launcher) forgetListing(base, key string) {
	delete(l.memo, listingKey(base, key))
}

// pickerOutcome is one project-picker pass as the launch reads it: done, refused
// (the picker said why), or unreached — the listing the pruning needs never
// arrived and the cache judged nothing, which is the gateway's state and not the
// profile's config error (#723).
type pickerOutcome int

const (
	pickerDone pickerOutcome = iota
	pickerRefused
	pickerUnreached
)

// gatewayListingRetries and gatewayListingPause bound the wait in front of the
// outage class: a gateway that has not answered yet may answer in a moment, and
// the launch waits that long — no longer, because a hang is exactly what a
// session may never be (INTENT promise 8).
const (
	gatewayListingRetries = 2
	gatewayListingPause   = time.Second
)

// awaitListing is the wait the picker's silence earns: the model listing is
// asked again a few seconds apart, and the first answer that is a listing ends
// it — the memo then carries it to the picker and every later reader. A 200
// whose body is not a listing (a proxy's error object) is no more an answer
// than silence is, so each attempt drops the memoized body and asks the
// gateway again: a route that answered wrongly once and then serves a listing
// recovers in the window, and one that never serves walks the same path as
// silence to the gateway class (#747).
func (l *launcher) awaitListing(name, base, key string) bool {
	for i := 1; i <= gatewayListingRetries; i++ {
		// What the picker's ask memoized is the body that made the pass
		// unreached; a retry's own malformed answer would be just as useless.
		l.forgetListing(base, key)
		fmt.Fprintf(os.Stderr, "ccmorph: Waiting for '%s' gateway (%ds)…\n", name, int(time.Duration(i)*gatewayListingPause/time.Second))
		time.Sleep(gatewayListingPause)
		if body, ok := l.listing(base, key); ok {
			if _, verdict := ctxwindow.ListingShapeBody(body); verdict != ctxwindow.ListingNotAListing {
				return true
			}
		}
	}
	return false
}

func (l *launcher) projectPicker(name, base, key, own, tag, launch string) pickerOutcome {
	home := filepath.Join(l.profileDirs, name)
	if fi, err := os.Lstat(filepath.Join(home, ".claude.json")); err != nil || !fi.Mode().IsRegular() {
		return pickerDone
	}
	if base == "" || key == "" {
		return pickerDone
	}
	body, _ := l.listing(base, key)
	if tag == "" {
		tag = "Gateway"
	}
	out, ok := try(l, "project-picker", func() pickerOutcome {
		switch util.ProjectPicker(util.PickerIn{Home: home, Conf: l.conf(name), Own: own, Tag: tag, Profile: name,
			Layer: l.profileLayer(name), Launch: launch, ModelsJSON: body}, os.Stderr) {
		case util.PickerDone:
			return pickerDone
		case util.PickerUnreached:
			return pickerUnreached
		}
		return pickerRefused
	})
	if !ok {
		return pickerRefused
	}
	return out
}

// profileEnvironment is `ccmorph _env`: the profile's launch env, after
// provisioning its config dir and running the host's prelaunch step. notice
// receives the removed-key lines the read raises: the launch prints them
// where a person sees them, _env drops them — its output is the answer a
// script reads.
func (l *launcher) profileEnvironment(name string, notice func(string)) ([]string, bool) {
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, notice)
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
// "Is" means both layers are done: ProfileOK answers for the shared layer and
// ProvisionOK for the auth files, so a provisioning skipped as busy is tried
// again by the next launch from the files' state, never a marker (#938).
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
	held, why := l.takeLive(name, policy)
	if !held {
		return false
	}
	if util.ProfileOK(l.profileDirs, name) && util.ProvisionOK(l.profileDirs, name, typ) {
		return true
	}
	setup := util.SetupProfile
	if why != nil && !errors.Is(why, util.ErrLiveHeld) {
		// #217: `add` and `use` go on when the live lock cannot be made. The
		// auth files' rewrite needs that lock (GH-938), so their setup skips it
		// and logs why; a launch refuses until the lock can be made, and then
		// ProvisionOK sends it through SetupProfile before claude starts.
		setup = util.SetupProfileWithoutLive
	}
	if !setup(l.profileDirs, name, typ, l.log, l.err) {
		return false
	}
	// The merge releases the lock it could not take back when its upgrade
	// failed (#205); the session must still be covered before claude starts.
	// No session follows the other policies, so there is nothing to re-cover —
	// and no second note either (#217).
	if policy == liveRefuse && !l.holdLive(name, policy) {
		return false
	}
	if file := l.profileLayer(name); paths.IsFile(file) {
		if fi, err := os.Lstat(file); err == nil && fi.Mode().IsRegular() {
			// The unwire rewrites the layer whole, so it holds the profile's
			// live lock exclusively across its read and rename; a live session
			// of the profile leaves the old status line in place (#939).
			if ok, why := util.LiveExclusive(l.profileDirs, name, func() {
				util.UnwireStatusline(file, filepath.Join(l.dir, "statusline"), filepath.Join(l.libDir, "statusline"))
			}); !ok {
				l.log("settings status line for '" + name + "' left in place: " + why.Error())
			}
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
	ok, _ := l.takeLive(name, policy)
	return ok
}

// takeLive is holdLive that also returns why this process holds no live lock:
// nil when it holds it, util.ErrLiveHeld for another holder, any other error
// for a lock file that could not be made (#217). setupDir reads it (GH-938).
func (l *launcher) takeLive(name string, policy livePolicy) (bool, error) {
	f, why := util.HoldLive(l.profileDirs, name, l.log)
	if f != nil {
		return true, nil
	}
	if policy == liveSilent {
		return true, why
	}
	path := util.LiveLockPath(l.profileDirs, name)
	if policy == liveNote {
		if errors.Is(why, util.ErrLiveHeld) {
			l.err("another launch of '" + name + "' is holding its live lock while it links shared history; the shared history links on a later launch")
		} else {
			l.err("the live lock for '" + name + "' could not be created at " + path + " (" + why.Error() + "); remove what blocks the path — the shared history links on a later launch that can make it")
		}
		return true, why
	}
	if errors.Is(why, util.ErrLiveHeld) {
		l.err("another launch of '" + name + "' is holding its live lock (" + path + ") while it links shared history and has not let go; nothing started. Try again in a moment; if it stays stuck, `lsof " + path + "` names the holder.")
	} else {
		l.err("the live lock for '" + name + "' could not be created at " + path + " (" + why.Error() + "); nothing started. Remove what blocks the path and try again.")
	}
	return false, why
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
	// The fold writes the profile layer the store reads, so the memoized reads
	// read it again (#478).
	l.storeReads = nil
	err = settingscompose.Fold(dir, l.canonical(), profile.SetOf(l.mapping), util.LiveFile(l.profileDirs, name), nil)
	if err != nil {
		// A fold that cannot take the profile's live lock is skipped, never a
		// failed launch: another session of the profile keeps the settings it
		// has and the next launch folds again (#939, promise 8).
		l.log("settings fold for '" + name + "' skipped: " + err.Error())
		return false
	}
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
	return util.LaunchEnv(penv, port, key, adapter, l.adapterPort, os.Getenv("CCMORPH_TASK"), os.Getenv("CCMORPH_ATTEMPT"), os.Getenv("CLAUDE_CODE_SESSION_ID")), true
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
// launch's materialize replaces whatever hook it finds with this one. The
// command is built by settingscompose's one builder, which also matches it, so
// the spelling and the recognition cannot drift (#638).
func (l *launcher) usageHookFor(name, typ, usageRoute string) string {
	pricing := usagePricing(typ, usageRoute)
	if pricing == "" {
		return ""
	}
	return settingscompose.UsageHookCommand(stableCore(l.libexec), name, pricing)
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
	if !paths.IsFile(filepath.Join(stable, "ccmorph-core")) {
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

// edgeEnv is what ccmorph-rc's session_env sets for the edge's route: its proxy and
// CA, the route's thread setting and the record a nested launch reads.
var edgeEnv = []string{"HTTPS_PROXY", "https_proxy", "NODE_EXTRA_CA_CERTS", "CLAUDE_CODE_TETHER_LIVE", "CCMORPH_EDGE_ROUTE_ENV"}

// loopbackHTTPSProxy: HTTPS_PROXY (or https_proxy) is http://127.0.0.1:<port>.
func loopbackHTTPSProxy() bool {
	_, ok := util.LoopbackProxyPort()
	return ok
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
func (l *launcher) providerStartRefusal(name string) string { return bridgeRefusal(l.dir, name) }

func bridgeRefusal(dir, name string) string {
	if _, err := exec.LookPath("cliproxyapi"); err != nil {
		return "cliproxyapi not found — install it: " + util.ClipInstallStep()
	}
	root := filepath.Join(dir, "auth")
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

// acquireBridge starts or joins the profile's shared bridge and holds this
// caller's use of it until the returned bridge is released. It reads no
// launcher state, so the agent proxy's goroutines may call it; the bridge's own
// notices go to errw.
func acquireBridge(dir, libexec, logPath, name string, errw io.Writer) (*util.SharedBridge, error) {
	if why := bridgeRefusal(dir, name); why != "" {
		return nil, errors.New(why)
	}
	bin, _ := exec.LookPath("cliproxyapi")
	root := filepath.Join(dir, "auth")
	auth := filepath.Join(root, name)
	start := func() (pid, port, key, tmp string, ok bool) {
		cmd := exec.Command(filepath.Join(libexec, "ccmorph-core"), "util", "bridge-start", name, bin, auth, logPath)
		cmd.Stderr = errw
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
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, util.ErrBridgeNotStarted
	}
	return b, nil
}

// providerStart starts or joins the profile's shared bridge, recording the
// handle every session of the profile shares. A route env joins it too, and a
// holder process keeps the route's use of it until the route retires (#300).
func (l *launcher) providerStart(name string) bool {
	if why := l.providerStartRefusal(name); why != "" {
		l.err(why)
		return false
	}
	l.logInit()
	b, err := acquireBridge(l.dir, l.libexec, l.logOrNull(), name, os.Stderr)
	if err != nil && !errors.Is(err, util.ErrBridgeNotStarted) {
		l.err(err.Error())
	}
	if err != nil {
		return false
	}
	l.providerBridge = b
	l.providerPID, l.providerPort, l.providerKey, l.providerTD = b.PID, b.Port, b.Key, b.TmpDir
	l.providerAuth, l.providerActive = filepath.Join(l.dir, "auth", name), name
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
		base, _ = nativeBase(base)
		if _, code := util.ProbeListing(base, authKey(env), "anthropic-version: 2023-06-01"); probeVerdict(code) == probeRejected {
			return "Anthropic rejected the API key (HTTP " + code + ")"
		}
		return ""
	}
	_, code, why := util.ProbeListingWhy(base, authKey(env))
	switch probeVerdict(code) {
	case probeSilent:
		if why != "" {
			return "could not reach '" + profile.RedactURL(base) + "' — " + why
		}
		return "the endpoint " + profile.RedactURL(base) + " did not answer"
	case probeRejected:
		return "the endpoint rejected the API key (HTTP " + code + ")"
	}
	return ""
}

// reachGateway is the launch's first question to a gateway (#260): the
// profile's base_url and api_key — the route profile.Env builds, since an
// env.* line may not override either (profile.BackendEnv) — asked once with
// doctor's bounds, through the profile's env.CCMORPH_UPSTREAM_PROXY (via)
// when it names one, the route the session's own requests take. A progress line follows one second of
// waiting. why is the cause, said here in the sentence models and doctor use,
// "" when the gateway answered or failed in a way the four causes do not name;
// down is true when nothing answered within the bound: then the route check is
// named too, and the caller ends the launch, because every later wait (the
// picker's, the adapter's) would sit on the same silence.
func (l *launcher) reachGateway(name, base, key, via string) (why string, down bool) {
	if base == "" || key == "" {
		return "", false
	}
	// A key Claude Code was told to refuse stops the launch before any
	// request (#236): keyApproval says so, and the probe must not ask first.
	if util.KeyResponse(filepath.Join(l.profileDirs, name), key) == util.KeyRefused {
		return "", false
	}
	var strip []string
	if l.inEdge() {
		strip = edgeEnv // a named profile never dials through the edge (see resolveLaunch)
	}
	progress := func() {
		fmt.Fprintf(os.Stderr, "ccmorph: Checking '%s' gateway (up to %ds); Ctrl-C to cancel.\n", name, util.ReachBoundSeconds())
	}
	r := reachListing(base, key, via, strip, progress)
	if r.Why == "" {
		return "", false
	}
	l.err(listingFailureWhy(name, profile.RedactURL(base), 0, r.Why))
	if r.Waited {
		fmt.Fprintf(os.Stderr, "  Check the route: ccmorph doctor %s\n", name)
	}
	return r.Why, r.Waited
}

// adapterModel is the gateway id the adapter serves: the explicit or default
// model, a tier word resolved by resolveTier — the one spelling of that rule.
func (l *launcher) adapterModel(name string, penv []string, explicit, defaultModel string) (string, bool) {
	model := explicit
	if model == "" {
		model = defaultModel
	}
	resolved, ok := resolveTier(model,
		func(key string) (string, bool) { return envGet(penv, key) },
		func(tier string) (string, bool) { return l.tierAlias(name, tier) })
	if !ok {
		origin := l.modelOrigin(name)
		if explicit != "" {
			origin = "given with --model"
		}
		base, _ := envGet(penv, "ANTHROPIC_BASE_URL")
		l.err(util.UnservedTier(name, util.TierWord(model), origin, util.ServedSample(l.conf(name), l.listed(base, authKey(penv)))))
		return "", false
	}
	return resolved, true
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
		// A refusal the adapter worded itself — the cause and the one command
		// that repairs it — is the whole message: the route checks would repeat
		// the cause and name a command that disagrees with it (#374). A bare
		// death or a timeout still gets plain words first when the cause is the
		// route itself (#286), then the one command that diagnoses the route.
		if !strings.Contains(string(out), util.Refused) {
			if why := l.routeRefusal(name); why != "" {
				fmt.Fprintln(os.Stderr, "  "+why+".")
			}
			fmt.Fprintln(os.Stderr, "  Check the route: ccmorph doctor "+name)
		}
		if l.logPath != "" && paths.Exists(l.logPath) {
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
	// The child runs in this process's working directory (cmd.Dir is unset), so a
	// relative CLAUDE_CONFIG_DIR names the store transcriptStore resolves against it.
	configDir, _ := envGet(env, "CLAUDE_CONFIG_DIR")
	l.endedTranscript = readEndedTranscript(cmd.Process.Pid, l.profileDirs, configDir)
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
		base, _ := nativeBase(f["base_url"])
		return int(util.ModelServed(want, "apikey", base, "", f["api_key"]))
	}
	return l.modelKnown(name, want)
}
