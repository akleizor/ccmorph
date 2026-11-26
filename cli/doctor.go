package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
	"github.com/akleizor/ccmorph/util/paths"
)

// authStatusDeadline bounds doctor's `claude auth status`: Claude Code owns the
// sign-in and can wait on a person (a keychain prompt) or wedge, and doctor's
// promise is a verdict, never a hang. A variable so the test can shorten it.
var authStatusDeadline = 10 * time.Second

// translatorInstalled is adapter.TranslatorInstalled. A variable so a test can
// pin the not-installed verdict: a machine with cliproxyapi at one of launch's
// fixed paths would otherwise decide it, and the row this issue is about is
// the one that says the translator is missing.
var translatorInstalled = adapter.TranslatorInstalled

// signInStatus asks Claude Code whether it holds a sign-in for the profile's
// config dir. answered is false when claude did not finish in time — a hang is
// "cannot tell", never a guess. The answer is read from the output: a real
// signed-out status still prints its JSON and exits 1.
func (l *launcher) signInStatus(name string) (out []byte, answered bool) {
	ctx, cancel := context.WithTimeout(context.Background(), authStatusDeadline)
	defer cancel()
	cmd := l.claudeAuthCtx(ctx, filepath.Join(l.profileDirs, name), "status")
	// A wedged claude's helpers can outlive it holding the output pipe; after
	// the kill, wait at most this long for the pipe before reading what came.
	cmd.WaitDelay = time.Second
	out, _ = cmd.CombinedOutput()
	if ctx.Err() != nil {
		return nil, false
	}
	return out, true
}

// doctorVerb is `ccmorph doctor [--verbose] [profile|-i]`: read-only, so it runs
// before the state dirs are secured and falls back to a not-yet-migrated
// ~/.claude-profiles. --verbose adds a running adapter's processes and ports.
func (l *launcher) doctorVerb(args []string) int {
	verbose := false
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if a == "-v" || a == "--verbose" {
			verbose = true
			continue
		}
		rest = append(rest, a)
	}
	args = rest
	if len(args) > 1 {
		return l.extraArg("doctor", args[1])
	}
	legacy := filepath.Join(os.Getenv("HOME"), ".claude-profiles")
	if _, err := os.Lstat(l.profileDirs); err != nil && paths.IsDir(legacy) && !paths.IsSymlink(legacy) {
		l.profileDirs = legacy
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	if interactive(name) {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	return l.doctor(name, verbose)
}

func (l *launcher) status(kind, msg string) {
	symbols := map[string][2]string{"OK": {"✓", "d77757"}, "FAIL": {"✗", "ff6b80"},
		"WARN": {"!", "ffc107"}, "INFO": {"ℹ", "b1b9f9"}, "SKIP": {"–", "999999"}}
	s, ok := symbols[kind]
	switch {
	case !ok:
		fmt.Println(kind + " " + msg)
	case isTTY(1) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb":
		var r, g, b int
		fmt.Sscanf(s[1], "%02x%02x%02x", &r, &g, &b)
		fmt.Printf("\033[38;2;%d;%d;%dm%s\033[0m %s\n", r, g, b, s[0], msg)
	case isTTY(1):
		fmt.Println(s[0] + " " + msg)
	default:
		fmt.Println(kind + " " + msg)
	}
}

// doctorClaude is the claude row; false only when it printed a FAIL.
func (l *launcher) doctorClaude() bool {
	if p, err := exec.LookPath("claude"); err == nil {
		l.status("OK", "claude executable: "+p)
		return true
	}
	l.status("FAIL", "claude executable: "+util.ClaudeMissing)
	return false
}

// doctorEnvironment is what a fresh install can say before any profile exists.
func (l *launcher) doctorEnvironment() int {
	failed := 0
	l.status("OK", "ccmorph: "+l.version)
	if v := l.zshVersion; v != "" {
		l.status("OK", "zsh: "+v)
	}
	if _, err := os.Lstat(filepath.Join(l.dir, "rc-hosts")); err == nil {
		v, ok := util.RCInterpreter()
		switch {
		case ok:
			l.status("OK", "remote control interpreter: "+v)
		case v != "":
			l.status("FAIL", "remote control interpreter: "+v+" (need 3.8 or newer)")
			failed = 1
		default:
			l.status("FAIL", "remote control interpreter: not found (need 3.8 or newer)")
			failed = 1
		}
	}
	if !l.doctorClaude() {
		failed = 1
	}
	tool := func(name, missing string) {
		if p, err := exec.LookPath(name); err == nil {
			l.status("INFO", name+": "+p)
		} else {
			l.status("INFO", name+": not found ("+missing+")")
		}
	}
	tool("fzf", "numbered menus")
	tool("cliproxyapi", "Claude-family models only on gateways")
	if p, err := exec.LookPath("ccmorph"); err == nil {
		l.status("OK", "PATH: ccmorph is on PATH ("+filepath.Dir(p)+")")
	} else {
		l.status("INFO", "PATH: ccmorph is not on PATH (add the directory it was installed to)")
	}
	l.doctorCompinit(l.doctorCompletion())
	l.status("INFO", "state: "+l.dir+" (no profiles yet) — next: ccmorph add <name>")
	return failed
}

// shellWord spells s as one shell word: bare when it is plain, else in single quotes.
func shellWord(s string) string {
	plain := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+,:@%=", r)) {
			plain = false
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// completionProbeDeadline bounds the zsh the completion row starts; a shell
// that cannot answer in time is read as "cannot tell": the row then says what
// the startup files still miss — never a guess, and never silence, which would
// read as a working completion. A variable so the test can shorten it.
var completionProbeDeadline = 5 * time.Second

// completionBound asks a new zsh the only question that matters: does that
// shell have a completion bound to the ccmorph command? It starts the login
// shell a new terminal would ($SHELL, or zsh from PATH) with ZDOTDIR forced to
// zdot so the person's own startup files are the ones that load, and FPATH
// dropped so the shell rebuilds fpath from those files exactly as a fresh
// terminal does — a leaked FPATH would replace the whole array and hide even
// compinit. answered is false when no marker came back (no shell, a broken
// rc, a timeout); bound is true only when the shell named a completion. The
// shell may write a zcompdump or touch nothing else a terminal's shells do
// not; nothing of the person's is edited.
func (l *launcher) completionBound(zdot string) (bound, answered bool) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		if p, err := exec.LookPath("zsh"); err == nil {
			shell = p
		} else {
			return false, false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), completionProbeDeadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-ilc",
		`print -r -- "__ccmorph_completion_${_comps[ccmorph]-}"`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "FPATH=") || strings.HasPrefix(kv, "ZDOTDIR=") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env, "ZDOTDIR="+zdot)
	out, _ := cmd.Output()
	if ctx.Err() != nil {
		return false, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "__ccmorph_completion_"); ok {
			return v != "", true
		}
	}
	return false, false
}

// zshCompletion is what doctor read about Tab completion in a new zsh (#363):
// zsh completes ccmorph only when the install's directory is on fpath and
// something has run compinit, so the hint has two halves, each its own method.
// ask is false when there is nothing to say: no completion file installed, or a
// bash or fish login shell.
type zshCompletion struct {
	ask      bool
	dir      string // the install's share/zsh/site-functions
	rcName   string // the .zshrc as a person writes it: ~/.zshrc under HOME
	onFpath  bool   // the startup files or FPATH already reach a _ccmorph
	compinit bool   // the .zshrc runs compinit or the Oh My Zsh loader
	bound    bool   // a new zsh answered with a completion bound to ccmorph
}

// doctorCompletion reads what a new zsh would have and prints the fpath half
// of the completion hint: the fpath line, when zsh does not reach the
// directory, or where it belongs, when it sits below compinit. When zsh
// already reaches the file the fpath half is done and this prints nothing;
// doctorCompinit, given what this returns, says whether compinit is the half
// still missing. Doctor calls the two together, so the whole hint goes quiet
// only when a new zsh answers that it completes the ccmorph command (see
// completionBound) — a shell that cannot answer is never a working verdict.
// The whole hint fits in two short lines at an 80-column terminal. Like the
// PATH row it reports and never edits. Homebrew links the file into its own
// share/zsh/site-functions, which `brew shellenv` puts on fpath; there the
// missing half, if any, is only the line that starts completions.
func (l *launcher) doctorCompletion() zshCompletion {
	c := zshCompletion{dir: filepath.Join(filepath.Dir(l.libDir), "share", "zsh", "site-functions")}
	if !paths.IsFile(filepath.Join(c.dir, "_ccmorph")) {
		return c
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "bash", "fish":
		return c
	}
	c.ask = true
	zdot := os.Getenv("ZDOTDIR")
	if zdot == "" {
		zdot = os.Getenv("HOME")
	}
	rcFile := filepath.Join(zdot, ".zshrc")
	c.rcName = rcFile
	if home := os.Getenv("HOME"); home != "" {
		if rest, ok := strings.CutPrefix(rcFile, home+"/"); ok {
			c.rcName = "~/" + rest
		}
	}
	rc, _ := os.ReadFile(rcFile)
	c.onFpath = util.FpathBound(rc, c.dir, os.Getenv("FPATH"))
	c.compinit = util.CompinitRuns(rc)
	bound, answered := l.completionBound(zdot)
	c.bound = answered && bound
	if c.bound {
		return c
	}
	// Every line is its own row: doctor's report keeps one status word a line.
	switch {
	case c.onFpath && c.compinit:
		l.status("INFO", "completion: put the fpath line above compinit in "+c.rcName+":")
		l.status("INFO", "  fpath=("+shellWord(c.dir)+" $fpath)")
	case c.onFpath:
		// zsh reaches the file; doctorCompinit names the line it still misses.
	case c.compinit:
		l.status("INFO", "completion: add to "+c.rcName+", above compinit (or Oh My Zsh):")
		l.status("INFO", "  fpath=("+shellWord(c.dir)+" $fpath)")
	default:
		l.status("INFO", "  fpath=("+shellWord(c.dir)+" $fpath)")
	}
	return c
}

// doctorCompinit prints the compinit half of the completion hint from what
// doctorCompletion read: nothing when a new zsh completes ccmorph or the
// .zshrc already runs compinit, the compinit line under a short lead when the
// directory is on fpath, or — when the fpath half printed its line — the
// compinit line carrying the lead as a comment, so the two read as one hint.
func (l *launcher) doctorCompinit(c zshCompletion) {
	switch {
	case !c.ask || c.bound || c.compinit:
	case c.onFpath:
		l.status("INFO", "completion: nothing in "+c.rcName+" runs compinit — add:")
		l.status("INFO", "  autoload -Uz compinit && compinit")
	default:
		l.status("INFO", "  autoload -Uz compinit && compinit # add both lines to "+c.rcName)
	}
}

// credentialStorage says where a profile's credential is kept: the login
// keychain when the conf carries only its keychain reference, the conf file
// when it holds the value itself. The caller's read has already resolved the
// reference into the value, so the raw conf is what answers; the value never
// appears in the answer.
func credentialStorage(conf, field string) string {
	if _, raw, err := ctxwindow.ReadConf(conf); err == nil && strings.HasPrefix(raw[field], "keychain:") {
		return "in the login keychain"
	}
	return "in the conf file, unencrypted"
}

func (l *launcher) doctor(name string, verbose bool) int {
	if name == "" {
		// readActiveFile, the read the launch's readActive stands on — not
		// readActive itself: doctor keeps its own reading of the default. An
		// empty or unreadable one is no profile selected, not a refusal, and
		// doctor never writes, where readActive re-tightens the file to 0600.
		if read, ok := l.readActiveFile(); ok {
			name = read
		}
	}
	if name == "" {
		if len(l.confNames()) == 0 {
			return l.doctorEnvironment()
		}
		l.status("FAIL", "profile: no profile selected (use: ccmorph doctor <profile>)")
		l.accountsHint()
		return 1
	}
	if !profile.ValidName(name) {
		return l.badName("doctor", name)
	}
	conf := filepath.Join(l.dir, name+".conf")
	recreate := "edit " + conf + " to fix it, or re-create the profile: ccmorph rm " + name + " && ccmorph add " + name
	// GH-259: on a volume that ignores letter case present(conf) is true for a
	// name typed with the wrong case, and doctor read tok.conf as TOK and
	// recommended `ccmorph run TOK`. Only a directory entry spelled as typed is
	// this profile; every other verb decides the same way.
	if !l.exists(name) {
		if paths.IsSymlink(conf) {
			l.status("FAIL", "profile: missing or symlinked config ("+conf+")")
		} else {
			l.status("FAIL", "profile: "+l.noProfileMsg(name))
		}
		return 1
	}
	f, readErr := l.readProfile(name, func(line string) { fmt.Fprintln(os.Stderr, line) })
	if readErr != nil {
		l.err(readErr.Error())
		l.status("FAIL", "config: invalid profile schema (launch would reject it)")
		if f["type"] == "sub" && f["email"] == "" {
			// GH-649: a sign-in interrupted before its email line is this
			// conf's whole story, and `login` is the one repair every other
			// verb already names for it — doctor may not point elsewhere.
			l.status("FAIL", "not ready: sign in again: ccmorph login "+name)
		} else if repair := profile.RepairOf(readErr); repair != "" {
			// GH-839: the conf refusal already names the one edit that repairs
			// it (`set … effort`, remove the unknown key), so the verdict
			// repeats it instead of a re-create that throws the profile away.
			l.status("FAIL", "not ready: "+repair)
		} else {
			l.status("FAIL", "not ready: "+recreate)
		}
		return 1
	}
	typ, base := f["type"], f["base_url"]
	key, keyField := f["api_key"], "api_key"
	if key == "" {
		key, keyField = f["token"], "token"
	}
	if typ == "" {
		l.status("FAIL", "config: missing type")
		l.status("FAIL", "not ready: "+recreate)
		return 1
	}
	failed := 0
	var warns []string
	// firstFix is the command or step that repairs the first failed check: the
	// verdict names it instead of pointing at the rows above (#282).
	firstFix := ""
	// warnFix is the step the first warning that names one carries: a WARN
	// verdict names it instead of the launch that warning would fail (#841).
	warnFix := ""
	// report prints one line and remembers its subject (the text before the
	// first colon) so a warning can be named in the verdict without repeating it.
	report := func(kind, msg string, fix ...string) {
		subject, _, _ := strings.Cut(msg, ":")
		switch kind {
		case "FAIL":
			step := "fix " + subject
			if len(fix) > 0 && fix[0] != "" {
				step = fix[0]
			}
			if firstFix == "" {
				firstFix = step
			}
		case "WARN":
			if !slices.Contains(warns, subject) {
				warns = append(warns, subject)
			}
			if warnFix == "" && len(fix) > 0 {
				warnFix = fix[0]
			}
		}
		l.status(kind, msg)
	}
	switch typ {
	case "apikey", "sub", "token", "provider":
	default:
		report("FAIL", "config: unsupported profile type '"+typ+"'", recreate)
		failed = 1
	}
	if typ == "apikey" && key == "" {
		report("FAIL", "config: API key profile has no credential (value not displayed)",
			"set the API key: ccmorph config "+name)
		failed = 1
	}
	report("OK", "profile: "+name+" ("+profile.KindLabel(typ, f["provider"])+")")
	report("OK", "ccmorph: "+l.version+" (command: ccmorph)")
	if !l.doctorClaude() {
		failed = 1
		if firstFix == "" {
			firstFix = util.ClaudeMissing
		}
	}
	if typ == "provider" {
		// The launch's own refusal, in its words (providerStartRefusal, which
		// hop asks too): a bridge that cannot start is a profile that is not
		// ready (#787).
		if why := l.providerStartRefusal(name); why != "" {
			report("FAIL", "provider bridge: "+why, why)
			failed = 1
		} else {
			report("OK", "provider bridge: cliproxyapi installed, auth directory present")
		}
	}
	home := filepath.Join(l.profileDirs, name)
	// homeBlocker names the path that stops a launch from making the profile
	// home, with what to do about it; both empty when nothing does. The launch's
	// own MkdirAll (util.SetupProfile) walks these two paths, so doctor reads
	// them the same way: a path that is simply missing is ordinary absence — the
	// launch makes it — while anything else that is not a folder (Lstat under a
	// file answers ENOTDIR, which a bare exists-check misreads as absence), or
	// cannot be read, is the blocker the verdict must name.
	homeBlocker := func() (blocker, fix string) {
		for _, p := range []string{l.profileDirs, home} {
			fi, err := os.Lstat(p)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				// Not there yet: the first launch makes it.
			case err != nil:
				return err.Error(), "check the permissions on " + p
			case fi.IsDir():
			case fi.Mode()&os.ModeSymlink != 0:
				// A symlink to a folder is transparent to the launch; a
				// dangling one, or one to a file, is what blocks it.
				if s, e := os.Stat(p); e != nil || !s.IsDir() {
					return p + " is a symlink, not a folder",
						"remove " + p + " so the first launch can create the profile home: ccmorph run " + name
				}
			default:
				return p + " is not a directory",
					"remove " + p + " so the first launch can create the profile home: ccmorph run " + name
			}
		}
		return "", ""
	}
	blocker, fix := homeBlocker()
	switch {
	case paths.IsDir(home):
		report("OK", "profile home: "+home)
	case blocker != "":
		// GH-291: a launch genuinely cannot make the home — something other
		// than a folder sits at the home or where the state keeps its
		// profiles, or a path cannot be read — so this is a real failed
		// check, and its fix names the blocker and what clears it.
		report("FAIL", "profile home: cannot be created — "+blocker, fix)
		failed = 1
	default:
		// GH-291: the home is the launch's own artefact — run makes it, and so
		// does the login a refused key or a signed-out account needs — so a
		// profile copied in or hand-written, never yet launched, is ready: the
		// missing folder is a note, not a failed check.
		report("INFO", "profile home: not created yet ("+home+"); the first launch makes it: ccmorph run "+name)
	}
	// The conf's one silent promise, said aloud where the profile's other
	// behaviour is reported (#292).
	if f["private"] == "1" {
		report("INFO", "history: deleted when the session ends (private=1)")
	}
	settings := l.profileLayer(name)
	// The effective model is named below and adjudicated by DoctorRoute when the
	// listing answers (#267); read it once, read-only.
	savedModel := l.accountModel(name)
	model := savedModel
	// A tier word is a valid saved model (#475), and a launch resolves it to a
	// gateway id before it classifies one — resolveTier, the one spelling of
	// that rule, which this mirrored by hand until #517. The rows below ask
	// about that id, the one launch classifies; asking about the tier word left
	// a chat-dialect gateway behind an alias with no adapter row and a ready
	// verdict (#412, one branch over).
	tierMissing := ""
	if bare := util.TierWord(savedModel); util.IsTier(bare) && typ == "apikey" && !util.NativeBase(base) {
		if resolved, ok := resolveTier(savedModel,
			func(key string) (string, bool) { v, ok := f["env."+key]; return v, ok },
			func(tier string) (string, bool) { return l.tierAlias(name, tier) }); ok {
			model = resolved
		} else {
			tierMissing = bare
		}
	}
	broken := ""
	if paths.IsFile(settings) {
		// Each file is judged on its own, so the line names the one that is
		// actually broken: the shared canonical file and the profile's file are
		// fixed in different places.
		for _, p := range []string{filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), settings} {
			if !util.SettingsValid(p) {
				broken = p
				break
			}
		}
		if broken == "" {
			report("OK", "profile settings: valid JSON ("+settings+")")
		} else {
			report("FAIL", "profile settings: invalid JSON or not an object ("+broken+"); fix that file",
				"fix the JSON in "+broken)
			failed = 1
		}
	} else {
		report("INFO", "profile settings: none yet (a launch writes them)")
	}
	// The effective rows answer what a plain launch of this profile runs with, in
	// the words every screen shares for the unsaved state (#381). They stand with
	// or without a settings file — a profile never launched is ready (#291) — and
	// only a broken file above, or a store that cannot answer, leaves them out.
	if top, rc := l.configStoreQ("get", name, "effort"); rc == 0 && broken == "" {
		if model != "" {
			report("INFO", "effective model: "+model+" (profile default)")
		} else {
			report("INFO", "effective model: "+unsavedModelWords(typ, base)+" (no saved model)")
		}
		// The level is the one a plain launch passes (#356): the override saved
		// for the model it carries — a 1M claude model is sent under its [1m]
		// send id, and the launch keys this lookup by that id (runLaunch), so
		// doctor asks the resolver's own rule for it in place, as the launch's
		// model-window --fields does — else the profile's own default (#449).
		effortModel := savedModel
		if strings.HasPrefix(effortModel, "claude-") &&
			(typ == "provider" || (typ == "apikey" && !util.NativeBase(base))) {
			// The launch's model-window read migrates legacy state first; doctor
			// asks the resolver in place (#449), so it must move that state
			// forward itself or key the lookup by an id the launch would have
			// replaced (#556). The context half alone: doctor is otherwise
			// read-only, so it never moves credentials. A failed migration leaves
			// the saved spelling, as it leaves the launch's requested model.
			if l.migrateContextState() {
				if send := ctxwindow.LoadInputs(l.conf(name)).Resolve(effortModel).SendID; send != "" {
					effortModel = send
				}
			}
		}
		switch effort := l.savedEffort(name, effortModel); effort {
		case "":
			report("INFO", "effective effort: "+unsavedEffortWords+" (no saved effort)")
		case top:
			report("INFO", "effective effort: "+effort+" (profile default)")
		default:
			report("INFO", "effective effort: "+effort+" (profile override for "+effortModel+")")
		}
	}
	if tierMissing != "" {
		// Launch's own refusal (adapterModel), in its words: the alias, where it
		// came from, and the one command that picks a served model (#475, #862).
		report("FAIL", "model: "+util.UnservedTier(name, tierMissing, l.modelOrigin(name), nil), util.UnservedTierFix(name))
		failed = 1
	}
	// The route rows are computed before the api-key lines print, because the
	// endpoint's own verdict decides whether Claude Code's locally stored answer
	// is worth a line: an approval says the key was accepted locally, which a
	// rejection below would contradict two lines later (GH-272). The translator
	// verdict comes from what a launch itself reads without inference (the
	// classification cache and the listing's declared protocols), so doctor
	// reports what launch would actually do, never a guess. A tier word above is
	// already the id a launch resolves it to; then launch cuts the spelling at
	// its first '[' before the adapter sees it (util/adapterstart), translates
	// that wire id through model_map, and classifies the target; so the verdict
	// is asked about that id. A row decided for a model the launch never
	// classifies would call a profile ready that dies at adapter start (#412,
	// #475). A map that does not parse is launch's to reject; doctor then asks
	// about the wire id rather than inventing a translation.
	wire := strings.SplitN(model, "[", 2)[0]
	var modelMap map[string]any
	if raw := strings.TrimSpace(f["model_map"]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &modelMap); err != nil {
			modelMap = nil
		}
	}
	classified := adapter.LaunchModel(wire, modelMap)
	need := func(listing string) util.TranslatorNeed {
		needed, known := adapter.TranslatorVerdict(base, l.profileDirs, classified, listing)
		return util.TranslatorNeed{Known: known, Needed: needed, Installed: translatorInstalled()}
	}
	rows, bad, keyRejected, listing := util.DoctorRoute(name, typ, base, model, f["model_map"], need, key, home, verbose)
	if key != "" {
		// The endpoint refused it (a FAIL row below): "present" would call a
		// rejected credential fine above its FAIL (GH-272).
		if !keyRejected {
			report("OK", "credentials: present "+credentialStorage(conf, keyField)+" (value hidden)")
		}
	} else {
		switch typ {
		case "sub":
			// Claude Code owns this sign-in state, so ask it the same way add
			// does after a browser login — bounded, because a claude that
			// never answers must not hold the verdict. A real signed-out
			// status still prints its JSON and exits 1, so the answer is read
			// from the output, not the exit code.
			out, answered := l.signInStatus(name)
			m := claudeStatusRE.FindSubmatch(out)
			switch {
			case !answered:
				report("WARN", "credentials: cannot tell (claude auth status did not answer within 10 seconds)")
			case m == nil:
				report("WARN", "credentials: cannot tell (claude auth status reported no sign-in state)")
			case string(m[1]) != "true":
				report("FAIL", "credentials: not signed in — sign in again: ccmorph login "+name,
					"sign in again: ccmorph login "+name)
				failed = 1
			default:
				if e := emailRE.FindSubmatch(out); e != nil {
					report("OK", "credentials: signed in to Claude as "+string(e[1]))
				} else {
					report("OK", "credentials: signed in to Claude")
				}
			}
		case "provider":
			report("INFO", "credentials: provider login (renew it: ccmorph login "+name+")")
		default:
			report("SKIP", "credentials: none stored")
		}
	}
	if typ == "apikey" && key != "" && !keyRejected {
		// A refused key is stored by Claude Code, not by ccmorph: the credential
		// is present and the route may be healthy, yet every launch opens the
		// login menu instead of using it. Any other stored answer — or none yet
		// — means a launch uses the key without asking, which is all a reader
		// needs to know.
		switch util.KeyResponse(home, key) {
		case util.KeyRefused:
			report("FAIL", "api key: Claude Code was told not to use it, so a launch would open its login menu — fix: ccmorph login "+name,
				"let Claude Code use the key again: ccmorph login "+name)
			failed = 1
		default:
			report("OK", "api key: Claude Code will use it without asking")
		}
	}
	// Run's model stops, asked of the listing above (#862). They come before
	// the adapter row because a launch stops at the picker before any adapter
	// starts: the model repair, not a translator install, is the first stop the
	// verdict names. The no-model note promises a terminal picker, so it is
	// printed only when the listing leaves a row to pick.
	if typ == "apikey" && !util.NativeBase(base) && listing != "" {
		_, pickable := util.GwModels(conf, true, listing)
		switch {
		case util.ExclusionsLeaveNone(conf, listing):
			report("FAIL", "model: "+util.ExclusionsRefusal(name, conf), util.ExclusionsFix(name, conf))
			failed = 1
		case savedModel == "" && !need(listing).Known && pickable:
			report("INFO", "model: none saved; in a terminal, ccmorph run asks which model first, and without one it stops: "+util.NoModelReason(name))
		}
	}
	if bad {
		failed = 1
	}
	for _, r := range rows {
		report(r.Kind, r.Msg, r.Fix)
	}
	l.doctorCompinit(l.doctorCompletion())
	// A masking mismatch is a privacy gap, not a start failure: the profile
	// still runs, so the row warns without weighing on the verdict (#293).
	// A value the shim refuses is different: the launch stops in the shim, so
	// the row fails and the verdict names the value to replace (#411).
	maskOn, maskBad := util.MaskOn(f["env.CCMORPH_MASK"])
	maskable := util.Maskable(typ, base)
	switch {
	case maskable && maskBad:
		report("FAIL", "masking: env.CCMORPH_MASK="+f["env.CCMORPH_MASK"]+
			" is refused — a launch stops in the shim before a session starts (must be 1 or 0)",
			"set env.CCMORPH_MASK=1 or 0: ccmorph config "+name)
		failed = 1
	case maskOn && !maskable:
		l.status("WARN", "masking: env.CCMORPH_MASK="+f["env.CCMORPH_MASK"]+
			" is set but nothing is masked — this route reaches its backend without ccmorph's shim; masking covers gateway (apikey) routes only")
	}
	l.doctorRemoteControl(name, f)
	// The verdict is the report's last line and answers the only question a
	// person has: is this profile ready to run, or what must be fixed first.
	switch {
	case firstFix != "":
		l.status("FAIL", "not ready: "+firstFix)
	case len(warns) > 0:
		next := "then: ccmorph run " + name
		if warnFix != "" {
			next = warnFix
		}
		l.status("WARN", "may not start: check "+strings.Join(warns, ", ")+" (see above); "+next)
	default:
		l.status("OK", "ready: start it with ccmorph run "+name)
	}
	return failed
}

// doctorRemoteControl compares the tiers a hosted edge publishes it pins against
// the ones this profile itself maps (GH-881): until #815 the hosts supervisor
// kept its own hand-kept copy of the route profile's tier map, and a drifted
// copy served phone sessions model ids the route does not serve — the gateway
// 400 reached Alex as an adapter error on the phone before any machine said a
// word. The supervisor reconciles pins per tick now, so a row that still
// disagrees names a state the loop cannot clear (a stopped supervisor, a route
// profile whose read keeps failing): requested-vs-served for Remote Control,
// on the check that promises a plain reason for a broken route. The requested
// side is resolveTier, the one spelling launch uses (#517), overlaid by the
// `tier.` overrides the supervisor published. What is hosted is the
// supervisor's own parse() answer, read from hosted.json, because a Go copy of
// that grammar missed a shape in each of three review rounds — the supervisor
// publishes what only it knows (#881).
// An alias mapped to its own Claude family is not a pin (the native id is what
// the phone sends and the route serves), a `[1m]` marker names one model with
// or without it (ccmorph-rc's pinned_tiers), and a record naming another route
// is not this edge (#846). A drift warns without weighing on the verdict — the
// profile itself still launches, only the hosted route is stale (the masking
// rule of #293).
func (l *launcher) doctorRemoteControl(name string, f map[string]string) {
	raw, err := paths.ReadRegularFile(filepath.Join(l.dir, "profiles", ".rc-bridge", "hosted.json"))
	if err != nil {
		return // nothing published (no supervisor ran, or it stopped cleanly), or no regular file: no row, no waiting
	}
	var hosted struct {
		Routes    []string          `json:"routes"`
		Overrides map[string]string `json:"overrides"`
	}
	if json.Unmarshal(raw, &hosted) != nil || !slices.Contains(hosted.Routes, name) {
		return
	}
	overrides := hosted.Overrides
	// The served side is what the launcher published about its own edge; a
	// record naming another route is that route's, never this one's (#846).
	// Read through the store's nonblocking regular-file reader: a FIFO left
	// under either name must not hold doctor's verdict (the #654 rule).
	published := false
	served := map[string]string{}
	if b, err := paths.ReadRegularFile(filepath.Join(l.dir, "profiles", ".rc-bridge", "edges", name+".json")); err == nil {
		var rec map[string]any
		if json.Unmarshal(b, &rec) == nil {
			if r, named := rec["route"].(string); !named || r == name {
				if tiers, ok := rec["tiers"].(map[string]any); ok {
					published = true
					for alias, model := range tiers {
						if m, ok := model.(string); ok {
							served[alias] = m
						}
					}
				}
			}
		}
	}
	pin := func(m string) string { return strings.TrimSuffix(m, "[1m]") }
	want := map[string]string{}
	for _, tier := range util.TierWords {
		if m, ok := resolveTier(tier,
			func(key string) (string, bool) { v, ok := f["env."+key]; return v, ok },
			func(t string) (string, bool) { return l.tierAlias(name, t) }); ok &&
			!strings.HasPrefix(strings.ToLower(m), "claude-"+tier) {
			want[tier] = m
		}
	}
	for alias, model := range overrides {
		if slices.Contains(util.TierWords, alias) {
			want[alias] = model
		}
	}
	aliases := make([]string, 0, len(want)+len(served))
	for alias := range want {
		aliases = append(aliases, alias)
	}
	for alias := range served {
		if _, held := want[alias]; !held {
			aliases = append(aliases, alias)
		}
	}
	slices.Sort(aliases)
	if !published {
		// Nothing to compare: no edge has published for this route (or only a
		// twin's record, which is no one's live edge), and a fresh edge starts
		// on the current map — nothing is stale yet.
		l.status("INFO", "remote control: hosted; no edge record to compare (a running edge publishes one)")
		return
	}
	drifted := 0
	for _, alias := range aliases {
		w, wanted := want[alias]
		s, pinned := served[alias]
		w, s = pin(w), pin(s)
		switch {
		case wanted && pinned && w != s:
			l.status("WARN", "remote control: the hosted edge pins "+alias+"="+s+
				"; this profile maps "+alias+"="+w+" — phone sessions start on the old map")
			drifted++
		case wanted && !pinned:
			l.status("WARN", "remote control: the hosted edge pins no "+alias+" model; this profile maps "+
				alias+"="+w+" — phone sessions send the native id instead")
			drifted++
		case pinned && !wanted:
			l.status("WARN", "remote control: the hosted edge pins "+alias+"="+s+
				"; this profile maps no "+alias+" model — phone sessions get a model the profile does not name")
			drifted++
		}
	}
	if drifted > 0 {
		l.status("WARN", "remote control: restart the supervisor to repin every host: "+
			"launchctl kickstart -k gui/$(id -u)/com.ccmorph.rc-hosts")
		return
	}
	l.status("OK", "remote control: hosted; the edge pins the tiers this profile maps")
}
