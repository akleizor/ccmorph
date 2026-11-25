package cli

import (
	"encoding/json"
	"fmt"
	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/util"
	"os"
	"slices"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

func interactive(a string) bool { return a == "-i" || a == "--interactive" }

// sameTerminal reports whether a and b are one terminal: the screen a person
// reads both on.
func sameTerminal(a, b *os.File) bool {
	if !isTTY(int(a.Fd())) || !isTTY(int(b.Fd())) {
		return false
	}
	sa, errA := a.Stat()
	sb, errB := b.Stat()
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// valueRows is how many terminal rows a value line took, columns wide: a line
// that exactly fills a row does not wrap, and the newline moves on (#361).
func valueRows(value string, columns int) int {
	return (max(ctxwindow.Cells(value), 1)-1)/columns + 1
}

// settingWord reports whether word is the model or effort setting, the two the
// shorthand lets lead a command when the profile is left out (#788).
func settingWord(word string) bool {
	return word == "model" || word == "effort"
}

// explicitKey reports whether word is a key the command reads or writes right
// after the profile. A key there means the word before it is the profile, never
// the shorthand: `get model base_url` reads the endpoint of a profile named
// model, the explicit form this change must keep.
func explicitKey(verb, word string) bool {
	switch word {
	case "model", "effort":
		return true
	case "base_url", "api_key", "--json":
		return verb == "get"
	}
	return false
}

// jsonLast moves a get's leading --json behind the profile slot: the flag reads
// the same wherever it stands (#788). The word after it, if any, is the profile;
// "" when it was left out. moved reports a leading --json.
func jsonLast(args []string) (out []string, moved bool) {
	if len(args) == 0 || args[0] != "--json" {
		return args, false
	}
	name := ""
	if len(args) > 1 {
		name = args[1]
	}
	return []string{name, "--json"}, true
}

// getSet is `ccmorph get [profile] [model|effort|base_url|api_key|--json]` and `ccmorph set [profile]
// [model|effort] [value]`. A missing profile, key or value opens its chooser.
func (l *launcher) getSet(verb string, args []string) int {
	if verb == "get" && len(args) > 2 {
		return l.extraArg(verb, args[2])
	}
	if verb == "set" && len(args) > 3 {
		return l.extraArg(verb, args[3])
	}
	at := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	// --json is a flag wherever it stands, so a leading one moves behind the
	// profile slot before any word is read as a profile (#788).
	jsonFirst := false
	if verb == "get" {
		args, jsonFirst = jsonLast(args)
	}
	name := at(0)
	shorthand := settingWord(name) && !explicitKey(verb, at(1))
	if shorthand {
		// The documented shorthand: the usage line marks the profile optional
		// (`set [profile] [model|effort] [value]`), so a leading model or effort
		// is the setting and the profile is chosen, not required (#788). A key
		// behind it keeps the explicit form reachable for a profile named after
		// a setting: `get model model`, and `get model base_url` for its other
		// fields.
		args = append([]string{""}, args...)
		name = ""
	}
	if (shorthand || jsonFirst) && name == "" {
		// One saved profile is the choice the picker would make; more ask it.
		if names := l.confNames(); len(names) == 1 {
			name = names[0]
		}
	}
	if !l.requireProfile(&name, verb) {
		return 1
	}
	// The chooser's pick takes args[0]'s place whether the menu ran or not —
	// name is args[0] when it did not.
	args = append([]string{name}, args[min(1, len(args)):]...)
	key := at(1)
	pick := func(key string) (string, bool) {
		if key == "model" {
			return l.pickModelDefault(name)
		}
		return l.pickValue(key, name)
	}
	if verb == "set" && (key == "model" || key == "effort") && (len(args) == 2 || interactive(at(2))) {
		v, ok := pick(key)
		if !ok {
			return 1
		}
		if v == "" {
			// The picker kept a default that was never set: nothing to store.
			return 0
		}
		args = []string{name, key, v}
	}
	if (key == "" || interactive(key)) && len(args) <= 2 {
		k, rc := uiSelect("native", "Setting — "+name,
			"model\tModel\tDefault model for new sessions\neffort\tEffort\tDefault effort level", "")
		if rc != 0 {
			return 1
		}
		key = k
		args = []string{name, key}
		if verb == "set" {
			v, ok := pick(key)
			if !ok {
				return 1
			}
			if v == "" {
				// The picker kept a default that was never set: nothing to store.
				return 0
			}
			args = append(args, v)
		}
	}
	// get is the one public read of a profile, so scripts never parse its .conf;
	// --json adds the tier map ({alias: model}) a launch resolves.
	if verb == "get" && key == "api_key" {
		return l.cred([]string{name})
	}
	if verb == "get" && (key == "base_url" || key == "--json") {
		f, ok := l.readRaw(name)
		if !ok {
			return 1
		}
		if key == "--json" {
			out := map[string]any{"profile": name, "type": f["type"], "base_url": f["base_url"], "api_key": credOf(f)}
			out["model"], _ = l.configStoreQ("get", name, "model")
			out["effort"], _ = l.configStoreQ("get", name, "effort")
			// The tier map a launch serves, through resolveTier, so a caller that
			// pins tiers (the Remote Control supervisor) reads them here rather
			// than keeping a second copy that drifts.
			tiers := map[string]string{}
			for _, tier := range util.TierWords {
				if m, ok := resolveTier(tier,
					func(key string) (string, bool) { v, ok := f["env."+key]; return v, ok },
					func(tier string) (string, bool) { return l.tierAlias(name, tier) }); ok {
					tiers[tier] = m
				}
			}
			out["tiers"] = tiers
			b, _ := json.Marshal(out)
			fmt.Println(string(b))
			return 0
		}
		if f["base_url"] == "" {
			say(name + ": no base_url — it connects to the provider directly")
			return 0
		}
		fmt.Println(f["base_url"])
		return 0
	}
	if key != "model" && key != "effort" {
		if strings.EqualFold(key, "reasoning") {
			// The menus once called this setting reasoning (#367): say the word
			// the commands take, not a usage line whose [model|effort] buries it.
			l.err("the setting is called effort, not reasoning")
		}
		l.err("usage: " + usageLine(verb))
		return 2
	}
	if verb == "get" {
		if len(args) != 2 {
			l.err("usage: " + usageLine("get"))
			return 2
		}
		// The one conf validator every reader shares: a profile `list` would
		// skip must not answer confidently here (GH-839).
		if _, ok := l.readRaw(name); !ok {
			return 1
		}
		out, rc := l.configStore("get", name, key)
		if rc != 0 {
			return rc
		}
		if out == "" {
			// Unset is a real state, not a broken one (#268): say so on stderr
			// and keep stdout empty — not even a newline — for scripts. The
			// words after the dash are the shared unsaved-state words (#381):
			// what a launch of this profile does with nothing saved.
			if key == "model" {
				typ, base := "", ""
				if f, ok := l.readQuiet(name); ok {
					typ, base = f["type"], f["base_url"]
				}
				say(name + ": no model saved — " + unsavedModelWords(typ, base))
			} else {
				say(name + ": no effort saved — " + unsavedEffortWords)
			}
			return 0
		}
		fmt.Println(out)
		return 0
	}
	if len(args) != 3 || args[2] == "" {
		l.err("usage: " + usageLine("set"))
		return 2
	}
	if rc, done := l.locked(name, append([]string{"set"}, args...)); done {
		return rc
	}
	value := args[2]
	// A set is judged on the conf it will leave behind: overwriting the one
	// value the file refuses is the repair, and every other refusal still
	// stands (GH-839).
	if _, err := l.readProfile(name, func(line string) { fmt.Fprintln(os.Stderr, line) }); err != nil {
		if key != "effort" || !slices.Contains(profile.Efforts, value) {
			l.err(err.Error())
			return 1
		}
		if _, rerr := profile.ReadForSave(l.dir, name, true, profile.Defaults(), func(string) {}, map[string]string{key: value}); rerr != nil {
			l.err(rerr.Error())
			return 1
		}
	}
	// unchecked is a save no listing vouched for, in the sentence that says so
	// after the save stands; empty when the save was checked or none was asked
	// for (the saved model kept, a native route, no key to ask with).
	unchecked := ""
	// askedListing records that a model list could actually be asked of this
	// profile; a save nobody could vouch for without one is noted below (#636).
	askedListing := false
	// notChecked is decided before the save: after it, accountModel answers
	// with the value just written and could never tell the two apart (#636).
	notChecked := key == "model" && value != l.accountModel(name)
	if key == "model" {
		// A gateway that answers its listing knows what it serves: an id it does
		// not serve is saved only when the person confirms it, because the
		// alternative is a session that fails at its first message (#267). A
		// listing that cannot be read adjudicates nothing, so the save proceeds —
		// but says it went unchecked, the way the add says a key it could not ask
		// about was not checked (gh728). The model already saved is not judged
		// again: keeping it changes nothing (#279).
		// readQuiet: the guard only decides whether a listing can be asked, so
		// a conf the save itself accepts must not print a refusal here (#636).
		if f, ok := l.readQuiet(name); ok && value != l.accountModel(name) && f["type"] == "apikey" && !util.NativeBase(f["base_url"]) && strings.TrimSpace(f["api_key"]) != "" {
			askedListing = true
			listing, status, answered := util.ListingReply(f["base_url"], f["api_key"])
			shown := profile.RedactURL(f["base_url"])
			switch {
			case !answered:
				unchecked = shown + " did not answer, so the model was not checked"
			case status != 0:
				unchecked = fmt.Sprintf("%s answered HTTP %d, so the model was not checked", shown, status)
			default:
				// An answer that is no listing (an empty 204, {}, a page, a
				// malformed body) judged nothing: the save proceeds and says so (gh728).
				if msg, refuse := util.ServedRefusal(listing, value, f["model_map"], name); refuse {
					l.err(msg)
					if uiConfirm("save '"+value+"' anyway?") != 0 {
						return 1
					}
				} else if util.ServedByListing(listing, value, f["model_map"]) == util.ServedUnknown {
					unchecked = shown + " answered without a usable model listing, so the model was not checked"
				}
			}
		}
	}
	model, effort := "", ""
	if key == "model" {
		model = value
	} else {
		effort = value
	}
	set := l.storeArgs("set", name, "")
	set.Change = util.StoreChange{Model: model, Effort: effort}
	if _, rc := l.runStore(set, os.Stderr); rc != 0 {
		return 1
	}
	// stdout carries the value alone on its own line, for a script to capture.
	// On the terminal the Saved sentence goes to, that line is an echo: a full
	// terminal folds it into the sentence, as every picker folds its answer
	// (#361); a dumb one cannot erase, so there it is left out (#358).
	shared := sameTerminal(os.Stdout, os.Stderr)
	fold := shared && os.Getenv("TERM") != "dumb"
	if !shared || fold {
		fmt.Println(value)
	}
	if fold {
		_, columns := termSize()
		eraseLines(valueRows(value, columns))
	}
	// Piped too, as the guide says: a script's log records what was saved,
	// while stdout stays the value alone (#636).
	fmt.Fprintf(os.Stderr, "%sSaved%s %s %s = %s%s%s %s(new sessions)%s\n",
		l.accent, l.reset, name, key, l.bold, value, l.reset, l.dim, l.reset)
	if unchecked != "" {
		// The save stands — custom ids stay allowed — but it says what nobody
		// could check, on the channel a person reads: stdout kept carrying the
		// value alone, for the script that captured it (gh728).
		say(name + ": saved unchecked — " + unchecked)
	}
	if notChecked && !askedListing {
		// #636: the profiles a listing cannot answer — a browser sign-in, a
		// native base, a save without a key — took any model in silence, so
		// inspect's launch line was the first hint of a typo. The save
		// stands; it just says nobody could check it.
		say(name + ": saved unchecked — '" + value + "' was not checked: no model list answered for " + name + ", so a wrong id fails at the session's first message")
	}
	return 0
}

// config is `ccmorph config [profile]`: endpoint, credential and defaults in one
// save. A terminal chooses a section; piped input keeps the full sequence.
func (l *launcher) config(args []string) int {
	if len(args) > 1 {
		return l.extraArg("config", args[1])
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	if !l.requireProfile(&name, "config") {
		return 1
	}
	if rc, done := l.locked(name, []string{"config", name}); done {
		return rc
	}
	// The conf's one validator runs before any store read: a profile `list`
	// would skip must not answer from the store with a different fault (GH-839).
	f, ok := l.readRaw(name)
	if !ok {
		return 1
	}
	snapshot, rc := l.configStore("snapshot", name, "")
	if rc != 0 {
		return 1
	}
	typ, endpoint := f["type"], f["base_url"]
	credential := f["api_key"]
	if credential == "" {
		credential = f["token"]
	}
	model, rc := l.configStore("get", name, "model")
	if rc != 0 {
		return 1
	}
	effort, rc := l.configStore("get", name, "effort")
	if rc != 0 {
		return 1
	}
	section := "all"
	if isTTY(0) {
		section = "defaults"
		if typ == "apikey" || typ == "token" {
			rows := "defaults\tModel and effort\tDefaults for new sessions"
			if typ == "apikey" {
				rows += "\nconnection\tConnection\tEndpoint or API key"
			} else {
				rows += "\nconnection\tSign-in token\tReplace the stored token"
			}
			s, rc := uiSelect("native", "Settings — "+name, rows, "")
			if rc != 0 {
				return 1
			}
			section = s
		}
	}
	if section != "defaults" && typ == "apikey" {
		shown, _ := nativeBase(endpoint)
		answer, rc := uiInput("Endpoint ["+shown+"]: ", false)
		if rc != 0 {
			return 1
		}
		if answer != "" {
			endpoint = answer
		}
		f["base_url"] = endpoint
		if endpoint != "" {
			if err := profile.ValidateBaseURL("profile '"+name+"'", endpoint); err != nil {
				l.err(err.Error())
				return 1
			}
		}
	}
	if section != "defaults" && (typ == "apikey" || typ == "token") {
		replace := section == "connection"
		if !replace {
			switch uiConfirm("Replace stored credential?") {
			case 0:
				replace = true
			case 2:
				return 1
			}
		}
		if replace {
			answer, rc := uiInput("Credential (hidden; Enter keeps current): ", true)
			if rc != 0 {
				return 1
			}
			if answer != "" {
				credential = answer
			}
		}
		if strings.ContainsAny(credential, "\r\n") {
			l.err("credential contains CR/LF")
			return 1
		}
		if typ == "apikey" {
			f["api_key"] = credential
		} else {
			f["token"] = credential
		}
	}
	if err := profile.Validate(name, f); err != nil {
		l.err(err.Error())
		return 1
	}
	if section != "connection" {
		preview := []string{endpoint, credential}
		if model, ok = l.promptDefault("model", name, model, typ, endpoint, preview); !ok {
			return 1
		}
		if effort, ok = l.promptDefault("effort", name, effort, typ, endpoint, preview); !ok {
			return 1
		}
	}
	say("Profile: " + name)
	if section != "defaults" {
		if typ == "apikey" {
			shown, _ := nativeBase(endpoint)
			say("Endpoint: " + shown)
		}
		say("Credential: hidden")
	}
	say("Model: " + valueOrUnsaved(model, "model", typ, endpoint) + " · Effort: " + valueOrUnsaved(effort, "effort", typ, endpoint))
	save := l.storeArgs("configure", name, "")
	save.Change = util.StoreChange{Model: model, Effort: effort, Snapshot: snapshot, Type: typ, Endpoint: endpoint, Credential: credential}
	if _, rc := l.runStore(save, os.Stderr); rc != 0 {
		return 1
	}
	fmt.Println("Saved " + name + " (new sessions)")
	return 0
}

// promptDefault is _ccm_prompt_default: keep the current value by default, and
// allow a custom model id when the catalogue is unavailable. Nothing saved
// shows as the none it keeps with the words for what that means at launch.
func (l *launcher) promptDefault(slot, name, current, typ, base string, preview []string) (string, bool) {
	shown := current
	if shown == "" {
		shown = unsavedLabel(slot, typ, base)
	}
	if !isTTY(0) {
		answer, rc := uiInput("Default "+slot+" ["+shown+"]: ", false)
		if rc != 0 {
			return "", false
		}
		if answer == "" {
			answer = current
		}
		return answer, true
	}
	return l.valueMenu(slot, name, current, typ, base, l.defaultRows(slot, name, typ, preview...), true)
}
