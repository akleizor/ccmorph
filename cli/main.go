package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/profile"
)

// retiredVerb is the retired window verb, spelled without writing it: still
// reserved (never a profile name, never forwarded to Claude Code).
const retiredVerb = "mea" + "sure"

// ownVerbs are ccmorph's verbs; they win over any Claude Code verb of the same name.
var ownVerbs = map[string]bool{"list": true, "inspect": true, "show": true, "add": true, "config": true,
	"login": true, "default": true, "use": true, "rm": true, "mv": true, "get": true, "set": true,
	"context": true, "cred": true, "secret": true, "creds": true, "probe": true, "hop": true, "profile": true,
	"help": true, "version": true, "run": true, "start": true, retiredVerb: true}

// helpAsked reports whether any word asks for help.
func helpAsked(words []string) bool {
	return slices.Contains(words, "-h") || slices.Contains(words, "--help")
}

// verbArity is the most words each public command takes after the verb itself.
// One more is refused before anything runs. run and probe are handled apart:
// run's words after the profile are Claude Code's, and probe's are parsed by
// context-probe, which reports its own usage.
var verbArity = map[string]int{"list": 0, "use": 1, "cred": 1, "inspect": 1, "context": 3,
	"models": 2, "rm": 1, "mv": 2, "get": 2, "set": 3, "config": 1, "add": 1, "login": 1,
	"hop": 1, "quota": 2, "doctor": 2, "update": 1, "uninstall": 0, "version": 0, "help": 1,
	"profile": 0}

// usageLine is the Usage line of a command's help page, shared with its errors
// so the two cannot drift apart.
func usageLine(topic string) string {
	if topic == "get" || topic == "set" {
		line := "ccmorph " + topic + " [profile] [model|effort]"
		if topic == "set" {
			line += " [value]"
		}
		return line
	}
	return usageTopics[topic][0]
}

// extraArg refuses a word beyond what the command takes, naming it.
func (l *launcher) extraArg(verb, word string) int {
	l.err("unexpected argument '" + word + "'")
	l.err("usage: " + usageLine(verb))
	return 2
}

// realVerbs is every verb `ccmorph <verb>` answers to, in one list for the typo
// hint: a word one edit away from exactly one of these is told to run it.
var realVerbs = []string{"add", "config", "context", "cred", "doctor", "get", "help", "hop",
	"inspect", "list", "login", "models", "mv", "probe", "profile", "quota", "rm", "run", "set",
	"uninstall", "update", "use", "version"}

// topFlags are the flags ccmorph itself takes before the verb.
var topFlags = []string{"--help", "--version"}

// guessedVerbs are the words a newcomer guesses at ccmorph's terser verbs; each
// names the command that does what it says.
var guessedVerbs = map[string]string{
	"ls": "list", "remove": "rm", "delete": "rm", "rename": "mv", "switch": "use",
}

// dispatch is the `ccmorph` command: every verb, help and the launch's own argv.
func (l *launcher) dispatch(args []string) int {
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch first {
	case "", "-h", "--help", "help":
		rest := args[min(1, len(args)):]
		if first == "help" && helpAsked(rest) {
			return l.usage("help")
		}
		rest = slices.DeleteFunc(slices.Clone(rest), func(a string) bool { return a == "-h" || a == "--help" })
		if len(rest) > 1 {
			return l.extraArg("help", rest[1])
		}
		topic := ""
		if len(rest) == 1 {
			topic = rest[0]
		}
		return l.usage(topic)
	case "-v", "-V", "--version", "version":
		if helpAsked(args[1:]) {
			return l.usage("version")
		}
		if len(args) > 1 {
			return l.extraArg("version", args[1])
		}
		fmt.Println("ccmorph " + l.version)
		return 0
	case "_backend_env":
		fmt.Println(strings.Join(l.backend, "\n"))
		return 0
	case "_env_keys": // tests: one of the Go env-key lists, a key per line
		lists := map[string][]string{"tuning": l.tuning, "profile": l.profileEnv, "launch": l.launchEnv,
			"backend": l.backend, "mapping": l.mapping}
		v, ok := lists[strings.Join(args[1:], "")]
		if !ok {
			return 2
		}
		fmt.Println(strings.Join(v, "\n"))
		return 0
	}
	if rest := args[1:]; first == "run" {
		// After a run profile the words are Claude Code's (INTENT promise 6), so
		// only a leading -h/--help is ccmorph's.
		if len(rest) > 0 && (rest[0] == "-h" || rest[0] == "--help") {
			return l.usage("run")
		}
	} else if first == "probe" || first == retiredVerb {
		// The page is the cli's, as every command's is; answer it before
		// secureDirs, so no picker opens and no state is provisioned.
		if helpAsked(rest) {
			return l.usage("probe")
		}
	} else if n, public := verbArity[first]; public {
		if helpAsked(rest) {
			return l.usage(first)
		}
		if first == "hop" && len(rest) > 0 && rest[0] == hopSlashFlag {
			n++ // the slash command's marker is not a word the user typed
		}
		if len(rest) > n {
			return l.extraArg(first, rest[n])
		}
	}
	switch first {
	case "update":
		return l.runInstaller("update", args[1:])
	case "uninstall":
		return l.runInstaller("uninstall", nil)
	case "doctor":
		return l.doctorVerb(args[1:])
	}
	if !l.secureDirs() {
		l.err("cannot secure ccmorph directories")
		return 1
	}
	switch first {
	case "_secure": // install.sh: the state dirs secured and migrated, nothing else
		return 0
	case "run":
		return l.runVerb(args[1:])
	case "probe", retiredVerb:
		return l.probeVerb(args[1:])
	case "start":
		return l.retired("start", "run", args)
	case "default":
		return l.retired("default", "use", args)
	case "show":
		return l.retired("show", "inspect", args)
	case "creds", "secret":
		return l.retired(first, "cred", args)
	case "_route_env":
		return l.routeEnv(args[1:])
	case "_route_stop":
		return l.routeStop(args[1:])
	case "_env":
		return l.envVerb(args[1:])
	case "list", "use", "cred", "inspect", "context", "models", "_models", "rm", "mv", "get", "set",
		"_menu", "_ui", "config", "add", "login", "hop", "profile", "quota":
		return l.verb(args)
	}
	return l.unknown(first, args)
}

// unknown answers a word no verb matched. A word that names a command — a
// common guess, or one typo away from a real verb — is told which one; the
// profile list waits for a word that could itself be a profile, so it never
// runs under a command guess or a flag.
func (l *launcher) unknown(first string, args []string) int {
	if strings.HasPrefix(first, "-") {
		if now := oneEditFrom(first, topFlags); now != "" {
			l.err("unknown option '" + first + "'. Run: ccmorph " + now)
		} else {
			l.err("unknown option '" + first + "'. Launch flags go after the verb: ccmorph run " + first)
		}
		return 1 // a flag is never a profile name
	}
	// The guess wins over a profile of the same name: a bare verb-shaped word
	// is what a newcomer typing a command produces.
	if now := guessedVerbs[first]; now != "" {
		return l.retired(first, now, args)
	}
	if l.exists(first) {
		l.err("unknown command '" + first + "'. Run: ccmorph --help")
		fmt.Fprintln(os.Stderr, "  To launch that profile: ccmorph run "+first)
		l.accountsHint()
		return 1
	}
	if now := oneEditFrom(first, realVerbs); now != "" {
		return l.retired(first, now, args)
	}
	l.err("unknown command '" + first + "'. Run: ccmorph --help")
	if profile.ValidName(first) {
		l.accountsHint()
	}
	return 1
}

// retired answers a word ccmorph does not run — a retired verb, a common
// guess, or a typo — by naming the command that does what it says.
func (l *launcher) retired(verb, now string, args []string) int {
	msg := "unknown command '" + verb + "'. Run: ccmorph " + now
	if len(args) > 1 {
		msg += " " + strings.Join(args[1:], " ")
	}
	l.err(msg)
	return 1
}

// oneEditFrom returns the word one insertion, deletion, substitution or
// adjacent transposition from word — the shape of a typo — when exactly one of
// words is, and "" when none or several are (a tie says nothing).
func oneEditFrom(word string, words []string) string {
	hit, ties := "", 0
	for _, w := range words {
		if oneEdit(word, w) {
			hit, ties = w, ties+1
		}
	}
	if ties == 1 {
		return hit
	}
	return ""
}

// oneEdit reports whether a and b are at most one insertion, deletion,
// substitution or adjacent transposition apart.
func oneEdit(a, b string) bool {
	x, y := []rune(a), []rune(b)
	i := 0
	for i < len(x) && i < len(y) && x[i] == y[i] {
		i++
	}
	if i == len(x) && i == len(y) {
		return true
	}
	tail := func(ai, bi int) bool { return slices.Equal(x[ai:], y[bi:]) }
	switch {
	case len(x) == len(y): // a substitution, or one swapped pair
		return tail(i+1, i+1) ||
			i+1 < len(x) && x[i] == y[i+1] && x[i+1] == y[i] && tail(i+2, i+2)
	case len(x) == len(y)+1: // one rune x has and y lacks
		return tail(i+1, i)
	case len(y) == len(x)+1: // one rune y has and x lacks
		return tail(i, i+1)
	}
	return false
}

// secureDirs is _ccm_secure_dirs: refuse symlinked roots, migrate, create and
// tighten the state dirs (chmod is best effort), then migrate stored state.
func (l *launcher) secureDirs() bool {
	for _, root := range []string{l.dir, l.profileDirs} {
		if isLink(root) {
			l.err("refusing symlinked state root '" + root + "'")
			return false
		}
	}
	home := os.Getenv("HOME")
	legacyHome := filepath.Join(home, ".claude-profiles")
	if _, err := os.Lstat(legacyHome); err == nil {
		same := isLink(legacyHome) && isDir(l.profileDirs) && !isLink(l.profileDirs) &&
			resolvePath(legacyHome) == resolvePath(l.profileDirs)
		if !same {
			moved, err := util.MigrateProfileHome(legacyHome, l.profileDirs)
			if moved {
				fmt.Fprintf(os.Stderr, "ccmorph: profile storage moved to %s; legacy path retained as a compatibility link\n", l.profileDirs)
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return false
			}
		}
	}
	if os.MkdirAll(l.dir, 0o700) != nil || os.MkdirAll(l.profileDirs, 0o700) != nil {
		return false
	}
	l.normalizePluginIndex()
	os.Chmod(l.dir, 0o700)
	os.Chmod(l.profileDirs, 0o700)
	active := filepath.Join(l.dir, "active")
	if _, err := os.Lstat(active); err == nil {
		if isLink(active) {
			return false
		}
		os.Chmod(active, 0o600)
	}
	confs, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf"))
	for _, c := range confs {
		if isLink(c) {
			return false
		}
		os.Chmod(c, 0o600)
	}
	return l.migrateState()
}

func (l *launcher) normalizePluginIndex() {
	home := os.Getenv("HOME")
	for _, name := range []string{"known_marketplaces.json", "installed_plugins.json"} {
		b, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", name))
		if err != nil {
			continue
		}
		s := string(b)
		if strings.Contains(s, filepath.Join(home, ".claude-profiles")+"/") || strings.Contains(s, l.profileDirs+"/") {
			util.NormalizePluginIndex(home, l.profileDirs)
			return
		}
	}
}

// runVerb is `ccmorph run [profile] [-i] [-- args]`: pick the profile, strip run's
// own separator and launch.
func (l *launcher) runVerb(args []string) int {
	if len(args) > 0 && (args[0] == "-a" || args[0] == "--profile" || strings.HasPrefix(args[0], "--profile=")) {
		l.err("launch profile is positional: ccmorph run <profile>")
		return 2
	}
	hasI, interactive := false, false
	if len(args) > 0 && (args[0] == "-i" || args[0] == "--interactive") {
		hasI, interactive, args = true, true, args[1:]
	}
	// A short word that is not one of Claude Code's options is a profile name
	// typed with a leading hyphen, not a launch flag (#373).
	if len(args) > 0 && !l.runFlag(args[0]) {
		return l.badName("run", args[0])
	}
	account := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if l.exists(args[0]) {
			account, args = args[0], args[1:]
			if len(args) > 0 && (args[0] == "-i" || args[0] == "--interactive") {
				interactive, args = true, args[1:]
			}
		} else if !l.isClaudeSubcommand(args[0]) {
			if !profile.ValidName(args[0]) {
				return l.badName("run", args[0])
			}
			l.noProfile(args[0])
			return 1
		}
	}
	if account == "" && l.active() == "" && !hasI {
		if len(l.confNames()) == 0 {
			l.err(noProfilesMsg)
			return 1
		}
		l.err("no default profile set — run: ccmorph use <profile> or ccmorph run -i")
		l.accountsHint()
		return 1
	}
	// `--` is run's own separator only in prefix position; later ones are Claude Code's.
	if i := runSeparator(args); i >= 0 {
		args = append(args[:i:i], args[i+1:]...)
	}
	sub := "0"
	if l.hasClaudeSubcommand(args) {
		sub = "1"
	}
	// The launch replaces this process, so the PID the caller holds is the launcher.
	env := append(l.handoff(), "_CCMORPH_LAUNCHER_PID="+strconv.Itoa(l.launcherPID),
		"_CCMORPH_NOTICE="+l.notice, "_CCMORPH_LAUNCH_PROFILE="+account, "_CCMORPH_NATIVE_ARGS=1",
		"_CCMORPH_INTERACTIVE="+map[bool]string{true: "1", false: "0"}[interactive],
		"CCMORPH_SUBCOMMAND_LAUNCH="+sub)
	core := filepath.Join(l.libexec, "ccmorph-core")
	base := os.Environ()
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		base = envDrop(base, k) // getenv takes the first copy, so ours must be the only one
	}
	err := syscall.Exec(core, append([]string{core, "launch"}, args...), append(base, env...))
	l.err("cannot start the launch: " + err.Error())
	return 1
}

// probeVerb is `ccmorph probe [profile|-i] [options]`: context-probe with the
// profile positional, and `--model -i` (or a bare trailing --model) picked.
func (l *launcher) probeVerb(args []string) int {
	var a []string
	if len(args) > 0 && (args[0] == "-i" || args[0] == "--interactive") {
		p, ok := l.menuProfile()
		if !ok {
			return 1
		}
		a, args = append(a, "--profile", p), args[1:]
	}
	// context-probe takes long options only; its one short, -h, is answered as
	// help before a verb runs. Any other short word in the profile slot is a
	// name typed with a leading hyphen, not an option (#373).
	if len(args) > 0 && strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[0], "--") {
		return l.badName("probe", args[0])
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		a, args = append(a, "--profile", args[0]), args[1:]
	}
	a = append(a, args...)
	for i, v := range a {
		p, named := "", false
		if v == "--profile" && i+1 < len(a) {
			p, named = a[i+1], true
		} else if strings.HasPrefix(v, "--profile=") {
			p, named = strings.TrimPrefix(v, "--profile="), true
		}
		if !named {
			continue
		}
		if !profile.ValidName(p) {
			return l.badName("probe", p)
		}
		if !l.exists(p) {
			l.noProfile(p)
			return 1
		}
		if l.noModelList(p) {
			return 1
		}
	}
	if mi := lastIndex(a, "--model"); mi >= 0 && (mi+1 >= len(a) || a[mi+1] == "" || interactive(a[mi+1])) {
		pi := lastIndex(a, "--profile")
		if pi < 0 || pi+1 >= len(a) {
			l.err("--model -i needs a profile: ccmorph probe <profile> --model -i")
			return 2
		}
		m, ok := l.pickValue("model", a[pi+1])
		if !ok {
			return 1
		}
		if mi+1 >= len(a) {
			a = append(a, m)
		} else {
			a[mi+1] = m
		}
	}
	if lastIndex(a, "--allow-inference") >= 0 {
		fmt.Fprintln(os.Stderr, "ccmorph: Checking context limits; inference requests may incur charges.")
	}
	cmd := exec.Command(filepath.Join(l.libexec, "ccmorph-core"), append([]string{"context-probe"}, a...)...)
	cmd.Env = append(os.Environ(), l.handoff()...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

// documentedShorts are Claude Code's short options, for the runs where its help
// cannot be read (a stub, an install that answers nothing).
var documentedShorts = []string{"-c", "-d", "-h", "-n", "-p", "-r", "-v", "-w"}

// runFlag reports whether word may lead a run as an option of Claude Code's,
// rather than being read as a profile name. A long option is Claude's and passes
// through untouched (INTENT promise 6), and so is a lone "-". A short word is
// Claude's when its first letter is one of Claude's short options, from its help
// or the documented list: Claude reads the rest as that option's value (-nTitle)
// or as more of its flags (-pc). Any other short word, -x among them, can only be
// a name typed with a leading hyphen (#373).
func (l *launcher) runFlag(word string) bool {
	if len(word) < 2 || word[0] != '-' || word[1] == '-' {
		return true
	}
	short := word[:2]
	if slices.Contains(documentedShorts, short) {
		return true
	}
	return l.claudeArity()[short] != ""
}

func lastIndex(a []string, v string) int {
	for i := len(a) - 1; i >= 0; i-- {
		if a[i] == v {
			return i
		}
	}
	return -1
}

// runSeparator is the index of run's own `--`, or -1.
func runSeparator(args []string) int {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--":
			return i
		case a == "-i" || a == "--interactive" || strings.HasPrefix(a, "--model=") || strings.HasPrefix(a, "--effort="):
		case a == "--model" || a == "--effort":
			i++
		default:
			return -1
		}
	}
	return -1
}

func (l *launcher) isClaudeSubcommand(word string) bool {
	if word == "" || ownVerbs[word] {
		return false
	}
	return slices.Contains(util.ClaudeVerbs(l.claudeHelp()), word)
}

// hasClaudeSubcommand is _ccm_launch_has_claude_subcommand: does the first
// positional, after stepping over each option's value, name a Claude Code command.
func (l *launcher) hasClaudeSubcommand(args []string) bool {
	var arity map[string]string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return false
		case a == "-i" || a == "--interactive" || a == "--dangerously-skip-permissions":
			continue
		case a == "-p" || a == "--print" || strings.HasPrefix(a, "--print="):
			return false
		case len(a) > 1 && a[0] == '-':
			if arity == nil {
				arity = l.claudeArity()
			}
			opt := a
			if strings.HasPrefix(a, "--") {
				opt, _, _ = strings.Cut(a, "=")
			}
			kind := arity[opt]
			if kind == "" {
				return false
			}
			if !strings.Contains(a, "=") && i+1 < len(args) {
				if next := args[i+1]; kind == "value" || (kind == "optional" && next != "" && !strings.HasPrefix(next, "-")) {
					i++
				}
			}
			continue
		}
		return l.isClaudeSubcommand(a)
	}
	return false
}

// envVerb is `ccmorph _env <profile>`: provision the profile and print its launch env.
func (l *launcher) envVerb(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if !profile.ValidName(name) {
		return 1
	}
	if !l.exists(name) {
		fmt.Fprintln(os.Stderr, "ccmorph: unknown profile '"+name+"'")
		return 1
	}
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, func(string) {})
	if err != nil {
		msg := err.Error()
		if msg == "" {
			msg = "could not read profile '" + name + "'"
		}
		l.err(msg)
		return 1
	}
	if !l.setupDir(name, typ, liveRefuse) {
		return 1
	}
	l.prelaunch(name)
	if len(env) > 0 {
		fmt.Println(strings.Join(env, "\n"))
	}
	return 0
}

// routeEnv is `ccmorph _route_env <profile>`: _env, plus the profile's shared
// bridge and the handle that retires it. The route joins the bridge a session
// of the profile runs, or starts it as the shared one — a second, private
// bridge on the same auth dir would spend the single-use refresh token twice
// and sign the profile out (#300). This process exits with the env, so a
// holder process owns the route's use of the bridge until the route retires:
// without it, the last session out or a sibling route's retirement would stop
// the bridge under a live route. A hold that never lands refuses the route
// outright rather than leave it unprotected.
func (l *launcher) routeEnv(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if !profile.ValidName(name) {
		return 1
	}
	if !l.exists(name) {
		fmt.Fprintln(os.Stderr, "ccmorph: unknown profile '"+name+"'")
		return 1
	}
	f, ok := l.read(name)
	if !ok {
		return 1
	}
	if f["type"] != "provider" {
		return l.envVerb(args)
	}
	if !l.providerStart(name) {
		return 1
	}
	holder, held := l.routeHold(filepath.Join(l.dir, "auth"), name)
	if !held {
		l.providerStop()
		l.err("could not keep a hold on the bridge for '" + name + "'; the route was not started — try again")
		return 1
	}
	if l.envVerb(args) != 0 {
		util.EndRouteHold(holder, util.BridgeFiles(filepath.Join(l.dir, "auth"), name)[2])
		l.providerStop()
		return 1
	}
	fmt.Println("ANTHROPIC_BASE_URL=http://127.0.0.1:" + l.providerPort)
	fmt.Println("ANTHROPIC_API_KEY=" + l.providerKey)
	fmt.Println("CCMORPH_ROUTE_HANDLE=provider|" + l.providerPID + "|" + l.providerTD + "|" + l.providerAuth + "|" + holder)
	return 0
}

// routeStop retires a route _route_env started; only a provider ever opens one.
// The handle names the profile's shared bridge and the holder that owns the
// route's use of it, so retiring it leaves a bridge a session or another route
// still uses running (#300).
func (l *launcher) routeStop(args []string) int {
	if len(args) == 0 || !strings.HasPrefix(args[0], "provider|") {
		return 0
	}
	parts := strings.Split(args[0], "|")
	for len(parts) < 5 {
		parts = append(parts, "")
	}
	if !digits.MatchString(parts[1]) {
		return 1
	}
	l.providerPID, l.providerTD, l.providerAuth, l.providerHolder = parts[1], parts[2], parts[3], parts[4]
	l.providerStop()
	return 0
}

// runInstaller hands `update [ref]` and `uninstall` to the install.sh beside lib/.
func (l *launcher) runInstaller(verb string, rest []string) int {
	root := filepath.Dir(l.libDir)
	installer := filepath.Join(root, "install.sh")
	if verb == "update" {
		if len(rest) == 1 {
			ref := rest[0]
			if !refName.MatchString(ref) || strings.Contains(ref, "..") {
				l.err("invalid ref '" + ref + "' — use a branch or tag name such as main or v0.1.0")
				return 2
			}
		}
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			l.err(root + " is a git checkout; update it with git (git -C " + shQuote(root) + " pull), not with ccmorph update")
			return 1
		}
	}
	if strings.Contains(resolvePath(root), "/Cellar/ccmorph/") {
		brew := map[string]string{"update": "upgrade", "uninstall": "uninstall"}[verb]
		l.err("ccmorph was installed with Homebrew; run: brew " + brew + " ccmorph")
		return 1
	}
	if !isFile(installer) {
		l.err("no installer at " + installer + ", so ccmorph cannot " + verb + " itself. Run the one-line install once:")
		fmt.Fprintln(os.Stderr, `  sh -c "$(curl -fsSL https://github.com/akleizor/ccmorph/releases/latest/download/install.sh)"`)
		return 1
	}
	flag := "--uninstall"
	env := os.Environ()
	if verb == "update" {
		flag = "--remote"
		env = envDrop(env, "CCMORPH_REF")
		if len(rest) == 1 {
			env = append(env, "CCMORPH_REF="+rest[0])
		}
	}
	cmd := exec.Command("sh", installer, flag)
	cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = env, os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run())
}

var (
	refName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	shSafe  = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)
)

func shQuote(s string) string {
	if shSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*exec.ExitError); ok {
		return e.ExitCode()
	}
	return 1
}

// helpWidth is the columns a help page wraps to: stdout's terminal width when
// it is a terminal, 0 — no wrapping, the authored lines stay as written — when
// it is a pipe or file, and 80 when a terminal reports no width.
func helpWidth() int {
	if !isTTY(1) {
		return 0
	}
	_, cols := winsize(1)
	if cols < 1 {
		return 80
	}
	return cols
}

// wrapWords breaks text into lines of at most width cells on word boundaries;
// a word is never split, so one wider than width keeps a line of its own.
func wrapWords(text string, width int) []string {
	if width < 1 {
		return []string{text}
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return []string{""}
	}
	lines, line := []string{}, words[0]
	for _, word := range words[1:] {
		if cells(line)+1+cells(word) <= width {
			line += " " + word
			continue
		}
		lines, line = append(lines, line), word
	}
	return append(lines, line)
}

// wrapBody wraps every line of a help body to width columns, continuing under
// the line's own indent so a wrapped list item stays one item. width 0 returns
// the body unchanged.
func wrapBody(body string, width int) string {
	if width <= 0 {
		return body
	}
	var out []string
	for _, line := range strings.Split(body, "\n") {
		text := strings.TrimLeft(line, " ")
		indent := line[:len(line)-len(text)]
		if cells(line) <= width {
			out = append(out, line)
			continue
		}
		for _, wrapped := range wrapWords(text, width-cells(indent)) {
			out = append(out, indent+wrapped)
		}
	}
	return strings.Join(out, "\n")
}

// helpGrid is grid for the help pages: the same row, but a description longer
// than the width left after the label column wraps under that column, the way
// grid already places a long label's description there. width 0 prints the row
// exactly as grid does.
func helpGrid(w int, label, desc string, width int) {
	if width <= 0 {
		grid(w, label, desc)
		return
	}
	col := 2 + w + 1
	chunks := wrapWords(desc, width-col)
	if len(label) > w {
		fmt.Printf("  %s\n  %-*s %s\n", label, w, "", chunks[0])
	} else {
		fmt.Printf("  %-*s %s\n", w, label, chunks[0])
	}
	for _, chunk := range chunks[1:] {
		fmt.Println(strings.Repeat(" ", col) + chunk)
	}
}

func (l *launcher) usage(topic string) int {
	return l.usageAt(topic, helpWidth())
}

// confDirSpelling is the state root as a help page spells a conf file's
// directory: the root this launcher resolved, cleaned, spelled ~/… under the
// home directory — so the default root spells itself ~/.config/ccmorph and
// only a redirected one changes a page.
func (l *launcher) confDirSpelling() string {
	root := filepath.Clean(l.dir)
	if home, err := os.UserHomeDir(); err == nil {
		if rel, ok := strings.CutPrefix(root, home+string(filepath.Separator)); ok {
			return "~/" + rel
		}
	}
	return root
}

// usageAt prints a help page wrapped to width columns (helpWidth). width 0 is
// what a pipe receives: the authored lines, unwrapped.
func (l *launcher) usageAt(topic string, width int) int {
	help := func(k, t string) { helpGrid(32, k, t, width) }
	head := func(s string) { fmt.Println("\n" + l.bold + s + l.reset) }
	switch topic {
	case "", "all":
		// `all` is the same page with the rest of the vocabulary below the
		// everyday groups, not the short page printed again inside it (#365).
		all := topic == "all"
		fmt.Println("ccmorph — Claude Code profile switcher")
		fmt.Println("\nUsage: ccmorph <command> [arguments]")
		head("Launch:")
		help("ccmorph run", "Launch the default profile")
		help("ccmorph run <profile>", "Use a profile for this session")
		help("ccmorph run -i", "Choose a profile interactively")
		help("ccmorph hop [profile]", "Switch a running session, from inside Claude")
		head("Profiles:")
		help("ccmorph add <profile>", "Add an account or API key")
		help("ccmorph list", "See profiles and the default")
		help("ccmorph quota [profile]", "Plan usage and spend of every profile")
		help("ccmorph use <profile>", "Set the default; do not launch")
		help("ccmorph config [profile]", "Change model, effort, or connection")
		head("Troubleshooting:")
		help("ccmorph inspect [profile]", "Show effective settings and masking; no secrets")
		help("ccmorph doctor [--verbose] [profile]", "Check why a profile is not working")
		if all {
			head("More commands:")
			help("ccmorph models <profile>", "List models from an API-key endpoint")
			help("ccmorph get <profile> <model|effort>", "Read a saved default")
			help("ccmorph set <profile> <model|effort> [value]", "Save a default; omit value to choose")
			help("ccmorph login <profile>", "Sign in again to a provider account")
			help("ccmorph mv <old> <new>", "Rename a profile")
			help("ccmorph rm <profile>", "Delete a profile and its private sign-in state")
			help("ccmorph profile", "Name the profile a session runs on")
			help("ccmorph cred <profile>", "Print a stored secret (sensitive)")
			help("ccmorph update [ref]", "Update to the latest version, or to a branch or tag")
			help("ccmorph uninstall", "Remove the command; profiles stay")
			head("Advanced troubleshooting (normally automatic):")
			help("ccmorph probe [profile]", "Look up context window sizes; no inference")
			help("ccmorph context <profile> <model> [tokens|auto]", "Override or reset one model context limit")
		}
		// The footer ends whichever page this is, once. Where to start is advice
		// for a reader with no profile yet: one who has any is past it, and the
		// example name might be a profile they already have (#365).
		fmt.Println()
		if len(l.confNames()) == 0 {
			fmt.Println("Start here: ccmorph add work")
		}
		if !all {
			fmt.Println("More commands: ccmorph help all")
		}
		fmt.Println("Command details: ccmorph help <command>")
		fmt.Println("Version: ccmorph --version")
		return 0
	}
	t, ok := usageTopics[topic]
	if !ok {
		l.err("unknown help topic '" + topic + "'. Run: ccmorph help all")
		return 2
	}
	fmt.Println("Usage: " + usageLine(topic))
	fmt.Println()
	body := t[1]
	if topic == "add" {
		// A provider login needs a bridge; name those only when one is installed.
		if list, _ := addProviders(); len(list) > 0 {
			labels := make([]string, len(list))
			for i, p := range list {
				labels[i] = providerLabel(p)
			}
			body = strings.Replace(body, "\n\nCredentials",
				"\n  "+strings.Join(labels, ", ")+": sign in to that service with your browser (through cliproxyapi).\n\nCredentials", 1)
		}
	}
	// A page that names a conf file names the one this launcher reads: the
	// state root follows CCMORPH_STATE_DIR and XDG_CONFIG_HOME, so a redirected
	// root — not the default spelling — is where the advice takes effect.
	if l.dir != "" {
		body = strings.ReplaceAll(body, "~/.config/ccmorph/", l.confDirSpelling()+"/")
	}
	fmt.Println(wrapBody(body, width))
	if pickerTopics[topic] {
		fmt.Println("\n" + wrapBody(pickerNote, width))
	}
	return 0
}

// pickerNote teaches a picker's keys. It ends only the pages whose command can
// open one (#263); a page for a command that never shows a picker stays silent.
const pickerNote = "Pickers: move with the arrow keys, Enter selects, Esc to cancel.\n" +
	"Profile and model lists also let you type to search (with fzf installed);\n" +
	"without fzf, type the number or part of the name of your choice."

// pickerTopics are the help pages whose command, used as its page teaches, can
// open a picker: an omitted argument, -i, or a menu the command always shows.
var pickerTopics = map[string]bool{"run": true, "add": true, "config": true, "use": true,
	"inspect": true, "doctor": true, "models": true, "get": true, "set": true, "login": true,
	"cred": true, "probe": true, "context": true}

var usageTopics = map[string][2]string{
	"run": {"ccmorph run [profile] [-i] [-- <claude arguments...>]", `Omit the profile to use your default. -i asks for a profile if needed,
then a model for routed profiles; a gateway with no model saved is
asked on a plain run too. Explicit --model <id> skips the model picker.
Choices apply to this session only; save a default with
ccmorph set <profile> model <id>.

Examples:
  ccmorph run work
  ccmorph run work --model sonnet
  ccmorph run work -- --continue

Native Claude help: ccmorph run <profile> --help`},
	"add": {"ccmorph add <profile>", `Choose how to sign in. Only the selected method asks for credentials:
  Claude account: sign in to Claude with your browser.
  Claude token: a long-lived token for scripts/CI; quota cannot read plan usage.
  API key: credential presented to Anthropic or a gateway, checked with that
  endpoint before the profile is saved. The provider list opens with Anthropic
  itself, then known gateways.

Credentials are hidden while typing. Ctrl-C cancels setup.`},
	"config": {"ccmorph config [profile]", `Choose what to change: model and effort, or connection/sign-in details.
Only settings relevant to that task and profile are shown.
Keep the current defaults or select new values. Changes save when you finish;
there is no extra confirmation. Esc cancels a picker without saving.
Changes affect new sessions.

For a single setting: ccmorph set <profile> model|effort [value]

Masking is a line in the profile's conf file, not a menu item: on an API-key
gateway profile, env.CCMORPH_MASK=1 in ~/.config/ccmorph/<profile>.conf sends
secrets as placeholders; ccmorph inspect shows whether it is on.`},
	"list": {"ccmorph list", "List saved profiles. * marks the default used by ccmorph run; private marks one whose history is deleted when the session ends."},
	"quota": {"ccmorph quota [profile] [--json]", `Every profile in one view, grouped by kind: Claude accounts and tokens,
Codex subscriptions, budget accounts, and API key or provider profiles. Each
plan window is a bar with the percent used and when it resets; a plan tier
comes from the provider's own account data. Plan usage is read for Claude
account and Codex profiles; a pasted Claude token cannot read it. A
rate-limited or unreachable read shows the last good reading and its age.
Gateways and API keys show today's and this month's billed spend from their
receipts, and the account balance where the provider publishes one (DeepSeek,
Kimi, GLM, OpenRouter, SiliconFlow, StepFun). --json prints the same data for
tools. No inference, nothing billed.
Omit the profile to list every profile.`},
	"use": {"ccmorph use [profile|-i]", "Set the default without launching. No argument shows the current default."},
	"inspect": {"ccmorph inspect [profile|-i]", `Show effective settings and their sources, never the stored credential.
Omit the profile to inspect the default; -i opens the profile picker.

Masking replaces secrets with placeholders before they reach the endpoint and
puts the real values back in replies. It is off unless the profile's conf file
(~/.config/ccmorph/<profile>.conf) sets env.CCMORPH_MASK=1, and it covers
API-key gateway routes only: a Claude account or api.anthropic.com profile
keeps its direct connection, where nothing is masked.`},
	"doctor": {"ccmorph doctor [--verbose] [profile|-i]", `Read-only checks of the executable, sign-in and endpoint, ending with whether
the profile is ready. --verbose adds a running adapter's processes and ports.
Omit the profile to check the default. Does not repair or launch a session.`},
	"models": {"ccmorph models [profile] [--filtered|--filtered-json]", `List models from an API-key endpoint. Omit the profile to choose one.
--filtered applies profile exclusions; --filtered-json returns JSON.`},
	"get": {"", `Read the default model or effort level that new sessions use; nothing changes.
Omit the profile or the setting to choose it in a picker.
The value is printed to stdout; name both in scripts.
Effort: ` + strings.Join(profile.Efforts, ", ") + `. Available models come from the profile.`},
	"set": {"", `Save a default for new sessions: the model or the effort level.
Omit the profile, the setting or the value to choose it in a picker.
Name all three in scripts.
Effort: ` + strings.Join(profile.Efforts, ", ") + `. Available models come from the profile.`},
	"login": {"ccmorph login [profile|-i]", `Sign in again: a Claude account profile with its browser login, a
provider-backed profile with its own, or let Claude Code use an API-key
profile's key again after it was refused. To replace an API key or token
instead: ccmorph config <profile>`},
	"mv": {"ccmorph mv <old> <new>", "Rename a profile and its private state. Existing destinations are refused."},
	"rm": {"ccmorph rm <profile>", `Delete this profile, its saved credential and private sign-in state.
Shared conversation history is kept. This cannot be undone.
Leftover private state from a missing profile requires confirmation.`},
	"hop": {"ccmorph hop [profile]", `Inside a running Claude Code session, type one of:
  /ccmorph:hop <profile>
  ! ccmorph hop <profile>
The switch happens at once: hop ends this session and the relay resumes the
conversation on the new profile, keeping its model and effort when that profile
serves them. No exit is needed.
Without a profile it names the other profiles to choose from; hopping to the
profile already running changes nothing.
This command does not work from an unrelated terminal.`},
	"profile": {"ccmorph profile", `Inside a running Claude session, type: ! ccmorph profile
It prints the profile the session runs on. The status line names the same
profile without a command, and a hop changes both.`},
	"cred": {"ccmorph cred [profile]", `Print the stored API key or token to stdout. This reveals a secret.
Only profiles with a stored key or token are offered. Name one in scripts.`},
	"update": {"ccmorph update [ref]", `Download the latest release and run its installer, which updates this install
in place. Give a branch or tag to install that instead: ccmorph update v0.1.0.
Profiles, credentials and local.sh are kept, and the installer names what it kept.
A git checkout is refused: update it with git.
Needs the install.sh that sits beside the library; without it, run the one-line
install from the README once.`},
	"uninstall": {"ccmorph uninstall", `Remove the ccmorph command from ~/.local/bin. Your profiles and credentials stay
in ~/.config/ccmorph; it lists what it left (state, usage receipts, keychain
items) with the commands that remove them, if you want them gone.
Needs the install.sh that sits beside the library.`},
	"probe": {"ccmorph probe [profile] [options]", `Look up each model's context window size, the most tokens one session can hold,
in what the gateway publishes and in the model catalogue, and save the sizes for
new sessions. Omit the profile to look up every API-key, proxy and provider
profile. A model with no size found still works, at Claude Code's 200,000-token
default. No inference request is made unless --allow-inference.

Options:
  -i                         Choose the profile from a list
  --profile <name>           Look up this profile; repeat for more
  --model <id>               Only this model; repeat for more, or -i to choose
  --check                    Show what would change and save nothing
  --declared-only            Use published sizes only, no inference (default)
  --allow-inference          Measure --model with a large prompt; may be billed
  --probe-all                With --allow-inference, measure a sized model too
  --bisect                   If a refusal names no limit, try shorter prompts
  --bisect-accepts <n>       Stop --bisect after n accepted prompts (default 1)
  --probe-timeout <seconds>  Longest wait for each gateway reply (default 10)
  --deadline <seconds>       Time limit for the whole run (default 30)
  --listing-stdin            Size a model listing read from stdin (used by run)
  --jobs <n>                 Ignored; kept so older scripts still work
  -h, --help                 Show this page

--allow-inference needs one profile and one --model; --probe-all and --bisect
need --allow-inference.`},
	"context": {"ccmorph context <profile> <model> [tokens|auto]", `Read a context limit, set a known token count, or restore automatic discovery.
Normally unnecessary: compatibility and context limits are automatic.`},
	"help": {"ccmorph help [command|all]", `Show the everyday commands, every command (all), or one command's page.
-h or --help after any command shows that command's page.`},
	"version": {"ccmorph version", "Print the installed version of ccmorph. Same as ccmorph --version, -v or -V."},
}
