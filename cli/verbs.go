package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

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
	switch args[0] {
	case "list":
		return l.list()
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

func (l *launcher) configStoreQ(verb, name, key string) (string, int) {
	args := append([]string{"config-store", verb, l.conf(name), l.profileLayer(name), key, l.canonical()}, l.mapping...)
	return l.utilQ(nil, "", args...)
}

func providerLabel(p string) string {
	if p == "xai" {
		return "xAI"
	}
	if p == "" {
		return p
	}
	r := []rune(p)
	return string(unicode.ToUpper(r[0])) + strings.ToLower(string(r[1:]))
}

func (l *launcher) accountsHint() {
	if names := l.confNames(); len(names) > 0 {
		l.err("profiles: " + strings.Join(names, " "))
	}
}

func (l *launcher) list() int {
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
		label := l.dim + f["type"] + l.reset
		arrow := func(m string) string {
			if m == "" {
				return ""
			}
			return " → " + m
		}
		switch f["type"] {
		case "sub":
			if f["email"] != "" {
				label = l.cyan + "sub" + l.reset + l.dim + ": " + f["email"] + l.reset
			}
		case "token":
			label = l.yellow + "token" + l.reset
		case "apikey":
			base := f["base_url"]
			if base == "" {
				base = "api.anthropic.com"
			}
			label = l.cyan + "apikey" + l.reset + l.dim + ": " + base + arrow(l.accountModel(n)) + l.reset
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
		fmt.Printf(" %s %s (%s)\n", marker, nameFmt, label)
		found = true
	}
	if !found {
		fmt.Println(l.dim + "  No profiles. Run: ccmorph add <name>" + l.reset)
	}
	return 0
}

func (l *launcher) menuProfile(types ...string) (string, bool) {
	picked, ok := l.zshUI(`_ccm_menu_profile "$@"`, types...)
	return picked, ok && picked != ""
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
	if !profile.ValidName(name) {
		l.err("usage: ccmorph cred <profile>")
		return 1
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'")
		l.accountsHint()
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
		l.err("'" + name + "' is a " + f["type"] + " profile and stores no credential here")
		l.err("subscription profiles use browser login: ccmorph login " + name)
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
		} else {
			fmt.Println(l.dim + "No default set. Run: ccmorph use <profile>" + l.reset)
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
		l.err("invalid name — use alphanumeric/underscore")
		return 1
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'. Run: ccmorph list")
		return 1
	}
	f, ok := l.read(name)
	if !ok || !l.setupDir(name, f["type"]) {
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
	tmp, err := os.CreateTemp(dir, ".ccmorph.tmp.")
	if err != nil {
		return err
	}
	_, werr := tmp.WriteString(content)
	cerr := tmp.Close()
	if werr == nil && cerr == nil {
		werr = os.Chmod(tmp.Name(), 0o600)
	}
	if werr == nil && cerr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil || cerr != nil {
		os.Remove(tmp.Name())
		if werr != nil {
			return werr
		}
		return cerr
	}
	return nil
}
