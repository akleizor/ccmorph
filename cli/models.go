package cli

import (
	"fmt"

	"github.com/akleizor/ccmorph/profile"
)

const modelsUsage = "usage: ccmorph models <profile> [--filtered|--filtered-json]"

// models is `ccmorph models [profile] [--filtered|--filtered-json]`: the
// endpoint's own /v1/models, as the catalogue, the picker or filtered JSON.
func (l *launcher) models(args []string) int {
	if len(args) > 2 {
		l.err("usage: ccmorph models [profile] [--filtered|--filtered-json]")
		return 2
	}
	if len(args) == 0 || args[0] == "-i" || args[0] == "--interactive" {
		picked, ok := l.menuProfile("apikey")
		if !ok {
			return 1
		}
		args = append([]string{picked}, args[min(1, len(args)):]...)
	}
	mode := ""
	if len(args) > 1 {
		mode = args[1]
	}
	if mode != "" && mode != "--filtered" && mode != "--filtered-json" {
		l.err(modelsUsage)
		return 2
	}
	name := args[0]
	if !profile.ValidName(name) {
		l.err("model listing requires an profile")
		return 1
	}
	if !l.exists(name) {
		l.err("no profile '" + name + "'. Run: ccmorph list")
		return 1
	}
	listing, ok := l.modelsJSON(name)
	if !ok {
		return 1
	}
	if mode == "--filtered-json" {
		out, rc := l.util(nil, listing+"\n", "models-filtered-json", l.conf(name))
		if rc != 0 {
			return 1
		}
		printNonEmpty(out)
		return 0
	}
	style := "catalogue"
	if mode == "--filtered" {
		style = "picker"
	}
	out, rc := l.util(nil, listing+"\n", "gw-models", l.conf(name), style)
	if rc != 0 {
		l.err("'" + name + "' endpoint returned no usable model list")
		return 1
	}
	printNonEmpty(out)
	return 0
}

func printNonEmpty(s string) {
	if s != "" {
		fmt.Println(s)
	}
}

// modelsJSON is _ccm_gw_models_json for a saved profile.
func (l *launcher) modelsJSON(name string) (string, bool) {
	f, ok := l.read(name)
	if !ok {
		return "", false
	}
	base := f["base_url"]
	if base == "" {
		base = "https://api.anthropic.com"
	}
	if f["api_key"] == "" {
		l.err("'" + name + "' has no api_key — model discovery needs one")
		return "", false
	}
	out, rc := l.util(nil, f["api_key"]+"\n", "fetch-listing", base, "models")
	if rc != 0 {
		red, rrc := l.utilQ(nil, "", "redact-url", base)
		if rrc != 0 {
			red = "[unparseable endpoint]"
		}
		l.err("could not reach '" + red + "' — if it is an internal endpoint, connect your private network and try again")
		return "", false
	}
	return out, true
}
