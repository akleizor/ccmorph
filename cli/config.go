package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

func interactive(a string) bool { return a == "-i" || a == "--interactive" }

// getSet is `ccmorph get [profile] [model|effort]` and `ccmorph set [profile]
// [model|effort] [value]`. A missing profile, key or value opens its chooser.
func (l *launcher) getSet(verb string, args []string) int {
	if verb == "get" && len(args) > 2 {
		l.err("usage: ccmorph get [profile] [model|effort]")
		return 2
	}
	if verb == "set" && len(args) > 3 {
		l.err("usage: ccmorph set [profile] [model|effort] [value]")
		return 2
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
		l.err("usage: ccmorph " + verb + " <profile>")
		return 2
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'")
		return 1
	}
	key := at(1)
	pick := func(key string) (string, bool) {
		return l.pickValue(key, name)
	}
	if verb == "set" && (key == "model" || key == "effort") && (len(args) == 2 || interactive(at(2))) {
		v, ok := pick(key)
		if !ok {
			return 1
		}
		args = []string{name, key, v}
	}
	if (key == "" || interactive(key)) && len(args) <= 2 {
		k, rc := uiSelect("native", "Setting — "+name,
			"model\tModel\tDefault model for new sessions\neffort\tReasoning\tDefault reasoning level", "")
		if rc != 0 {
			return 1
		}
		key = k
		args = []string{name, key}
		if verb == "set" {
			v, _ := l.pickValue(key, name)
			args = append(args, v)
		}
	}
	if key != "model" && key != "effort" {
		if verb == "get" {
			l.err("usage: ccmorph get <profile> <model|effort>")
		} else {
			l.err("usage: ccmorph set <profile> <model|effort> [value]  (omit the value, or pass -i, to choose from a list)")
		}
		return 2
	}
	if verb == "get" {
		if len(args) != 2 {
			l.err("usage: ccmorph get <profile> <model|effort>")
			return 2
		}
		out, rc := l.configStore("get", name, key)
		if rc == 0 {
			fmt.Println(out)
		}
		return rc
	}
	if len(args) != 3 || args[2] == "" {
		l.err("usage: ccmorph set <profile> <model|effort> [value]  (omit the value, or pass -i, to choose from a list)")
		return 2
	}
	if rc, done := l.locked(name, append([]string{"set"}, args...)); done {
		return rc
	}
	value := args[2]
	model, effort := "", ""
	if key == "model" {
		model = value
	} else {
		effort = value
	}
	env := []string{"_CCM_CONFIG_MODEL=" + model, "_CCM_CONFIG_EFFORT=" + effort, "_CCM_CONFIG_SNAPSHOT=",
		"_CCM_CONFIG_TYPE=", "_CCM_CONFIG_ENDPOINT=", "_CCM_CONFIG_CREDENTIAL="}
	if _, rc := l.configStore("set", name, "", env...); rc != 0 {
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
		l.err("usage: ccmorph config [profile]")
		return 2
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
		l.err("usage: ccmorph config <profile>")
		return 2
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'")
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
			rows := "defaults\tModel and reasoning\tDefaults for new sessions"
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
			if msg, rc := l.util(nil, "", "profile-validate-url", "profile '"+name+"'", endpoint); rc != 0 {
				l.err(msg)
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
	if typ == "apikey" {
		var in strings.Builder
		for _, k := range []string{"type", "api_key", "base_url", "adapter"} {
			in.WriteString(k + "\x00" + f[k] + "\x00")
		}
		if msg, rc := l.util(nil, in.String(), "profile-validate", name); rc != 0 {
			l.err(msg)
			return 1
		}
	}
	if section != "connection" {
		preview := []string{endpoint, credential}
		if model, ok = l.promptDefault("model", name, model, typ, preview); !ok {
			return 1
		}
		if effort, ok = l.promptDefault("effort", name, effort, typ, preview); !ok {
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
	or := func(v string) string {
		if v == "" {
			return "Claude default"
		}
		return v
	}
	say("Model: " + or(model) + " · Effort: " + or(effort))
	env := []string{"_CCM_CONFIG_MODEL=" + model, "_CCM_CONFIG_EFFORT=" + effort, "_CCM_CONFIG_SNAPSHOT=" + snapshot,
		"_CCM_CONFIG_TYPE=" + typ, "_CCM_CONFIG_ENDPOINT=" + endpoint, "_CCM_CONFIG_CREDENTIAL=" + credential}
	if _, rc := l.configStore("configure", name, "", env...); rc != 0 {
		return 1
	}
	fmt.Println("Saved " + name + " (new sessions)")
	return 0
}

// promptDefault is _ccm_prompt_default: keep the current value by default, and
// allow a custom model id when the catalogue is unavailable.
func (l *launcher) promptDefault(slot, name, current, typ string, preview []string) (string, bool) {
	shown := current
	if shown == "" {
		shown = "Claude default"
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
	if slot == "model" && typ == "apikey" {
		say("Loading models from the selected endpoint…")
	}
	var rows string
	if typ == "apikey" && slot == "model" {
		rows, _ = l.valueRows(slot, name, preview...)
	} else {
		rows, _ = l.valueRows(slot, name)
	}
	if slot == "model" && rows == "" {
		say("Model list unavailable. Keep the current default or enter a custom model ID.")
	}
	menu := []string{"@keep\tKeep " + shown}
	for _, row := range splitRows(rows) {
		value := rowID(row)
		if value == current {
			continue
		}
		menu = append(menu, value+"\t"+value+"\t"+strings.TrimPrefix(row, value+"\t"))
	}
	if slot == "model" {
		menu = append(menu, "@custom\tEnter a custom model ID…")
	}
	choice, rc := chooseValue(slot, "Default "+slot+" — "+name, strings.Join(menu, "\n"), "labels")
	if rc != 0 {
		return "", false
	}
	switch choice {
	case "@keep":
		return current, true
	case "@custom":
		answer, rc := uiInput("Custom model ID (Enter keeps current): ", false)
		if rc != 0 {
			return "", false
		}
		if answer == "" {
			answer = current
		}
		return answer, true
	}
	return choice, true
}
