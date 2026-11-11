package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"os"
	"path/filepath"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
)

// Verb is `ccmorph-core cli <verb> [args]`: the `ccmorph` verbs that run in the
// core. The zsh dispatcher has already secured the state dirs.
func Verb(args []string) int {
	l := fromEnv()
	l.theme()
	if len(args) == 0 {
		return 2
	}
	if args[0] == "main" {
		return l.dispatch(args[1:])
	}
	return l.verb(args)
}

func (l *launcher) verb(args []string) int {
	switch args[0] {
	case "list":
		return l.list(args[1:])
	case "use":
		return l.use(args[1:])
	case "cred":
		return l.cred(args[1:])
	case "inspect":
		return l.inspect(args[1:])
	case "context":
		return l.context(args[1:])
	case "models", "_models":
		return l.models(args[1:])
	case "rm":
		return l.rm(args[1:])
	case "mv":
		return l.mv(args[1:])
	case "get", "set":
		return l.getSet(args[0], args[1:])
	case "_ui":
		return l.uiVerb(args[1:])
	case "_menu":
		return l.modelMenu(args[1:])
	case "config":
		return l.config(args[1:])
	case "add":
		return l.add(args[1:])
	case "login":
		return l.login(args[1:])
	case "hop":
		return l.hop(args[1:])
	case "profile":
		return l.profileVerb(args[1:])
	case "quota":
		return l.quota(args[1:])
	}
	fmt.Fprintln(os.Stderr, "usage: ccmorph-core cli <verb>")
	return 2
}

// globQuote quotes a literal path for use in front of a glob pattern. The zsh
// globbed a quoted "$_CCMORPH_DIR" followed by the pattern's own
// metacharacters, so the directory stayed literal; a joined Go pattern parses
// the whole path as syntax, and a `[` or `*` in it matches the wrong thing.
func globQuote(path string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`, `[`, `\[`).Replace(path)
}

// confNames is "$_CCMORPH_DIR"/*.conf(N), by name.
func (l *launcher) confNames() []string {
	confs, _ := filepath.Glob(filepath.Join(globQuote(l.dir), "*.conf"))
	var names []string
	for _, c := range confs {
		names = append(names, strings.TrimSuffix(filepath.Base(c), ".conf"))
	}
	return names
}

func (l *launcher) active() string {
	name, ok := l.readActive()
	if !ok {
		return ""
	}
	return name
}

// accountModel is _ccm_account_model: the profile's saved model, never written.
func (l *launcher) accountModel(name string) string {
	v, rc := l.configStoreQ("get", name, "model")
	if rc != 0 || strings.ContainsAny(v, "\n\r") {
		return ""
	}
	return v
}

func providerLabel(p string) string { return profile.ProviderName(p) }

func (l *launcher) accountsHint() {
	if names := l.confNames(); len(names) > 0 {
		l.err("profiles: " + strings.Join(names, " "))
	}
}

// noProfilesMsg is the one sentence a fresh install hears from every command
// that needs a profile, never indented, each command in its own voice. The probe
// core says it too, so the text is owned by ctxwindow.
const noProfilesMsg = ctxwindow.NoProfilesMsg

// noProfileMsg is the one answer every verb gives a name with no .conf: the
// profiles that do exist, or how to make the first.
func (l *launcher) noProfileMsg(name string) string {
	if twin := l.caseTwin(name); twin != "" {
		return "no profile '" + name + "'. Did you mean '" + twin + "'? Profile names match letter case exactly."
	}
	if names := l.confNames(); len(names) > 0 {
		return "no profile '" + name + "'. Profiles: " + strings.Join(names, " ")
	}
	return "no profile '" + name + "'. " + noProfilesMsg
}

// caseClashMsg names the profile a new or renamed name collides with on a
// volume that ignores letter case. It never offers rm: that would delete the
// other profile.
func caseClashMsg(name, twin string) string {
	return "'" + name + "' clashes with the existing profile '" + twin + "' (the names differ only by letter case). Choose another name."
}

// existsMsg is what add says for a name already taken (#391), one line per
// l.err. It names config, the command that changes a profile. A browser
// sign-in (a Claude account or a provider) is renewed by login and never
// hears rm; any other kind hears rm only as starting over, with what it deletes.
func existsMsg(name, typ string) []string {
	restart := "To start over instead: ccmorph rm " + name + " deletes the profile and its saved credential for good, then ccmorph add " + name
	switch typ {
	case "sub", "provider":
		return []string{
			"'" + name + "' already exists. Sign in to it again: ccmorph login " + name,
			"To change its model or effort: ccmorph config " + name,
		}
	case "apikey":
		return []string{"'" + name + "' already exists. To change its key, endpoint, model or effort: ccmorph config " + name, restart}
	case "token":
		return []string{"'" + name + "' already exists. To replace its token or change its model or effort: ccmorph config " + name, restart}
	}
	return []string{"'" + name + "' already exists. To change it: ccmorph config " + name, restart}
}

func (l *launcher) noProfile(name string) { l.err(l.noProfileMsg(name)) }

// badNameMsg is profile.BadNameMsg plus the profiles that do exist, when any:
// one line, the same from every verb, and the list that spots a typo the way
// the no-profile reply does (#228, #373).
func (l *launcher) badNameMsg(name string) string {
	msg := profile.BadNameMsg(name)
	if names := l.confNames(); len(names) > 0 {
		msg += ". Profiles: " + strings.Join(names, " ")
	}
	return msg
}

// badName is the one refusal every verb gives a word that can never be a
// profile name (#373). A name left out is a usage fault, not a bad word.
func (l *launcher) badName(verb, name string) int {
	if name == "" {
		l.err("usage: " + usageLine(verb))
		return 1
	}
	l.err(l.badNameMsg(name))
	return 1
}

func (l *launcher) list(args []string) int {
	// `list` has no options, so a word after it (a --json that never existed,
	// say) was silently ignored and read as success (#268). Name it instead.
	if len(args) > 0 {
		l.err("list takes no arguments: '" + args[0] + "'")
		return 2
	}
	active, found := l.active(), false
	for _, n := range l.confNames() {
		f, ok := l.readRaw(n)
		if !ok {
			fmt.Fprintf(os.Stderr, "ccmorph: skipping invalid profile '%s'\n", n)
			continue
		}
		marker, nameFmt := " ", n
		if n == active {
			marker, nameFmt = l.accent+"*"+l.reset, l.bold+n+l.reset
		}
		label := l.dim + profile.KindLabel(f["type"], f["provider"]) + l.reset
		arrow := func(m string) string {
			if m == "" {
				return ""
			}
			return " → " + m
		}
		switch f["type"] {
		case "sub":
			if f["email"] != "" {
				label = l.cyan + "Claude account" + l.reset + l.dim + ": " + f["email"] + l.reset
			}
		case "token":
			label = l.yellow + "Claude token" + l.reset
		case "apikey":
			base := f["base_url"]
			if base == "" {
				base = "api.anthropic.com"
			}
			label = l.cyan + "API key" + l.reset + l.dim + ": " + base + arrow(l.accountModel(n)) + l.reset
		case "provider":
			m := l.accountModel(n)
			p := f["provider"]
			if p == "" {
				p = "provider"
			}
			label = l.accent + providerLabel(p) + l.reset
			if m != "" {
				label += l.dim + arrow(m) + l.reset
			}
		}
		if f["private"] == "1" {
			// Marked where profiles are listed; inspect and doctor say what the
			// mark means (#292).
			label += " " + l.yellow + "· private" + l.reset
		}
		fmt.Printf(" %s %s (%s)\n", marker, nameFmt, label)
		found = true
	}
	if !found {
		fmt.Println(l.dim + noProfilesMsg + l.reset)
	}
	return 0
}

func (l *launcher) cred(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if (name == "" || name == "-i" || name == "--interactive") && isTTY(0) {
		picked, ok := l.menuProfile("apikey", "token")
		if !ok {
			return 1
		}
		name = picked
	}
	if interactive(name) {
		name = ""
	}
	if !profile.ValidName(name) {
		return l.badName("cred", name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		return 1
	}
	c := f["api_key"]
	if c == "" {
		c = f["token"]
	}
	if c == "" {
		l.err("'" + name + "' is a " + profile.KindLabel(f["type"], f["provider"]) + " profile and stores no credential here")
		l.err("sign in again: ccmorph login " + name)
		return 1
	}
	if isTTY(1) {
		fmt.Println(c)
	} else {
		fmt.Print(c)
	}
	return 0
}

func (l *launcher) use(args []string) int {
	if len(args) == 0 || args[0] == "" {
		if a := l.active(); a != "" {
			fmt.Println(l.bold + a + l.reset)
		} else if names := l.confNames(); len(names) == 0 {
			// #368: a fresh install hears the one sentence every command gives,
			// not the command it just typed with no profile to name.
			fmt.Println(l.dim + noProfilesMsg + l.reset)
		} else {
			// #368: profiles exist but none is the default, so the answer names
			// the profiles to choose from, never the command just typed.
			fmt.Println(l.dim + "No default set. Profiles: " + strings.Join(names, " ") + l.reset)
		}
		return 0
	}
	name := args[0]
	if name == "-i" || name == "--interactive" {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		return l.badName("use", name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	f, ok := l.read(name)
	if !ok || !l.setupDir(name, f["type"], liveSilent) {
		return 1
	}
	if err := atomicWrite(filepath.Join(l.dir, "active"), name+"\n"); err != nil {
		l.err("could not set default profile")
		return 1
	}
	fmt.Println(l.accent + "Default set" + l.reset + " to " + l.bold + name + l.reset)
	return 0
}

// atomicWrite is _ccm_atomic_write_unlocked: a 0600 temp beside the target, renamed over it.
func atomicWrite(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return util.AtomicWrite(path, ".ccmorph.tmp.", []byte(content), 0o600, false)
}

// modelMenu is `ccmorph _menu [profile]`: the model picker, chained from the
// profile chooser when no profile is named.
func (l *launcher) modelMenu(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if name == "" {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		return l.badName("models", name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	v, ok := l.pickValue("model", name)
	if !ok {
		return 1
	}
	fmt.Println(v)
	return 0
}
