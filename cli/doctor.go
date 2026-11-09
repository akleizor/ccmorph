package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/util"
)

// authStatusDeadline bounds doctor's `claude auth status`: Claude Code owns the
// sign-in and can wait on a person (a keychain prompt) or wedge, and doctor's
// promise is a verdict, never a hang. A variable so the test can shorten it.
var authStatusDeadline = 10 * time.Second

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
	if _, err := os.Lstat(l.profileDirs); err != nil && isDir(legacy) && !isLink(legacy) {
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
	l.doctorCompletion()
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

// doctorCompletion says how to make zsh find the completion this install
// carries. Like the PATH row it reports and never edits: it stays silent when
// a directory on FPATH already holds `_ccmorph` or the .zshrc names the
// directory, and for a bash or fish login shell. Homebrew links the file into
// its own share/zsh/site-functions, which `brew shellenv` puts on FPATH, so
// there the FPATH check keeps this row quiet.
func (l *launcher) doctorCompletion() {
	dir := filepath.Join(filepath.Dir(l.libDir), "share", "zsh", "site-functions")
	if !isFile(filepath.Join(dir, "_ccmorph")) {
		return
	}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "bash", "fish":
		return
	}
	for _, d := range filepath.SplitList(os.Getenv("FPATH")) {
		if d == dir || isFile(filepath.Join(d, "_ccmorph")) {
			return
		}
	}
	zdot := os.Getenv("ZDOTDIR")
	if zdot == "" {
		zdot = os.Getenv("HOME")
	}
	if b, err := os.ReadFile(filepath.Join(zdot, ".zshrc")); err == nil &&
		(strings.Contains(string(b), dir) || strings.Contains(string(b), "ccmorph/share/zsh/site-functions")) {
		return
	}
	l.status("INFO", "completion: zsh does not search "+dir+" yet — add this line to "+filepath.Join(zdot, ".zshrc")+
		" above compinit (or Oh My Zsh): fpath=("+shellWord(dir)+" $fpath)")
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
	active := filepath.Join(l.dir, "active")
	fromDefault := false
	if name == "" && isFile(active) && !isLink(active) {
		b, _ := os.ReadFile(active)
		name = strings.TrimRight(string(b), "\n")
		fromDefault = true
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
		l.status("FAIL", "profile: invalid profile name")
		return 1
	}
	// A default written by an older ccmorph carries the spelling the person
	// typed; read it as the profile that is really there, as use and list do.
	if fromDefault && !l.exists(name) {
		if twin := l.caseTwin(name); twin != "" {
			name = twin
		}
	}
	conf := filepath.Join(l.dir, name+".conf")
	recreate := "edit " + conf + " to fix it, or re-create the profile: ccmorph rm " + name + " && ccmorph add " + name
	// GH-259: on a volume that ignores letter case present(conf) is true for a
	// name typed with the wrong case, and doctor read tok.conf as TOK and
	// recommended `ccmorph run TOK`. Only a directory entry spelled as typed is
	// this profile; every other verb decides the same way.
	if !l.exists(name) {
		if isLink(conf) {
			l.status("FAIL", "profile: missing or symlinked config ("+conf+")")
		} else {
			l.status("FAIL", "profile: "+l.noProfileMsg(name))
		}
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		l.status("FAIL", "config: invalid profile schema (launch would reject it)")
		l.status("FAIL", "not ready: "+recreate)
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
	home := filepath.Join(l.profileDirs, name)
	if isDir(home) {
		report("OK", "profile home: "+home)
	} else {
		report("FAIL", "profile home: missing ("+home+")", "the first launch creates it: ccmorph run "+name)
		failed = 1
	}
	// The conf's one silent promise, said aloud where the profile's other
	// behaviour is reported (#292).
	if f["private"] == "1" {
		report("INFO", "history: deleted when the session ends (private=1)")
	}
	settings := l.profileLayer(name)
	// The effective model is named below and adjudicated by DoctorRoute when the
	// listing answers (#267); read it once, read-only.
	model := l.accountModel(name)
	if isFile(settings) {
		// Each file is judged on its own, so the line names the one that is
		// actually broken: the shared canonical file and the profile's file are
		// fixed in different places.
		broken := ""
		for _, p := range []string{filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), settings} {
			if !util.SettingsValid(p) {
				broken = p
				break
			}
		}
		if broken == "" {
			report("OK", "profile settings: valid JSON ("+settings+")")
			if model != "" {
				report("INFO", "effective model: "+model+" (profile default)")
			} else {
				report("INFO", "effective model: Claude Code default (no saved model)")
			}
		} else {
			report("FAIL", "profile settings: invalid JSON or not an object ("+broken+"); fix that file",
				"fix the JSON in "+broken)
			failed = 1
		}
	} else {
		report("INFO", "profile settings: none yet (Claude Code defaults apply)")
	}
	// The route rows are computed before the api-key lines print, because the
	// endpoint's own verdict decides whether Claude Code's locally stored answer
	// is worth a line: an approval says the key was accepted locally, which a
	// rejection below would contradict two lines later (GH-272). The translator
	// verdict comes from what a launch itself reads without inference (the
	// classification cache and the listing's declared protocols), so doctor
	// reports what launch would actually do, never a guess.
	need := func(listing string) util.TranslatorNeed {
		needed, known := adapter.TranslatorVerdict(base, l.profileDirs, model, listing)
		return util.TranslatorNeed{Known: known, Needed: needed, Installed: adapter.TranslatorInstalled()}
	}
	rows, bad, keyRejected := util.DoctorRoute(name, typ, base, model, f["model_map"], need, key, home, verbose)
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
	if bad {
		failed = 1
	}
	for _, r := range rows {
		report(r.Kind, r.Msg, r.Fix)
	}
	l.doctorCompletion()
	// A masking mismatch is a privacy gap, not a start failure: the profile
	// still runs, so the row warns without weighing on the verdict (#293).
	if on, _ := util.MaskOn(f["env.CCMORPH_MASK"]); on && !util.Maskable(typ, base) {
		l.status("WARN", "masking: env.CCMORPH_MASK="+f["env.CCMORPH_MASK"]+
			" is set but nothing is masked — this route reaches its backend without ccmorph's shim; masking covers gateway (apikey) routes only")
	}
	// The verdict is the report's last line and answers the only question a
	// person has: is this profile ready to run, or what must be fixed first.
	switch {
	case firstFix != "":
		l.status("FAIL", "not ready: "+firstFix)
	case len(warns) > 0:
		l.status("WARN", "may not start: check "+strings.Join(warns, ", ")+" (see above); then: ccmorph run "+name)
	default:
		l.status("OK", "ready: start it with ccmorph run "+name)
	}
	return failed
}
