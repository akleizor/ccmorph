package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	"context": true, "cred": true, "secret": true, "creds": true, "probe": true, "hop": true,
	"help": true, "version": true, "run": true, "start": true, retiredVerb: true}

// dispatch is the `ccmorph` command: every verb, help and the launch's own argv.
func (l *launcher) dispatch(args []string) int {
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch first {
	case "", "-h", "--help", "help":
		if len(args) > 2 {
			l.err("usage: ccmorph help [command|all]")
			return 2
		}
		topic := ""
		if len(args) == 2 {
			topic = args[1]
		}
		return l.usage(topic)
	case "-v", "-V", "--version", "version":
		fmt.Println("ccmorph " + l.version)
		return 0
	case "_backend_env":
		for _, k := range l.backend {
			fmt.Println(k)
		}
		return 0
	}
	if len(args) == 2 && (args[1] == "-h" || args[1] == "--help") && first != "run" && first != "probe" && first != retiredVerb {
		return l.usage(first)
	}
	switch first {
	case "update":
		if len(args) > 2 {
			l.err("usage: ccmorph update [ref]")
			return 2
		}
		return l.runInstaller("update", args[1:])
	case "uninstall":
		if len(args) != 1 {
			l.err("usage: ccmorph uninstall")
			return 2
		}
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
		"_menu", "_ui", "config", "add", "login", "hop", "quota":
		return l.verb(args)
	}
	if strings.HasPrefix(first, "-") {
		l.err("unknown command '" + first + "'. Launch flags go after the verb: ccmorph run " + first)
	} else {
		l.err("unknown command '" + first + "'. Run: ccmorph --help")
		if l.exists(first) {
			fmt.Fprintln(os.Stderr, "  To launch that profile: ccmorph run "+first)
		}
	}
	l.accountsHint()
	return 1
}

func (l *launcher) retired(verb, now string, args []string) int {
	msg := "unknown command '" + verb + "'. Run: ccmorph " + now
	if len(args) > 1 {
		msg += " " + strings.Join(args[1:], " ")
	}
	l.err(msg)
	return 1
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
			if _, rc := l.util(nil, "", "migrate-profile-home", legacyHome, l.profileDirs); rc != 0 {
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
			l.utilQ(nil, "", "normalize-plugin-index", home, l.profileDirs)
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
	account := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if l.exists(args[0]) {
			account, args = args[0], args[1:]
			if len(args) > 0 && (args[0] == "-i" || args[0] == "--interactive") {
				interactive, args = true, args[1:]
			}
		} else if !l.isClaudeSubcommand(args[0]) {
			if !profile.ValidName(args[0]) {
				l.err("invalid profile name '" + args[0] + "'")
				return 2
			}
			l.err("no profile '" + args[0] + "'. Run: ccmorph list")
			return 1
		}
	}
	if account == "" && l.active() == "" && !hasI {
		if len(l.confNames()) == 0 {
			l.err("no profiles yet — run: ccmorph add <name>")
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
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		a, args = append(a, "--profile", args[0]), args[1:]
	}
	a = append(a, args...)
	if mi := lastIndex(a, "--model"); mi >= 0 && (mi+1 >= len(a) || a[mi+1] == "" || interactive(a[mi+1])) {
		pi := lastIndex(a, "--profile")
		if pi < 0 || pi+1 >= len(a) {
			l.err("usage: ccmorph probe <profile> --model -i")
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
	for _, v := range strings.Split(l.claudeTable("claude-verbs"), "\n") {
		if v != "" && v == word {
			return true
		}
	}
	return false
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
				arity = map[string]string{}
				for _, line := range strings.Split(l.claudeArity(), "\n") {
					if k, v, ok := strings.Cut(line, " "); ok {
						arity[k] = v
					}
				}
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
	out, rc := l.utilQ(nil, "", "profile-env", l.dir, l.profileDirs, name,
		strings.Join(l.profileEnv, ","), strings.Join(l.mapping, ","))
	if rc != 0 {
		if out == "" {
			out = "could not read profile '" + name + "'"
		}
		l.err(out)
		return 1
	}
	head, rest, _ := strings.Cut(out, "\n")
	if !l.setupDir(name, strings.TrimPrefix(head, "type=")) {
		return 1
	}
	l.prelaunch(name)
	if rest != "" {
		fmt.Println(rest)
	}
	return 0
}

// routeEnv is `ccmorph _route_env <profile>`: _env, plus a started provider bridge
// and the handle that retires it.
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
	if l.envVerb(args) != 0 {
		l.providerStop()
		return 1
	}
	fmt.Println("ANTHROPIC_BASE_URL=http://127.0.0.1:" + l.providerPort)
	fmt.Println("ANTHROPIC_API_KEY=" + l.providerKey)
	fmt.Println("CCMORPH_ROUTE_HANDLE=provider|" + l.providerPID + "|" + l.providerTD + "|" + l.providerAuth)
	return 0
}

// routeStop retires a route _route_env started; only a provider ever opens one.
func (l *launcher) routeStop(args []string) int {
	if len(args) == 0 || !strings.HasPrefix(args[0], "provider|") {
		return 0
	}
	parts := strings.Split(args[0], "|")
	for len(parts) < 4 {
		parts = append(parts, "")
	}
	if !digits.MatchString(parts[1]) {
		return 1
	}
	l.providerPID, l.providerTD, l.providerAuth = parts[1], parts[2], parts[3]
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

func (l *launcher) usage(topic string) int {
	help := func(k, t string) { grid(32, k, t) }
	head := func(s string) { fmt.Println("\n" + l.bold + s + l.reset) }
	switch topic {
	case "":
		fmt.Println("ccmorph — Claude Code profile switcher")
		fmt.Println("\nUsage: ccmorph <command> [arguments]")
		head("Launch:")
		help("ccmorph run", "Launch the default profile")
		help("ccmorph run <profile>", "Use a profile for this session")
		help("ccmorph run -i", "Choose a profile interactively")
		head("Profiles:")
		help("ccmorph add <profile>", "Add an account or API key")
		help("ccmorph list", "See profiles and the default")
		help("ccmorph quota [profile]", "Plan usage and spend of every profile")
		help("ccmorph use <profile>", "Set the default; do not launch")
		help("ccmorph config [profile]", "Change model, reasoning, or connection")
		head("Troubleshooting:")
		help("ccmorph inspect [profile]", "Show effective settings; no secrets")
		help("ccmorph doctor [profile]", "Check why a profile is not working")
		fmt.Println("\nStart here: ccmorph add work")
		fmt.Println("More commands: ccmorph help all")
		fmt.Println("Command details: ccmorph help <command>")
		fmt.Println("Version: ccmorph --version")
		return 0
	case "all":
		l.usage("")
		head("More commands:")
		help("ccmorph models <profile>", "List models from an API-key endpoint")
		help("ccmorph get <profile> <model|effort>", "Read a saved default")
		help("ccmorph set <profile> <model|effort> [value]", "Save a default; omit value to choose")
		help("ccmorph login <profile>", "Sign in again to a provider account")
		help("ccmorph mv <old> <new>", "Rename a profile")
		help("ccmorph rm <profile>", "Delete a profile and its private sign-in state")
		help("ccmorph hop <profile>", "Switch a running session; use inside Claude")
		help("ccmorph cred <profile>", "Print a stored secret (sensitive)")
		help("ccmorph update [ref]", "Update to the latest version, or to a branch or tag")
		help("ccmorph uninstall", "Remove the command; profiles stay")
		head("Advanced troubleshooting (normally automatic):")
		help("ccmorph probe [profile]", "Refresh context-limit metadata; no inference")
		help("ccmorph context <profile> <model> [tokens|auto]", "Override or reset one model context limit")
		return 0
	}
	t, ok := usageTopics[topic]
	if !ok {
		l.err("unknown help topic '" + topic + "'. Run: ccmorph help all")
		return 2
	}
	usage := t[0]
	if topic == "get" || topic == "set" {
		usage = "ccmorph " + topic + " [profile] [model|effort]"
		if topic == "set" {
			usage += " [value]"
		}
	}
	fmt.Println("Usage: " + usage)
	fmt.Println()
	fmt.Println(t[1])
	fmt.Println("\nFixed choices: ↑/↓ move, Enter selects one, Esc cancels. No search box.")
	fmt.Println("Profile/model catalogues: type to search with fzf; numbers without it.")
	return 0
}

var usageTopics = map[string][2]string{
	"run": {"ccmorph run [profile] [-i] [-- <claude arguments...>]", `Omit the profile to use your default. -i asks for a profile if needed,
then a model for routed profiles. Explicit --model <id> skips the model picker.
Choices apply to this session only; saved defaults are unchanged.

Examples:
  ccmorph run work
  ccmorph run work --model sonnet
  ccmorph run work -- --continue

Native Claude help: ccmorph run <profile> --help`},
	"add": {"ccmorph add <profile>", `Choose how to sign in. Only the selected method asks for credentials:
  Subscription: sign in to Claude with your browser.
  API key: credential presented to Anthropic or a gateway (apikey).
  OAuth token: paste an existing Claude token.
  Provider: browser login, when a supported provider bridge is installed.

Credentials are hidden while typing. Ctrl-C cancels setup.`},
	"config": {"ccmorph config [profile]", `Choose what to change: model and reasoning, or connection/sign-in details.
Only settings relevant to that task and profile are shown.
Keep the current defaults or select new values. Changes save when you finish;
there is no extra confirmation. Esc cancels a picker without saving.
Changes affect new sessions.

For a single setting: ccmorph set <profile> model|effort [value]`},
	"list": {"ccmorph list", "List saved profiles. * marks the default used by ccmorph run."},
	"quota": {"ccmorph quota [profile] [--json]", `Every profile in one view, grouped by kind: Claude subscriptions, Codex
subscriptions, budget accounts, and API or gateway profiles. Each plan window is a
bar with the percent used and when it resets; a plan tier comes from the provider's
own account data. A rate-limited or unreachable read shows the last good reading
and its age. Gateways and API keys show today's and this month's billed spend from
their receipts, and the account balance where the provider publishes one (DeepSeek,
Kimi, GLM, OpenRouter, SiliconFlow, StepFun). --json prints the same data for tools. No inference, nothing billed.
Omit the profile to list every profile.`},
	"use": {"ccmorph use [profile|-i]", "Set the default without launching. No argument shows the current default."},
	"inspect": {"ccmorph inspect [profile|-i]", `Show effective settings and their sources, never the stored credential.
Omit the profile to inspect the default; -i opens the profile picker.`},
	"doctor": {"ccmorph doctor [profile|-i]", `Read-only checks of the executable, endpoint and generated state.
Omit the profile to check the default. Does not repair or launch a session.`},
	"models": {"ccmorph models [profile] [--filtered|--filtered-json]", `List models from an API-key endpoint. Omit the profile to choose one.
--filtered applies profile exclusions; --filtered-json returns JSON.`},
	"get": {"", `Read or save a default for new sessions. Missing arguments open pickers.
Use explicit arguments in scripts. Values are printed to stdout.
Effort: low, medium, high, xhigh. Available models come from the profile.`},
	"login": {"ccmorph login [profile|-i]", `Sign in again to a provider-backed profile using its browser login.
To replace an API key or token instead: ccmorph config <profile>`},
	"mv": {"ccmorph mv <old> <new>", "Rename a profile and its private state. Existing destinations are refused."},
	"rm": {"ccmorph rm <profile>", `Delete this profile, its saved credential and private sign-in state.
Shared conversation history is kept. This cannot be undone.
Leftover private state from a missing profile requires confirmation.`},
	"hop": {"ccmorph hop <profile>", `Inside a running Claude session, type: ! ccmorph hop <profile>
Then exit Claude to complete the switch. Keeps model and effort when supported.
This command does not work from an unrelated terminal.`},
	"cred": {"ccmorph cred [profile]", `Print the stored API key or token to stdout. This reveals a secret.
Only profiles with a stored key or token are offered. Name one in scripts.`},
	"update": {"ccmorph update [ref]", `Download the latest release and run its installer, which updates this install
in place. Give a branch or tag to install that instead: ccmorph update v0.1.0.
Profiles, credentials and local.sh are kept, and the installer names what it kept.
A git checkout is refused: update it with git.
Needs the install.sh that sits beside the library; without it, run the one-line
install from the README once.`},
	"uninstall": {"ccmorph uninstall", `Remove the ccmorph command from ~/.local/bin. Your profiles and credentials stay
in ~/.config/ccmorph; delete that directory by hand if you want them gone.
Needs the install.sh that sits beside the library.`},
	"probe": {"ccmorph probe [profile] [options]", `Refresh published context metadata; no inference requests by default.
Paid inference requires --allow-inference, --profile and --model explicitly.
Full probe options: ccmorph probe --help`},
	"context": {"ccmorph context <profile> <model> [tokens|auto]", `Read a context limit, set a known token count, or restore automatic discovery.
Normally unnecessary: compatibility and context limits are automatic.`},
}

func init() { usageTopics["set"] = usageTopics["get"] }
