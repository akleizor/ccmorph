package cli

import (
	"encoding/json"
	"fmt"
	"github.com/akleizor/ccmorph/router"
	"github.com/akleizor/ccmorph/shim"
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
	case "price":
		return l.price(args[1:])
	case "check":
		return l.check(args[1:])
	case "_plugin_dirs":
		// Launchers that exec claude themselves (ccmorph-rc) take the session's plugin path from here.
		fmt.Println(pluginDirs(l.libDir))
		return 0
	case "router":
		router.QuotaInput = l.quotaInput
		return router.Main(args[1:], os.Stdin, os.Stdout, os.Stderr)
	}
	fmt.Fprintln(os.Stderr, "usage: ccmorph-core cli <verb>")
	return 2
}

// priceCacheHint is the one repair sentence for an empty price cache: the plain
// answer, the JSON hint and the help page all print it (#636). No backticks: a
// help page refuses them.
const priceCacheHint = "Run a provider profile with ccmorph run <profile>; its launch refreshes this cache in the background."

// priceJSON prints one price answer as JSON, its hint's <profile> as written
// rather than \u003c-escaped.
func priceJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (l *launcher) price(args []string) int {
	models := make([]string, 0, len(args))
	asJSON := false
	// An option-shaped argument is refused, not looked up: `--jsoon` printed as
	// an unpriced model a script could not tell from a real answer. After --,
	// every argument is a model id, dash or not (#636).
	dashes := false
	for _, arg := range args {
		switch {
		case dashes:
			models = append(models, arg)
		case arg == "--":
			dashes = true
		case arg == "--json":
			asJSON = true
		case strings.HasPrefix(arg, "-") && arg != "-":
			l.err("unknown option '" + arg + "'")
			l.err("usage: " + usageLine("price"))
			fmt.Fprintln(os.Stderr, "  Use -- before a model id that starts with a dash.")
			return 2
		default:
			models = append(models, arg)
		}
	}
	if len(models) == 0 {
		l.err("usage: " + usageLine("price"))
		return 2
	}
	book := shim.CachedOwnedPriceBook("")
	type listPrice struct {
		Input  float64 `json:"input"`
		Output float64 `json:"output"`
	}
	type jsonPriceResult struct {
		Cache  string               `json:"cache"`
		Hint   string               `json:"hint,omitempty"`
		Prices map[string]listPrice `json:"prices"`
	}
	prices := map[string]listPrice{}
	if !book.HasCachedPrices() {
		if asJSON {
			result := jsonPriceResult{Cache: "empty", Hint: priceCacheHint, Prices: prices}
			if err := priceJSON(result); err != nil {
				l.err(err.Error())
				return 1
			}
		} else {
			fmt.Printf("No cached model prices. %s\n", priceCacheHint)
			for _, model := range models {
				fmt.Printf("%s  no cached price\n", model)
			}
		}
		return 0
	}
	for _, model := range models {
		input, output, ok := book.Lookup(model)
		if asJSON {
			if ok {
				prices[model] = listPrice{input, output}
			}
		} else if ok {
			fmt.Printf("%s  $%v/$%v per 1M tokens\n", model, input, output)
		} else {
			fmt.Printf("%s  unpriced\n", model)
		}
	}
	if asJSON {
		// One shape for both caches: the wrapper says which state it is in,
		// which a bare price map could not (#636).
		if err := priceJSON(jsonPriceResult{Cache: "ok", Prices: prices}); err != nil {
			l.err(err.Error())
			return 1
		}
	}
	return 0
}

// globQuote is util.GlobQuote under the name the cli's scans already call:
// a literal path in front of a glob pattern, so a `[` or `*` in a custom root
// matches the root's own name and not the wrong thing (#848).
func globQuote(path string) string { return util.GlobQuote(path) }

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
	valid, invalid := l.splitProfiles()
	msg := "no profile '" + name + "'. "
	switch {
	case len(valid) > 0:
		msg += "Profiles: " + strings.Join(valid, " ")
	case len(invalid) == 0:
		return msg + noProfilesMsg
	default:
		msg += "No valid profiles"
	}
	// A conf list skips doesn't read as a choice: it is named apart, with the
	// command that says why (#636).
	if len(invalid) > 0 {
		msg += "; not valid: " + strings.Join(invalid, " ") + " (ccmorph doctor " + invalid[0] + " says why)"
	}
	return msg
}

// splitProfiles is every conf, split into those that read as valid and those
// list would skip, each in name order.
func (l *launcher) splitProfiles() (valid, invalid []string) {
	for _, n := range l.confNames() {
		if _, ok := l.readQuiet(n); ok {
			valid = append(valid, n)
		} else {
			invalid = append(invalid, n)
		}
	}
	return valid, invalid
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
func (l *launcher) badNameMsg(name string) string { return l.nameRefusal(profile.BadNameMsg(name)) }

// rmBadNameMsg is badNameMsg in rm's voice: rm takes the legacy range, so the
// length reason names the bound rm applies (#551).
func (l *launcher) rmBadNameMsg(name string) string {
	return l.nameRefusal(profile.RemovableBadNameMsg(name))
}

// nameRefusal appends the profiles that do exist, when any, so a refusal's one
// line spots a typo the way the no-profile reply does.
func (l *launcher) nameRefusal(msg string) string {
	if names := l.confNames(); len(names) > 0 {
		msg += ". Profiles: " + strings.Join(names, " ")
	}
	return msg
}

// badName is the refusal a verb gives a word that can never be a profile name
// (#373). A name left out is a usage fault, not a bad word.
func (l *launcher) badName(verb, name string) int { return l.refuseName(verb, name, l.badNameMsg) }

// rmBadName is badName for the one verb whose name rule is wider than
// ValidName's: the refusal names rm's own bound (#551).
func (l *launcher) rmBadName(verb, name string) int { return l.refuseName(verb, name, l.rmBadNameMsg) }

// refuseName prints a name refusal and is the exit it gives.
func (l *launcher) refuseName(verb, name string, msg func(string) string) int {
	if name == "" {
		// With no profile at all, the usage line names an argument there is no
		// value for: the one first-run sentence is the answer (#636).
		if len(l.confNames()) == 0 {
			l.err(noProfilesMsg)
			return 1
		}
		l.err("usage: " + usageLine(verb))
		return 1
	}
	l.err(msg(name))
	return 1
}

// judgeProfile is the refusal pair a verb's preamble ends with: the word that
// can never be a name, then the name no profile answers to. The chooser a verb
// opens before it stays the verb's own — where the menu appears is each verb's
// pinned behaviour — but the refusals are the same words from every verb.
func (l *launcher) judgeProfile(name, verb string) bool {
	if !profile.ValidName(name) {
		l.badName(verb, name)
		return false
	}
	if !l.exists(name) {
		l.noProfile(name)
		return false
	}
	return true
}

// requireProfile is the verb preamble: the profile chooser a left-out or -i
// slot opens, limited to types when given, then judgeProfile's refusals.
func (l *launcher) requireProfile(name *string, verb string, types ...string) bool {
	if *name == "" || interactive(*name) {
		picked, ok := l.menuProfile(types...)
		if !ok {
			return false
		}
		*name = picked
	}
	return l.judgeProfile(*name, verb)
}

func (l *launcher) list(args []string) int {
	// `list` has no options, so a word after it (a --json that never existed,
	// say) was silently ignored and read as success (#268). Name it instead.
	if len(args) > 0 {
		l.err("list takes no arguments: '" + args[0] + "'")
		return 2
	}
	// The same description walk the profile menu renders from (#489): the
	// kind, the detail under it and the marks are decided once, so a profile
	// reads the same here as it does in the picker — a provider profile with
	// no saved model says Browser login, and a private one carries the mark.
	// What stays list's own is the paint, via kindColour below.
	descs, skipped := l.describeProfiles(l.active(), nil, nil)
	shown := false
	for _, d := range descs {
		marker, nameFmt := " ", d.name
		if d.active {
			marker, nameFmt = l.accent+"*"+l.reset, l.bold+d.name+l.reset
			shown = true
		}
		label := l.kindColour(d) + d.kind + l.reset
		if d.detail != "" {
			label += l.dim + ": " + d.detail + l.reset
		}
		// A saved default the row does not already show is named, so the
		// overview does not mislead by omission (#636).
		if m := l.accountModel(d.name); m != "" && !strings.Contains(d.detail, m) {
			label += l.dim + " · model " + m + l.reset
		}
		if e, rc := l.configStoreQ("get", d.name, "effort"); rc == 0 && e != "" && !strings.ContainsAny(e, "\n\r") {
			label += l.dim + " · effort " + e + l.reset
		}
		if d.private {
			// Marked where profiles are listed; inspect and doctor say what the
			// mark means (#292).
			label += " " + l.yellow + "· private" + l.reset
		}
		fmt.Printf(" %s %s (%s)\n", marker, nameFmt, label)
	}
	if shown {
		// #636: the `*` had no legend on first use; a newcomer had to guess
		// what the star meant. One line, only when a row carries it.
		fmt.Println(l.dim + "* = default profile (ccmorph use <profile> changes it)" + l.reset)
	}
	// A home whose only profile was skipped is not empty: the skip line already
	// named the fault and its repair, so the empty-home summary would be a
	// second, false answer (GH-839).
	if len(descs) == 0 && skipped == 0 {
		fmt.Println(l.dim + noProfilesMsg + l.reset)
	}
	return 0
}

// kindColour is `list`'s per-kind paint — the one deliberate difference from
// the profile menu (#489): the menu renders every kind in its single grey
// theme, while `list`, a screen of its own, colours each kind so the types
// read at a glance. The words come from the shared walk either way.
func (l *launcher) kindColour(d profileDesc) string {
	switch d.typ {
	case "token":
		return l.yellow
	case "provider":
		return l.accent
	default:
		return l.cyan
	}
}

func (l *launcher) cred(args []string) int {
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	if (name == "" || name == "-i" || name == "--interactive") && isTTY(0) {
		picked, ok := l.menuProfileCred()
		if !ok {
			return 1
		}
		name = picked
	}
	if interactive(name) {
		name = ""
	}
	if !l.judgeProfile(name, "cred") {
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
		return 1
	}
	c := credOf(f)
	if c == "" {
		l.err("'" + name + "' is " + profile.KindPhrase(f["type"], f["provider"]) + " profile and stores no credential here")
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

// credOf is the profile's resolved API key, or its token.
func credOf(f map[string]string) string {
	if f["api_key"] != "" {
		return f["api_key"]
	}
	return f["token"]
}

func (l *launcher) use(args []string) int {
	if len(args) == 0 || args[0] == "" {
		if a := l.active(); a != "" {
			// A terminal reads a sentence; a pipe keeps the bare name a
			// script captures (#636).
			if isTTY(1) {
				fmt.Println("Default profile: " + l.bold + a + l.reset)
			} else {
				fmt.Println(l.bold + a + l.reset)
			}
		} else if names := l.confNames(); len(names) == 0 {
			// #368: a fresh install hears the one sentence every command gives,
			// not the command it just typed with no profile to name. Off a
			// terminal it goes to stderr: stdout is the default's name, and
			// there is none (#636).
			if isTTY(1) {
				fmt.Println(l.dim + noProfilesMsg + l.reset)
			} else {
				fmt.Fprintln(os.Stderr, noProfilesMsg)
			}
		} else {
			// #368: profiles exist but none is the default, so the answer names
			// the profiles to choose from, never the command just typed.
			// A pipe's stdout is the default's name, and there is none: the
			// sentence goes to stderr there, so a script reads an empty answer
			// (#636).
			if isTTY(1) {
				fmt.Println(l.dim + "No default set. Profiles: " + strings.Join(names, " ") + l.reset)
			} else {
				fmt.Fprintln(os.Stderr, "No default set. Profiles: "+strings.Join(names, " "))
			}
		}
		return 0
	}
	name := args[0]
	if !l.requireProfile(&name, "use") {
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
	if !l.judgeProfile(name, "models") {
		return 1
	}
	v, ok := l.pickValue("model", name)
	if !ok {
		return 1
	}
	fmt.Println(v)
	return 0
}
