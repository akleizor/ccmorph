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

// DoctorRoute is doctor's read-only gateway and adapter checks for a profile with
// the given base_url, adapter flag and credential; failed is true when a row is
// FAIL. Nothing is started and nothing is written.
func DoctorRoute(name, base string, adapter bool, key string) (rows []DoctorRow, failed bool) {
	row := func(kind, msg string) {
		if kind == "FAIL" {
			failed = true
		}
		rows = append(rows, DoctorRow{kind, msg})
	}

	if base == "" {
		row("SKIP", "gateway: profile has no base_url (native endpoint)")
	} else {
		valid := httpURL.MatchString(base)
		if !valid {
			row("FAIL", "config: base_url must be an http(s) URL without query or fragment")
		}
		row("", "endpoint: "+profile.RedactURL(base))
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
				row("SKIP", "gateway: endpoint did not answer (no changes made)")
			default:
				row("FAIL", fmt.Sprintf("gateway: unreachable or rejected (HTTP %s)", code))
			}
		}
	}

	switch {
	case adapter:
		row("INFO", "adapter: configured=true (not started by read-only doctor)")
	case base != "":
		row("INFO", "adapter: automatic compatibility may apply at launch (not started by doctor)")
	default:
		row("SKIP", "adapter: native profile path (not started by doctor)")
	}
	if base != "" {
		running := runningAdapter(name)
		switch {
		case running == "__UNKNOWN__":
			row("SKIP", "adapter: status unknown (process inspection unavailable; not started by doctor)")
		case running == "":
			row("INFO", "adapter: not running (not started by read-only doctor)")
		default:
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
