package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

// doctorVerb is `ccmorph doctor [profile|-i]`: read-only, so it runs before the
// state dirs are secured and falls back to a not-yet-migrated ~/.claude-profiles.
func (l *launcher) doctorVerb(args []string) int {
	if len(args) > 1 {
		l.err("usage: ccmorph doctor [profile]")
		return 2
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
	return l.doctor(name)
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
		v, rc := l.utilQ(nil, "", "rc-interpreter")
		switch {
		case rc == 0:
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

func (l *launcher) doctor(name string) int {
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
	switch typ {
	case "apikey", "sub", "token", "provider":
	default:
		l.status("FAIL", "config: unsupported profile type '"+typ+"'")
		failed = 1
	}
	if typ == "apikey" && key == "" {
		l.status("FAIL", "config: apikey profile has no credential (value not displayed)")
		failed = 1
	}
	l.status("OK", "profile: "+name+" ("+typ+")")
	l.status("OK", "ccmorph executable: "+l.libDir+"/ccmorph.sh")
	if !l.doctorClaude() {
		failed = 1
	}
	if dir := filepath.Join(l.profileDirs, name); isDir(dir) {
		l.status("OK", "profile home: "+dir)
	} else {
		l.status("FAIL", "profile home: missing ("+dir+")")
		failed = 1
	}
	settings := l.profileLayer(name)
	if isFile(settings) {
		if _, rc := l.utilQ(nil, "", "settings-valid", filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), settings); rc == 0 {
			l.status("OK", "profile settings: valid layered JSON ("+settings+" overrides canonical settings)")
			if m := l.accountModel(name); m != "" {
				l.status("INFO", "effective model: "+m+" (profile default)")
			} else {
				l.status("INFO", "effective model: Claude Code default (no saved model)")
			}
		} else {
			l.status("FAIL", "profile settings: invalid JSON or non-object ("+settings+")")
			failed = 1
		}
	} else {
		l.status("SKIP", "profile settings: missing ("+settings+")")
	}
	if key != "" {
		l.status("OK", "credentials: present (value hidden)")
	} else {
		l.status("SKIP", "credentials: none stored")
	}
	out, rc := l.utilQ(nil, key+"\n", "doctor-route", name, base, adapter)
	if rc != 0 {
		failed = 1
	}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "\t"); ok {
			l.status(k, v)
		} else if line != "" {
			fmt.Println(line)
		}
	}
	l.status("SKIP", "generated state: ownership/conflict not established (no changes made)")
	return failed
}
