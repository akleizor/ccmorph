package cli

import (
	"fmt"
	"github.com/akleizor/ccmorph/util"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

// models is `ccmorph models [profile] [--filtered|--filtered-json]`: the
// endpoint's own /v1/models, as the catalogue, the picker or filtered JSON.
func (l *launcher) models(args []string) int {
	if len(args) > 2 {
		return l.extraArg("models", args[2])
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
		return l.extraArg("models", mode)
	}
	name := args[0]
	if !profile.ValidName(name) {
		l.err("model listing requires an profile")
		return 1
	}
	if !l.exists(name) {
		l.noProfile(name)
		return 1
	}
	listing, ok := l.modelsJSON(name)
	if !ok {
		return 1
	}
	if mode == "--filtered-json" {
		out, ok := util.ModelsFilteredJSON(l.conf(name), listing)
		if !ok {
			return 1
		}
		printNonEmpty(out)
		return 0
	}
	style := "catalogue"
	if mode == "--filtered" {
		style = "picker"
	}
	rows, ok := util.GwModels(l.conf(name), style == "picker", listing)
	if !ok {
		l.err("'" + name + "' endpoint returned no usable model list")
		return 1
	}
	printNonEmpty(modelRows(rows))
	return 0
}

func printNonEmpty(s string) {
	if s != "" {
		fmt.Println(s)
	}
}

// modelsJSON is _ccm_gw_models_json. preview is an unsaved base and key that
// configure is trying: never the old endpoint with the new credential.
func (l *launcher) modelsJSON(name string, preview ...string) (string, bool) {
	f, ok := l.read(name)
	if !ok {
		return "", false
	}
	if len(preview) > 0 {
		f["base_url"] = preview[0]
	}
	if len(preview) > 1 {
		f["api_key"] = preview[1]
	}
	base := f["base_url"]
	if base == "" {
		base = "https://api.anthropic.com"
	}
	if f["api_key"] == "" {
		l.err("'" + name + "' has no api_key — model discovery needs one")
		return "", false
	}
	// A launch that already named this gateway down must not wait on it again
	// in its picker (#260): the verdict's sentence was said once, so this pass
	// fails quietly — saying it again would only repeat the same line. A
	// preview is a different, unsaved door the verdict never covered.
	if len(preview) == 0 && l.unreachable[memoKey(base, f["api_key"])] {
		return "", false
	}
	out, status, why, ok := util.ModelsAnswer(base, f["api_key"])
	out = strings.TrimRight(out, "\n")
	if !ok {
		l.err(listingFailureWhy(name, profile.RedactURL(base), status, why))
		return "", false
	}
	return out, true
}

// listingFailure is what a person reads when the endpoint gave no model list:
// the status the endpoint refused with names the fix. Status 0 is an endpoint
// that never answered, said in the generic words only where no cause is known;
// where one is, listingFailureWhy says it.
func listingFailure(name, endpoint string, status int) string {
	return listingFailureWhy(name, endpoint, status, "if it is an internal endpoint, connect your private network and try again")
}

// listingFailureWhy is listingFailure with the cause an endpoint that never
// answered is blamed on, in plain words — which door did not open, or that
// nothing did — carrying the private-network advice only where it can be the
// explanation (#260).
func listingFailureWhy(name, endpoint string, status int, why string) string {
	switch {
	case status == 401 || status == 403:
		return fmt.Sprintf("'%s' rejected the API key for '%s' (HTTP %d) — replace it with: ccmorph config %s", endpoint, name, status, name)
	case status == 404:
		return fmt.Sprintf("'%s' has no model list (HTTP 404) — check the endpoint with: ccmorph config %s", endpoint, name)
	case status != 0:
		return fmt.Sprintf("'%s' answered the model list request with HTTP %d", endpoint, status)
	}
	return "could not reach '" + endpoint + "' — " + why
}
