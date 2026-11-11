package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"io"
	"os"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

func interactive(a string) bool { return a == "-i" || a == "--interactive" }

// getSet is `ccmorph get [profile] [model|effort]` and `ccmorph set [profile]
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
	name := at(0)
	if name == "" || interactive(name) {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		args = append([]string{picked}, args[min(1, len(args)):]...)
		name = picked
	}
	if !profile.ValidName(name) {
		return l.badName(verb, name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
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
	if key == "model" {
		// A gateway that answers its listing knows what it serves: an id it does
		// not serve is saved only when the person confirms it, because the
		// alternative is a session that fails at its first message (#267). A
		// listing that cannot be read adjudicates nothing, so the save proceeds. The
		// model already saved is not judged again: keeping it changes nothing (#279).
		if f, ok := l.read(name); ok && value != l.accountModel(name) && f["type"] == "apikey" && !util.NativeBase(f["base_url"]) && strings.TrimSpace(f["api_key"]) != "" {
			if listing, ok := util.ModelsListing(f["base_url"], f["api_key"], io.Discard); ok {
				if msg, refuse := util.ServedRefusal(listing, value, f["model_map"], name); refuse {
					l.err(msg)
					if uiConfirm("save '"+value+"' anyway?") != 0 {
						return 1
					}
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
	// The value alone on stdout for scripts; the sentence on stderr for a picked value.
	if isTTY(2) {
		fmt.Fprintf(os.Stderr, "%sSaved%s %s %s = %s%s%s %s(new sessions)%s\n",
			l.accent, l.reset, name, key, l.bold, value, l.reset, l.dim, l.reset)
	}
	fmt.Println(value)
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
	if name == "" || interactive(name) {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		return l.badName("config", name)
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	if rc, done := l.locked(name, []string{"config", name}); done {
		return rc
	}
	snapshot, rc := l.configStore("snapshot", name, "")
	if rc != 0 {
		return 1
	}
	f, ok := l.readRaw(name)
	if !ok {
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
		shown := endpoint
		if shown == "" {
			shown = "https://api.anthropic.com"
		}
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
			shown := endpoint
			if shown == "" {
				shown = "https://api.anthropic.com"
			}
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
