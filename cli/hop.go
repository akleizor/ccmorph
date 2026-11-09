package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/akleizor/ccmorph/profile"
)

func relayValid(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if !(r == '_' || r == '.' || r == '-' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

// hop is `ccmorph hop [profile]`: queue a relaunch of this Claude Code session
// on another profile, then end the session so the relay picks it up.
func (l *launcher) hop(args []string) int {
	session, relay := os.Getenv("CLAUDE_CODE_SESSION_ID"), os.Getenv("CCMORPH_RELAY_ID")
	relayPID, _, _ := strings.Cut(relay, ".")
	name, current := "", false
	if len(args) > 0 {
		name = args[0]
	}
	if interactive(name) {
		if session == "" || !relayValid(relay) {
			l.err("profile switching requires a running ccmorph session")
			return 1
		}
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if name == "" && relay != "" {
		if n, err := strconv.Atoi(relayPID); err == nil {
			if v := util.SessionAdapter(n); v != "" {
				name, current = v, true
			}
		}
	}
	if cfg := os.Getenv("CLAUDE_CONFIG_DIR"); name == "" && relay != "" && cfg != "" {
		// Like zsh's :A, resolve what exists: the profile dir itself may not yet.
		parent, dirs := resolvePath(filepath.Dir(cfg)), resolvePath(l.profileDirs)
		if parent != "" && parent == dirs {
			name, current = filepath.Base(cfg), true
		}
	}
	if !profile.ValidName(name) {
		l.err("usage: ccmorph hop [profile]")
		return 1
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	if _, ok := l.read(name); !ok {
		return 1
	}
	if session == "" {
		l.err("not inside a Claude Code session")
		return 1
	}
	if !relayValid(relay) {
		l.err("this session was not launched by the ccmorph relay")
		return 1
	}
	relayN, err := strconv.Atoi(relayPID)
	pid := 0
	if err == nil {
		pid = util.HopTarget(relayN, os.Getppid())
	}
	if pid <= 0 {
		l.err("cannot find the Claude Code process to end. Run hop from the Claude Code prompt (/ccmorph:hop or ! ccmorph hop)")
		return 1
	}
	if !l.hopCheck(name) {
		return 1
	}
	signal := "profile=" + name + "\nsession=" + session
	if model, effort, ok := l.sessionState(session); ok {
		if current || l.modelKnown(name, model) != 1 {
			signal += "\nmodel=" + model
		} else {
			fmt.Printf("%sccmorph:%s %s%s%s is not served by %s%s%s — resuming on its default model.\n",
				l.yellow, l.reset, l.bold, model, l.reset, l.bold, name, l.reset)
		}
		signal += "\neffort=" + effort
	}
	file := filepath.Join(l.dir, ".reswitch."+relay)
	if atomicWrite(file, signal+"\n") != nil {
		l.log("hop FAILED to write relay target=" + name + " relay_id=" + relay)
		l.err("could not queue switch")
		return 1
	}
	l.log("hop queued target=" + name + " session=" + session + " relay_id=" + relay)
	fmt.Println(l.accent + "Switching" + l.reset + " → " + l.bold + name + l.reset)
	l.log("hop ends claude pid=" + strconv.Itoa(pid))
	if syscall.Kill(pid, syscall.SIGTERM) != nil {
		os.Remove(file)
		l.err("could not end Claude Code (pid " + strconv.Itoa(pid) + ")")
		return 1
	}
	return 0
}

// hopCheck is what a hop owes the session it is about to end (#286): the target
// must be able to start, or the switch is refused before anything closes and
// the session keeps running. It mirrors the target's own launch aborts — a key
// Claude Code was told to refuse, a provider whose bridge cannot start, and
// doctor's endpoint probe — so what hop refuses is what run would fail on. It
// never mutates state the relay pass would not touch anyway.
func (l *launcher) hopCheck(name string) bool {
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, func(string) {})
	if err != nil {
		return true // the target's own launch reports this in its own words
	}
	if typ == "provider" {
		if why := l.providerStartRefusal(name); why != "" {
			l.refuseHop(name, why)
			return false
		}
	} else if k, ok := envGet(env, "ANTHROPIC_API_KEY"); ok && k != "" && !l.keyApproval(name, k) {
		// keyApproval said why in the launch path's own words; the session
		// survives, and `ccmorph login <name>` is the fix, not doctor.
		fmt.Fprintln(os.Stderr, "  This session keeps running.")
		return false
	}
	if why := l.routeRefusal(name); why != "" {
		l.refuseHop(name, why)
		fmt.Fprintln(os.Stderr, "  Check the route: ccmorph doctor "+name)
		fmt.Fprintln(os.Stderr, "  This session keeps running.")
		return false
	}
	return true
}

// refuseHop is a hop refused before the session it runs in was ended.
func (l *launcher) refuseHop(name, why string) {
	l.err("not switching to '" + name + "': " + why)
	fmt.Fprintln(os.Stderr, "  This session keeps running.")
}

// sessionState is _ccm_session_state: the transcript's last model and effort.
func (l *launcher) sessionState(id string) (string, string, bool) {
	if id == "" || strings.ContainsAny(id, "\r\n") {
		return "", "", false
	}
	transcript, ok := l.sessionFile(id)
	if !ok {
		return "", "", false
	}
	return util.SessionState(transcript)
}

// resolvePath is an absolute, symlink-resolved path, or "" when p is unset.
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	abs, _ := filepath.Abs(p)
	return abs
}
