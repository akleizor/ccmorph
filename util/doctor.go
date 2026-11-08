package util

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

var httpURL = regexp.MustCompile(`^https?://[^?#]*$`)

// DoctorRow is one line of doctor's route report: a status (OK, FAIL, WARN,
// INFO, SKIP) and its message; an empty Kind is a bare line.
type DoctorRow struct{ Kind, Msg string }

// nativeBase lists the endpoints that need no adapter: launch treats these as
// Anthropic's own, where only the key is in question.
func nativeBase(base string) bool {
	return base == "" || base == "https://api.anthropic.com" || base == "https://api.anthropic.com/"
}

// DoctorRoute is doctor's read-only endpoint and adapter checks for a profile of
// the given type, base_url, adapter flag and credential; failed is true when a
// row is FAIL. verbose adds a running adapter's processes and ports; the default
// report says only what launch will do, which is what a reader can act on.
// Nothing is started and nothing is written.
func DoctorRoute(name, typ, base string, adapter bool, key string, verbose bool) (rows []DoctorRow, failed bool) {
	row := func(kind, msg string) {
		if kind == "FAIL" {
			failed = true
		}
		rows = append(rows, DoctorRow{kind, msg})
	}

	if nativeBase(base) {
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
				row("FAIL", "credentials: Anthropic rejected the API key (HTTP "+code+"); replace it: ccmorph config "+name)
			case code == "000" || code == "":
				row("WARN", "endpoint: Anthropic did not answer, so the key was not checked (check your network)")
			default:
				row("WARN", "endpoint: Anthropic answered HTTP "+code+", so the key was not checked")
			}
		case typ == "apikey":
			row("SKIP", "endpoint: Anthropic; key not checked (no API key stored)")
		default:
			row("INFO", "endpoint: Anthropic (claude checks this profile's sign-in at launch)")
		}
		// Launch starts no adapter on a native endpoint, so there is no
		// adapter row to print.
		return rows, failed
	}

	valid := httpURL.MatchString(base)
	if !valid {
		row("FAIL", "config: base_url must be an http(s) URL without query or fragment")
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
				row("OK", fmt.Sprintf("gateway: reachable (HTTP %s); models: %d", code, n))
			} else {
				row("FAIL", fmt.Sprintf("gateway: malformed model listing (HTTP %s)", code))
			}
		case code == "000" || code == "":
			row("WARN", "gateway: did not answer within 5 seconds (if it is a private endpoint, connect that network)")
		case code == "401" || code == "403":
			row("FAIL", "gateway: rejected the API key (HTTP "+code+"); replace it: ccmorph config "+name)
		default:
			row("FAIL", fmt.Sprintf("gateway: unreachable or rejected (HTTP %s)", code))
		}
	}

	// One adapter line answers the reader's question — what happens at launch —
	// and says so when a session already uses one. Pids, listeners and a failed
	// process inspection are internal detail: they appear only with --verbose.
	what := "adapter: ccmorph run starts a compatibility adapter if this endpoint needs one"
	if adapter {
		what = "adapter: ccmorph run starts a compatibility adapter for this endpoint (adapter=true)"
	}
	running := runningAdapter(name)
	if running != "" && running != "__UNKNOWN__" {
		what += "; an open session on this profile is using one now"
	}
	row("INFO", what)
	if verbose {
		switch {
		case running == "__UNKNOWN__":
			row("SKIP", "adapter: status unknown (process inspection unavailable; not started by doctor)")
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
	return rows, failed
}
