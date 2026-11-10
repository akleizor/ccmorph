package util

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

var httpURL = regexp.MustCompile(`^https?://[^?#]*$`)

// DoctorRow is one line of doctor's route report: a status (OK, FAIL, WARN,
// INFO, SKIP) and its message; an empty Kind is a bare line. Fix is the command
// or step that repairs a FAIL row, and doctor's verdict names the first one (#282).
type DoctorRow struct{ Kind, Msg, Fix string }

// NativeBase lists the endpoints that need no adapter: launch treats these as
// Anthropic's own, where only the key is in question.
func NativeBase(base string) bool {
	return base == "" || base == "https://api.anthropic.com" || base == "https://api.anthropic.com/"
}

// TranslatorNeed is doctor's verdict on whether a launch of this profile would
// start the translator: Needed is meaningful only when Known is true, which says
// the adapter cache or the endpoint's own model listing decides it. The caller
// computes it from what a launch itself reads without inference, so doctor
// reports what launch would actually do, never a guess.
type TranslatorNeed struct {
	Known     bool // the cache or the listing's declarations decide the need
	Needed    bool // at least one usable model needs the translator
	Installed bool // cliproxyapi can be found and run
	Pickable  bool // an automatic launch — no model named — could pick a model from the listing (#379)
}

// DoctorRoute is doctor's read-only endpoint and adapter checks for a profile of
// the given type, base_url and credential; home is the profile home, where a
// sub profile's sign-in is read when Claude Code keeps it there. failed is true
// when a row is FAIL, and keyRejected is true when the endpoint itself refused
// the credential — the caller uses it to drop Claude Code's locally stored
// answer, which would otherwise call a rejected key fine (GH-272). model and
// modelMap are the profile's saved default and map, so a reachable listing can
// adjudicate the default (#267). verbose adds a running adapter's processes and
// ports; the default report says only what launch will do, which is what a
// reader can act on. Nothing is started and nothing is written.
func DoctorRoute(name, typ, base, model, modelMap string, need func(listing string) TranslatorNeed, key, home string, verbose bool) (rows []DoctorRow, failed, keyRejected bool) {
	row := func(kind, msg string, fix ...string) {
		if kind == "FAIL" {
			failed = true
		}
		r := DoctorRow{Kind: kind, Msg: msg}
		if len(fix) > 0 {
			r.Fix = fix[0]
		}
		rows = append(rows, r)
	}

	if NativeBase(base) {
		// Launch sends no request here, so this is the only place a key on
		// Anthropic's own endpoint can be checked before a session fails.
		switch {
		case typ == "provider":
			row("INFO", "endpoint: provider bridge, started by ccmorph run")
		case typ == "apikey" && key != "":
			_, code := ProbeListing("https://api.anthropic.com", key, "anthropic-version: 2023-06-01")
			switch {
			case strings.HasPrefix(code, "2") && len(code) == 3:
				row("OK", "endpoint: Anthropic accepted the API key (HTTP "+code+")")
			case code == "401" || code == "403":
				row("FAIL", "credentials: Anthropic rejected the API key (HTTP "+code+"); replace it: ccmorph config "+name,
					"replace the API key: ccmorph config "+name)
				keyRejected = true
			case code == "000" || code == "":
				row("WARN", "endpoint: Anthropic did not answer, so the key was not checked (check your network)")
			default:
				row("WARN", "endpoint: Anthropic answered HTTP "+code+", so the key was not checked")
			}
		case typ == "apikey":
			row("SKIP", "endpoint: Anthropic; key not checked (no API key stored)")
		case typ == "token" && key != "":
			// A token is spent as a Bearer on Anthropic's own endpoint, and
			// launch sends no request first: this is the only place a token can
			// be checked before a session fails.
			code := ProbeToken(key)
			switch {
			case strings.HasPrefix(code, "2") && len(code) == 3:
				row("OK", "credentials: Anthropic accepted the token (HTTP "+code+")")
			case code == "401" || code == "403":
				row("FAIL", "credentials: Anthropic rejected the token (HTTP "+code+"); replace it: ccmorph config "+name,
					"replace the token: ccmorph config "+name)
				keyRejected = true
			case code == "000" || code == "":
				row("WARN", "endpoint: Anthropic did not answer, so the token was not checked (check your network)")
			default:
				row("WARN", "endpoint: Anthropic answered HTTP "+code+", so the token was not checked")
			}
		case typ == "sub":
			// The sign-in is a sub profile's whole credential, and launch sends
			// no request first: this is the only place it can be asked about.
			// Where Claude Code keeps it in the profile home it is read and
			// probed; where it does not (macOS keeps it in the login keychain),
			// the report says it was not checked instead of vouching for it.
			// A rejection is a warning, not a failure: a sign-in is refreshable
			// by design, and the next session refreshes it if it can.
			token := StoredSignin(home)
			switch {
			case token == "":
				row("WARN", "credentials: the sign-in was not checked (Claude Code keeps it outside the profile home; the next session refreshes it if it can)")
			default:
				code := ProbeToken(token)
				switch {
				case strings.HasPrefix(code, "2") && len(code) == 3:
					row("OK", "credentials: Anthropic accepted the stored sign-in (HTTP "+code+")")
				case code == "401" || code == "403":
					row("WARN", "credentials: Anthropic rejected the stored sign-in (HTTP "+code+"); the next session refreshes it if it can, else it opens the login menu")
				case code == "000" || code == "":
					row("WARN", "endpoint: Anthropic did not answer, so the sign-in was not checked (check your network)")
				default:
					row("WARN", "endpoint: Anthropic answered HTTP "+code+", so the sign-in was not checked")
				}
			}
		default:
			row("SKIP", "endpoint: Anthropic; no credential to check")
		}
		// Launch starts no adapter on a native endpoint, so there is no
		// adapter row to print.
		return rows, failed, keyRejected
	}

	listing := ""
	valid := httpURL.MatchString(base)
	if !valid {
		row("FAIL", "config: endpoint must be an http(s) URL without query or fragment", "set an http(s) endpoint: ccmorph config "+name)
	}
	row("INFO", "endpoint: "+profile.RedactURL(base))
	switch {
	case !valid:
		row("SKIP", "gateway: invalid endpoint (no request sent)")
	case key == "":
		row("SKIP", "gateway: no API credential for metadata request")
	default:
		body, code := ProbeListing(base, key)
		switch {
		case strings.HasPrefix(code, "2") && len(code) == 3:
			if n, ok := modelsCount(body); ok {
				listing = body
				row("OK", fmt.Sprintf("gateway: reachable (HTTP %s); models: %d", code, n))
				// A listing that answers and names models adjudicates the saved
				// default: ready would promise a launch that fails at its first
				// message (#267).
				if typ == "apikey" {
					if msg, refuse := ServedRefusal(body, model, modelMap, "this gateway"); refuse {
						// With nothing served there is no model to choose: the fix is on the
						// gateway, or in the endpoint (#282).
						fix := "choose a served model: ccmorph set " + name + " model <model-id>"
						if strings.HasSuffix(msg, "; served: (none)") {
							fix = "make a chat model available on this gateway, or change the endpoint: ccmorph config " + name
						}
						row("FAIL", "model: "+msg, fix)
					}
				}
			} else {
				row("FAIL", fmt.Sprintf("gateway: malformed model listing (HTTP %s)", code),
					"check the endpoint (it must list models at /v1/models): ccmorph config "+name)
			}
		case code == "000" || code == "":
			row("WARN", "gateway: did not answer within 5 seconds (if it is a private endpoint, connect that network)")
		case code == "401" || code == "403":
			row("FAIL", "gateway: rejected the API key (HTTP "+code+"); replace it: ccmorph config "+name,
				"replace the API key: ccmorph config "+name)
			keyRejected = true
		case code == "404":
			// The gateway answered, so it is not unreachable: it offers no model
			// list (#357). A saved model still launches — classification probes
			// it directly — and cannot be checked against a listing; without
			// one, no launch can pick a model, so the report names the step.
			if strings.TrimSpace(model) != "" {
				row("OK", "gateway: reachable (HTTP 404), but offers no model list; the saved model cannot be checked against one")
			} else {
				row("FAIL", "gateway: reachable (HTTP 404), but offers no model list and no model is set",
					"set a model: ccmorph set "+name+" model <model-id>")
			}
		default:
			row("FAIL", fmt.Sprintf("gateway: unreachable or rejected (HTTP %s)", code),
				"check the endpoint, or change it: ccmorph config "+name)
		}
	}

	// One adapter line answers the reader's question — does this profile need a
	// translator now, and can launch find one — or is left out when only a billed
	// check could answer. Pids, listeners and a failed ps are
	// internal detail: --verbose only.
	running := runningAdapter(name)
	if kind, msg := adapterLine(name, model, listing, running, need(listing)); kind != "" {
		fix := ""
		if kind == "FAIL" {
			fix = "install the translator: " + ClipInstallStep()
		}
		row(kind, msg, fix)
	}
	if verbose {
		switch {
		case running == "__UNKNOWN__":
			row("SKIP", "adapter: could not tell whether a translator is running")
		case running != "":
			pid, children, _ := strings.Cut(running, "\t")
			for _, spec := range strings.Split(children, ",") {
				cpid, cname, _ := strings.Cut(spec, ":")
				what := cname + " child"
				if cpid == pid {
					what = "in-process shim"
				}
				listener := listenerOf(cpid)
				switch {
				case listener == "__UNKNOWN__":
					row("SKIP", fmt.Sprintf("adapter: %s listener status unknown (pid %s)", what, cpid))
				case listener != "":
					row("OK", fmt.Sprintf("adapter: %s listener %s (pid %s; read-only check)", what, listener, cpid))
				default:
					row("WARN", fmt.Sprintf("adapter: %s running without TCP listener (pid %s)", what, cpid))
				}
			}
		}
	}
	return rows, failed, keyRejected
}

// adapterLine is the one adapter row, or none (kind ""). running is
// runningAdapter's answer and listing the model list doctor read ("" if none).
// The first applicable answer wins: what launch would do from the cache and the
// declared protocols — but only where a launch without a model could act on it,
// because a cached row for a model the listing no longer serves decides nothing
// such a launch could pick (#379); else what an open session shows; else, when
// no model is set and none can be picked from the listing, that no translator
// starts until one is chosen — and that a hop here will need a saved model,
// because a resumed hop opens no model picker (#379). A saved model nothing has
// classified is left out: only a billed check could answer it, and doctor
// sends none.
func adapterLine(name, model, listing, running string, n TranslatorNeed) (kind, msg string) {
	translator := running != "__UNKNOWN__" && strings.Contains(running, ":cliproxyapi")
	inUse := ""
	if translator {
		inUse = "; an open session on this profile is using one now"
	}
	// No model is set, the listing leaves the adapter nothing to pick, and no
	// open session shows this profile running: a hop here needs a saved model
	// (#379). An open session answers first — it already shows the profile at
	// work — so the warning never displaces it.
	stale := strings.TrimSpace(model) == "" && listing != "" && !n.Pickable &&
		!(running != "" && running != "__UNKNOWN__")
	switch {
	case stale:
		installed := "cliproxyapi is not installed"
		if n.Installed {
			installed = "cliproxyapi is installed"
		}
		return "INFO", "adapter: this profile needs no translator until a model is chosen, and a hop here will need a saved model (none is set, and none can be picked from the endpoint's model list); " +
			installed + "; list them: ccmorph models " + name + ", then save one: ccmorph set " + name + " model <model-id>"
	case n.Known && n.Needed && n.Installed:
		return "OK", "adapter: this endpoint needs a translator; cliproxyapi is installed" + inUse
	case n.Known && n.Needed:
		return "FAIL", "adapter: this endpoint needs a translator; cliproxyapi is not installed; install it: " + ClipInstallStep() + inUse
	case n.Known:
		return "OK", "adapter: this endpoint needs no translator"
	case translator:
		return "OK", "adapter: this profile needs a translator now: an open session on it is running cliproxyapi"
	case running != "" && running != "__UNKNOWN__":
		return "OK", "adapter: this profile needs no translator now: its open session runs without one"
	}
	return "", ""
}
