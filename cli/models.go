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
	// A mode flag with no profile leaves the profile out, not wrong: the
	// picker still chooses one, as it does when the word is absent.
	if len(args) == 1 && (args[0] == "--filtered" || args[0] == "--filtered-json") {
		args = []string{"-i", args[0]}
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
	if !l.judgeProfile(name, "models") {
		return 1
	}
	if l.noModelList(name) {
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

// noModelList answers models and probe for a Claude account or token profile
// (#390): neither kind has a model list from an endpoint, and set's picker is
// what chooses its model. It reports whether it answered; any other kind, or a
// conf that does not read, goes on to the command's own checks and refusal.
// The command its advice names is listingAdvice's — the one policy for which
// command answers what a route carries (#562).
func (l *launcher) noModelList(name string) bool {
	f, ok := l.readQuiet(name)
	if !ok || (f["type"] != "sub" && f["type"] != "token") {
		return false
	}
	l.err("'" + name + "' is " + profile.KindPhrase(f["type"], f["provider"]) + " profile and has no model list from an endpoint")
	l.err("choose its model: " + listingAdvice(name, f["type"]))
	return true
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
	base, _ := nativeBase(f["base_url"])
	if f["api_key"] == "" {
		l.err("'" + name + "' has no api_key — model discovery needs one")
		return "", false
	}
	out, status, why, ok := util.ModelsAnswerWhy(base, f["api_key"])
	out = strings.TrimRight(out, "\n")
	if !ok {
		l.err(listingFailureWhy(name, profile.RedactURL(base), status, why))
		return "", false
	}
	return out, true
}

// listingFailure is what a person reads when the endpoint gave no model list:
// the status the endpoint refused with names the fix.
func listingFailure(name, endpoint string, status int) string {
	return listingFailureWhy(name, endpoint, status, "")
}

// listingFailureWhy is listingFailure with why, util.WhyUnreachable's cause for
// a request nothing answered (status 0): which of the four causes it was, with
// the host and port, and the private-network advice only when nothing answered
// at all. With no cause to name, the sentence says only that no list came back
// (#260).
func listingFailureWhy(name, endpoint string, status int, why string) string {
	switch {
	case status == 401 || status == 403:
		return fmt.Sprintf("'%s' rejected the API key for '%s' (HTTP %d) — replace it with: ccmorph config %s", endpoint, name, status, name)
	case status == 404:
		return fmt.Sprintf("'%s' has no model list (HTTP 404) — a model id must be given by hand: ccmorph set %s model <model-id>, or check the endpoint with: ccmorph config %s", endpoint, name, name)
	case status != 0:
		return fmt.Sprintf("'%s' answered the model list request with HTTP %d", endpoint, status)
	}
	if why == "" {
		why = "no model list came back"
	}
	return "could not reach '" + endpoint + "' — " + why
}
