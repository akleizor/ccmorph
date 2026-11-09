package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

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
	l.status("FAIL", "claude executable: not found on PATH")
	return false
}

// doctorEnvironment is what a fresh install can say before any profile exists.
func (l *launcher) doctorEnvironment() int {
	failed := 0
	l.status("OK", "ccmorph: "+l.version+" ("+l.libDir+")")
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
	l.status("INFO", "state: "+l.dir+" (no profiles yet) — next: ccmorph add <name>")
	return failed
}

func (l *launcher) doctor(name string, verbose bool) int {
	active := filepath.Join(l.dir, "active")
	if name == "" && isFile(active) && !isLink(active) {
		b, _ := os.ReadFile(active)
		name = strings.TrimRight(string(b), "\n")
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
	conf := filepath.Join(l.dir, name+".conf")
	if !present(conf) {
		l.status("FAIL", "profile: "+l.noProfileMsg(name))
		return 1
	}
	if !isFile(conf) || isLink(conf) {
		l.status("FAIL", "profile: missing or symlinked config ("+conf+")")
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		l.status("FAIL", "config: invalid profile schema (launch would reject it)")
		return 1
	}
	typ, base, adapter := f["type"], f["base_url"], f["adapter"]
	key := f["api_key"]
	if key == "" {
		key = f["token"]
	}
	if typ == "" {
		l.status("FAIL", "config: missing type")
		return 1
	}
	failed := 0
	var fails, warns []string
	// report prints one line and remembers its subject (the text before the
	// first colon) so the verdict can name what to fix without repeating it.
	report := func(kind, msg string) {
		subject, _, _ := strings.Cut(msg, ":")
		switch kind {
		case "FAIL":
			if !slices.Contains(fails, subject) {
				fails = append(fails, subject)
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
		report("FAIL", "config: unsupported profile type '"+typ+"'")
		failed = 1
	}
	if typ == "apikey" && key == "" {
		report("FAIL", "config: apikey profile has no credential (value not displayed)")
		failed = 1
	}
	report("OK", "profile: "+name+" ("+typ+")")
	report("OK", "ccmorph executable: "+l.libDir+"/ccmorph.sh")
	if !l.doctorClaude() {
		failed = 1
		fails = append(fails, "claude executable")
	}
	if dir := filepath.Join(l.profileDirs, name); isDir(dir) {
		report("OK", "profile home: "+dir)
	} else {
		report("FAIL", "profile home: missing ("+dir+")")
		failed = 1
	}
	settings := l.profileLayer(name)
	if isFile(settings) {
		if util.SettingsValid(filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), settings) {
			report("OK", "profile settings: valid layered JSON ("+settings+" overrides canonical settings)")
			if m := l.accountModel(name); m != "" {
				report("INFO", "effective model: "+m+" (profile default)")
			} else {
				report("INFO", "effective model: Claude Code default (no saved model)")
			}
		} else {
			report("FAIL", "profile settings: invalid JSON or non-object ("+settings+")")
			failed = 1
		}
	} else {
		report("INFO", "profile settings: none yet (Claude Code defaults apply)")
	}
	if key != "" {
		report("OK", "credentials: present (value hidden)")
	} else {
		switch typ {
		case "sub":
			report("INFO", "credentials: Claude sign-in, kept in the profile home")
		case "provider":
			report("INFO", "credentials: provider login (renew it: ccmorph login "+name+")")
		default:
			report("SKIP", "credentials: none stored")
		}
	}
	if typ == "apikey" && key != "" {
		// A refused key is stored by Claude Code, not by ccmorph: the credential
		// is present and the route may be healthy, yet every launch opens the
		// login menu instead of using it.
		switch util.KeyResponse(filepath.Join(l.profileDirs, name), key) {
		case util.KeyRefused:
			report("FAIL", "api key: Claude Code was told not to use it, so a launch would open its login menu — fix: ccmorph login "+name)
			failed = 1
		case util.KeyApproved:
			report("OK", "api key: approved for Claude Code")
		default:
			report("INFO", "api key: ccmorph approves it for Claude Code at the next launch")
		}
	}
	rows, bad := util.DoctorRoute(name, typ, base, adapter == "true", key, verbose)
	if bad {
		failed = 1
	}
	for _, r := range rows {
		report(r.Kind, r.Msg)
	}
	// The verdict is the report's last line and answers the only question a
	// person has: is this profile ready to run, or what must be fixed first.
	switch {
	case len(fails) > 0:
		l.status("FAIL", "not ready: fix "+strings.Join(fails, ", ")+" (see above)")
	case len(warns) > 0:
		l.status("WARN", "may not start: check "+strings.Join(warns, ", ")+" (see above); then: ccmorph run "+name)
	default:
		l.status("OK", "ready: start it with ccmorph run "+name)
	}
	return failed
}
