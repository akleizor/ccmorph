package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akleizor/ccmorph/adapter"
	"github.com/akleizor/ccmorph/profile"
	"github.com/akleizor/ccmorph/quota"
	"github.com/akleizor/ccmorph/util"
)

// check is `ccmorph check <profile> <model> [--json]`: does this model of this
// profile answer right now? One smallest real request on the route a session
// of the profile takes, with its own credential; exit 0 answered, 1 refused,
// 2 a usage error.
func (l *launcher) check(args []string) int {
	asJSON, words := false, []string{}
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else {
			words = append(words, a)
		}
	}
	if len(words) != 2 {
		l.err("usage: " + usageLine("check"))
		return 2
	}
	name, model := words[0], words[1]
	if !profile.ValidName(name) || !l.exists(name) {
		l.err(l.noProfileMsg(name))
		return 2
	}
	typ, env, err := util.ProfileEnv(l.dir, l.profileDirs, name, func(string) {})
	if err != nil {
		l.err(err.Error())
		return 2
	}
	get := func(k string) string { v, _ := envGet(env, k); return v }
	base := get("ANTHROPIC_BASE_URL")
	if base == "" {
		base = "https://api.anthropic.com" // Claude Code's own default
	}
	modelMap := map[string]any{}
	_ = json.Unmarshal([]byte(get("CCMORPH_MODEL_MAP")), &modelMap)
	wire := adapter.LaunchModel(model, modelMap)
	anthropic := []string{"anthropic"}
	var r adapter.CheckResult
	switch typ {
	case "apikey":
		routes := adapter.Routes
		if d := adapter.CachedRoute(base, l.profileDirs, wire); d != "" {
			routes = []string{d}
		}
		r = adapter.Check(base, get("ANTHROPIC_API_KEY"), wire, get("CCMORPH_UPSTREAM_PROXY"), false, routes)
	case "token":
		r = adapter.Check(base, get("CLAUDE_CODE_OAUTH_TOKEN"), wire, "", true, anthropic)
	case "sub":
		if token, _, err := quota.SubscriptionToken(filepath.Join(l.profileDirs, name)); err != nil {
			r = adapter.CheckResult{Route: "anthropic", Note: err.Error()}
		} else {
			r = adapter.Check(base, token, wire, "", true, anthropic)
		}
	case "provider":
		// The bridge a session runs on: joined if one is up, else started for
		// this check alone and stopped after it.
		if why := l.providerStartRefusal(name); why != "" {
			r = adapter.CheckResult{Route: "anthropic", Note: why}
		} else if !l.providerStart(name) {
			r = adapter.CheckResult{Route: "anthropic", Note: "the provider bridge did not start"}
		} else {
			defer l.providerStop()
			r = adapter.Check("http://127.0.0.1:"+l.providerPort, l.providerKey, wire, "", false, anthropic)
		}
	default:
		l.err("profile '" + name + "' has type '" + typ + "', which check cannot ask")
		return 2
	}
	if asJSON {
		if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
			l.err(err.Error())
			return 1
		}
	} else {
		verdict := "refused"
		if r.OK {
			verdict = "ok"
		}
		line := fmt.Sprintf("%s %s: %s on %s (HTTP %d, %d ms)", name, model, verdict, r.Route, r.Status, r.MS)
		if r.Note != "" {
			line += ": " + r.Note
		}
		fmt.Println(line)
	}
	if !r.OK {
		return 1
	}
	return 0
}
