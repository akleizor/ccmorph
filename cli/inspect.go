package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

// grid is _ccm_grid: a label column of width w, wrapping a long label onto its own line.
func grid(w int, label, value string) {
	if len(label) > w {
		fmt.Printf("  %s\n  %-*s %s\n", label, w, "", value)
		return
	}
	fmt.Printf("  %-*s %s\n", w, label, value)
}

// thousands is printf "%'d" in an en_US-style locale.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// inspect is `ccmorph inspect [profile|-i]`: what a profile launches as, each
// value named with its source.
func (l *launcher) inspect(args []string) int {
	active := l.active()
	name := active
	if len(args) > 0 {
		name = args[0]
	}
	if name == "-i" || name == "--interactive" || name == "" {
		picked, ok := l.menuProfile()
		if !ok {
			return 1
		}
		name = picked
	}
	if !profile.ValidName(name) {
		l.err("usage: ccmorph inspect [<profile>]")
		return 1
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'")
		l.accountsHint()
		return 1
	}
	f, ok := l.read(name)
	if !ok {
		return 1
	}
	typ, base, tag := f["type"], f["base_url"], f["tag"]
	pctConf := f["env.CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"]

	head := l.bold + name + l.reset + "  " + l.dim + typ
	if tag != "" {
		head += " · " + tag
	}
	if name == active {
		head += " · default profile"
	}
	fmt.Println(head + l.reset)
	fmt.Println()
	if base != "" {
		grid(10, "endpoint", strings.TrimPrefix(base, "https://"))
	}

	model, rc := l.configStore("get", name, "model")
	if rc != 0 {
		return 1
	}
	from := "profile default"
	if model == "" {
		from = ""
		if b, err := os.ReadFile(l.conf(name)); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(line, "model=") {
					model = strings.TrimPrefix(line, "model=")
					break
				}
			}
		}
		if model != "" {
			from = "conf bootstrap"
		}
	}
	if model != "" {
		grid(10, "model", model+"  "+l.dim+from+l.reset)
	} else {
		grid(10, "model", l.dim+"— Claude Code chooses"+l.reset)
	}

	effort, rc := l.configStore("get", name, "effort")
	if rc != 0 {
		return 1
	}
	if effort != "" {
		grid(10, "effort", effort+"  "+l.dim+"profile default"+l.reset)
	} else {
		grid(10, "effort", l.dim+"— Claude Code's own default"+l.reset)
	}
	if ms, rc := l.configStore("model-settings", name, ""); rc == 0 {
		if rows, _ := l.util(nil, ms+"\n", "effort-rows"); rows != "" {
			fmt.Println(rows)
		}
	}

	// Window only when it is a probed fact about a routed model: an unprobed
	// default is not reported as though it were measured.
	if model != "" && typ == "apikey" {
		tok, unprobed, source := "", "1", "probed"
		if l.migrateState() {
			if win := l.coreNoCeiling(l.windowEnv(name), "model-window", name, model); win != "" {
				if lbl, rc := l.utilQ(nil, win+"\n", "window-label"); rc == 0 {
					if p := strings.Fields(lbl); len(p) == 3 {
						tok, unprobed, source = p[0], p[1], p[2]
					}
				}
			}
		}
		n, err := strconv.ParseInt(tok, 10, 64)
		if err == nil && n > 0 && unprobed == "0" {
			pct, ok := l.effectivePct(name, pctConf)
			if !ok {
				return 1
			}
			grid(10, "window", thousands(n)+"  "+l.dim+source+" · compaction target "+pct+"%"+l.reset)
		} else {
			grid(10, "window", l.dim+"unknown · compacts early to stay safe"+l.reset)
		}
	}

	switch base {
	case "", "https://api.anthropic.com", "https://api.anthropic.com/":
		grid(10, "protocol", "native Anthropic")
	default:
		grid(10, "protocol", "automatic — cached compatibility or validation at launch")
	}
	fmt.Println()
	if model != "" && typ == "apikey" {
		fmt.Println("  " + l.dim + "launches as" + l.reset + "  claude --model " + model)
	} else {
		fmt.Println("  " + l.dim + "launches as" + l.reset + "  claude " + l.dim + "(Claude Code picks the model)" + l.reset)
	}
	return 0
}

// effectivePct is _ccm_effective_compaction_percent.
func (l *launcher) effectivePct(name, conf string) (string, bool) {
	args := append([]string{"effective-pct", conf}, l.settingsSources(name)...)
	if v, rc := l.utilQ(nil, "", args...); rc == 0 && v != "" {
		return v, true
	}
	return l.compactionPercent()
}
