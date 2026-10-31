package util

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var httpURL = regexp.MustCompile(`^https?://[^?#]*$`)

// doctorRoute is `util doctor-route <profile> <base_url> <adapter flag>`, the
// credential on stdin: doctor's read-only gateway and adapter checks, as
// `<OK|FAIL|WARN|INFO|SKIP>\t<message>` rows plus one bare `endpoint: <redacted>`
// line. Exit 1 when a row is FAIL. Nothing is started and nothing is written.
func doctorRoute(args []string, in io.Reader, out, _ io.Writer) int {
	if len(args) != 3 {
		return 2
	}
	name, base, adapter := args[0], args[1], args[2]
	key, _ := bufio.NewReader(in).ReadString('\n')
	key = strings.TrimRight(key, "\r\n")
	failed := 0
	row := func(kind, msg string) {
		if kind == "FAIL" {
			failed = 1
		}
		fmt.Fprintf(out, "%s\t%s\n", kind, msg)
	}
	run := func(f verb, in io.Reader, args ...string) (string, int) {
		var b strings.Builder
		rc := f(args, in, &b, io.Discard)
		return strings.TrimRight(b.String(), "\n"), rc
	}

	if base == "" {
		row("SKIP", "gateway: profile has no base_url (native endpoint)")
	} else {
		valid := httpURL.MatchString(base)
		if !valid {
			row("FAIL", "config: base_url must be an http(s) URL without query or fragment")
		}
		safe, rc := run(redactURL, nil, base)
		if rc != 0 {
			safe = "[unparseable endpoint]"
		}
		fmt.Fprintln(out, "endpoint: "+safe)
		switch {
		case !valid:
			row("SKIP", "gateway: invalid endpoint (no request sent)")
		case key == "":
			row("SKIP", "gateway: no API credential for metadata request")
		default:
			payload, _ := run(fetchListing, strings.NewReader(key+"\n"), base, "doctor")
			body, code := "", payload
			if i := strings.LastIndex(payload, "\n"); i >= 0 {
				body, code = payload[:i], payload[i+1:]
			}
			switch {
			case strings.HasPrefix(code, "2") && len(code) == 3:
				if n, rc := run(modelsCount, strings.NewReader(body)); rc == 0 {
					row("OK", fmt.Sprintf("gateway: reachable (HTTP %s); models: %s", code, n))
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
	case adapter == "true":
		row("INFO", "adapter: configured=true (not started by read-only doctor)")
	case base != "":
		row("INFO", "adapter: automatic compatibility may apply at launch (not started by doctor)")
	default:
		row("SKIP", "adapter: native profile path (not started by doctor)")
	}
	if base != "" {
		running, _ := run(runningAdapter, nil, name)
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
				listener, _ := run(listenerOf, nil, cpid)
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
	return failed
}
