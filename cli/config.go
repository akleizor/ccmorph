package cli

import (
	"fmt"
	"os"

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
		v, ok := l.zshUI(`_ccm_pick_value "$1" "$2"`, key, name)
		return v, ok && v != ""
	}
	if verb == "set" && (key == "model" || key == "effort") && (len(args) == 2 || interactive(at(2))) {
		v, ok := pick(key)
		if !ok {
			return 1
		}
		args = []string{name, key, v}
	}
	if (key == "" || interactive(key)) && len(args) <= 2 {
		k, ok := l.zshUI(`ccm_ui_select native "$1" "$2"`, "Setting — "+name,
			"model\tModel\tDefault model for new sessions\neffort\tReasoning\tDefault reasoning level")
		if !ok {
			return 1
		}
		key = k
		args = []string{name, key}
		if verb == "set" {
			v, ok := l.zshUI(`_ccm_pick_value "$1" "$2"`, key, name)
			if !ok {
				return 1
			}
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
