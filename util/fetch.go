package util

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
	"github.com/akleizor/ccmorph/profile"
)

// LaunchListing is the per-launch /v1/models listing: GET <base>/v1/models?limit=1000
// with x-api-key and Bearer headers, sending anthropic-version, the body
// undisguised. ok is false on a non-2xx answer or nothing. The ambient HTTPS_PROXY
// is honoured: it is how a bridge reaches a private endpoint.
func LaunchListing(base, key string, maxSeconds int) (body string, ok bool) {
	body, ok, _ = fetchModels(key, io.Discard, "-sS", "-f", "--connect-timeout", "2", "--max-time", strconv.Itoa(maxSeconds),
		apiURL(base, "/v1/models?limit=1000"), "-H", "anthropic-version: 2023-06-01")
	return body, ok
}

// ModelsListing is the listing `ccmorph models` and model-known read: no
// anthropic-version (proxies disguise non-Claude ids when they see it). curl's
// complaint goes to errw.
func ModelsListing(base, key string, errw io.Writer) (body string, ok bool) {
	body, ok, _ = fetchModels(key, errw, "-sS", "-f", "--max-time", "20", apiURL(base, "/v1/models?limit=1000"))
	return body, ok
}

// ModelsAnswer is ModelsListing for a person: curl's complaint is read, never
// printed, status is the HTTP status the endpoint refused the request with, 0
// when nothing answered, and why names that nothing in plain words.
func ModelsAnswer(base, key string) (body string, status int, why string, ok bool) {
	var complaint strings.Builder
	body, ok, exit := fetchModels(key, &complaint, "-sS", "-f", "--max-time", "20", apiURL(base, "/v1/models?limit=1000"))
	if !ok {
		if m := refused.FindStringSubmatch(complaint.String()); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
		if status == 0 {
			why = WhyUnreachable(base, exit, complaint.String(), 20)
		}
	}
	return body, status, why, ok
}

// fetchModels is one curl-backed listing: the body when it answered, and curl's
// exit status for the caller that must name why it did not.
func fetchModels(key string, errw io.Writer, curlArgs ...string) (string, bool, int) {
	body, err := curlKeyed(key, curlArgs, errw)
	if err != nil || body == "" {
		return "", false, exitOf(err)
	}
	return undisguise(body), true, 0
}

// probeSeconds is doctor's own patience with a silent gateway: five seconds.
const probeSeconds = 5

// ProbeListing is the never-failing probe behind doctor's route checks, the
// launch's native key check (#255) and add's try (#254): the body, the HTTP
// status, 000 when nothing answered, and why naming that nothing in plain words.
// Each header is sent as a curl -H argument, so a native Anthropic check can ask
// for a version the way the launch route does.
func ProbeListing(base, key string, headers ...string) (body, code, why string) {
	args := []string{"-sS", "--connect-timeout", "2", "--max-time", "5", "-w", `\n__CCMORPH_STATUS__%{http_code}`}
	for _, h := range headers {
		args = append(args, "-H", h)
	}
	var complaint strings.Builder
	body, err := curlKeyed(key, append(args, apiURL(base, "/v1/models?limit=1000")), &complaint)
	code = "000"
	if i := strings.LastIndex(body, "__CCMORPH_STATUS__"); err == nil && i >= 0 {
		body, code = strings.TrimSuffix(body[:i], "\n"), strings.TrimRight(body[i+len("__CCMORPH_STATUS__"):], "\n")
	} else {
		body = ""
	}
	if code == "000" || code == "" {
		why = WhyUnreachable(base, exitOf(err), complaint.String(), probeSeconds)
	}
	return undisguise(body), code, why
}

// curlFed runs curl with a credential fed through its stdin config (-K -), so
// a credential never appears on a command line.
// shortcut: curl is the transport so the gateway test stubs keep answering; move to
// net/http when those fixtures become a loopback server.
func curlFed(config string, args []string, errw io.Writer) (string, error) {
	cmd := exec.Command("curl", append([]string{"-K", "-"}, args...)...)
	cmd.Stdin = strings.NewReader(config)
	cmd.Stderr = errw
	b, err := cmd.Output()
	return string(b), err
}

// escCred escapes a credential for one line of curl's stdin config.
func escCred(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`)
}

// curlKeyed sends both credential headers, because a gateway may take either.
func curlKeyed(key string, args []string, errw io.Writer) (string, error) {
	return curlFed(credConfig(key), args, errw)
}

// credConfig is the curl stdin config that carries both credential headers.
func credConfig(key string) string {
	esc := escCred(key)
	return "header = \"x-api-key: " + esc + "\"\nheader = \"Authorization: Bearer " + esc + "\"\n"
}

// curlBearer sends only the Bearer header an OAuth token travels as.
func curlBearer(token string, args []string, errw io.Writer) (string, error) {
	return curlFed("header = \"Authorization: Bearer "+escCred(token)+"\"\n", args, errw)
}

// WhyUnreachable names the one cause of a gateway that cannot be reached, in
// plain words with the host and port: curl's exit status tells which of the four
// transport failures happened, its complaint says how long nothing answered. A
// cause is named only when curl's own words confirm it: a connect failure curl
// gives no reason for is the no-answer case, so nothing is claimed that cannot
// be seen. The private-network advice rides only on the no-answer case, where a
// private endpoint is the likely explanation; no case quotes curl, whose words
// are not a person's, and none repeats the credential the URL may carry.
func WhyUnreachable(endpoint string, exit int, complaint string, boundSeconds int) string {
	where := whereAnswered(endpoint)
	switch {
	case exit == 6:
		return "name not found: " + where
	case exit == 7 && noRoute.MatchString(complaint):
		return "no route to host: " + where
	case exit == 7 && connectFailed.MatchString(complaint):
		return "connection refused: " + where
	case exit == 7:
		// curl gave no reason, so nothing is known to have answered: the
		// no-answer case, without a time curl never reported (#260).
		return noAnswer(where, "")
	case exit == 35 || exit == 60:
		return "secure connection failed: " + where
	case exit == 28 || exit < 0:
		n := boundSeconds
		if m := afterMilli.FindStringSubmatch(complaint); m != nil {
			if ms, err := strconv.Atoi(m[1]); err == nil && ms > 0 {
				n = max(ms/1000, 1)
			}
		}
		word := "seconds"
		if n == 1 {
			word = "second"
		}
		return noAnswer(where, fmt.Sprintf(" after %d %s", n, word))
	}
	return "the request failed without an answer"
}

// noAnswer is the no-answer sentence, the only one that carries the
// private-network advice: a private endpoint is its likely explanation.
// after is how long nothing answered, empty when curl did not say.
func noAnswer(where, after string) string {
	what := "no answer"
	if where != "" {
		what += " from " + where
	}
	return what + after + " (if it is an internal endpoint, connect your private network and try again)"
}

// SaysNoAnswer is whether why is the no-answer sentence: silence, the one cause
// whose every later wait would hang the same way. That is what separates it
// from the instant failures for a launch — a refused port or a name that does
// not resolve is answered at once, so the launch can be named at and keep
// going, while nothing downstream of a silent gateway ever comes back (#260).
func SaysNoAnswer(why string) bool {
	return strings.HasPrefix(why, "no answer")
}

// SaysWaitedNoAnswer is a no-answer sentence that names the seconds waited: the
// probe really sat through the silence (curl timed out, or it was killed at its
// bound), as opposed to a connect failure that came back at once with no reason
// (#260).
func SaysWaitedNoAnswer(why string) bool {
	return SaysNoAnswer(why) && strings.Contains(why, " after ")
}

// whereAnswered is the host and port a URL asked for, with the scheme's default
// port when the URL names none: the two facts that tell a person which door did
// not open. It reads RedactURL's cut, never the endpoint: a password may open on
// digits and hold a /, which Go reads as this host's port
// (alice:2024/ss@proxy:3128), so the credential-safe cut runs before any parse —
// the same one every other diagnostic gets.
func whereAnswered(endpoint string) string {
	u, err := url.Parse(profile.RedactURL(endpoint))
	if err != nil || u.Host == "" || u.Hostname() == "" {
		return ""
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Hostname() + " port " + port
}

// exitOf is a command's exit status, -1 when a signal ended it, 0 when it cannot
// be told.
func exitOf(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

// apiURL joins an API root or a versioned SDK base (".../v1") with a /v1 path.
func apiURL(base, path string) string {
	base = strings.TrimSuffix(base, "/")
	if versioned.MatchString(strings.ToLower(base)) {
		path = strings.TrimPrefix(path, "/v1")
	}
	return base + path
}

var (
	versioned      = regexp.MustCompile(`/v[0-9]+$`)
	quotedDisguise = regexp.MustCompile(`"(claude-[^"]*-dd-[^"]+)"`)
	refused        = regexp.MustCompile(`returned error: ([0-9]{3})`)
	afterMilli     = regexp.MustCompile(`after ([0-9]+) *(?:ms|milliseconds)`)
	noRoute        = regexp.MustCompile(`(?i)no route to host|network is unreachable`)
	connectFailed  = regexp.MustCompile(`(?i)connection refused|couldn't connect to server|failed to connect`)
)

// undisguise restores every quoted claude-<cover>-dd-<reversed> id to its real id.
func undisguise(body string) string {
	return quotedDisguise.ReplaceAllStringFunc(body, func(q string) string {
		return `"` + ctxwindow.RealID(q[1:len(q)-1]) + `"`
	})
}

// Served is whether a route serves a model.
type Served int

const (
	ServedYes Served = iota
	ServedNo
	ServedUnknown // cannot tell
)

// ModelServed reports whether the route serves want. Tier aliases always resolve; a
// native route needs a claude-* id; a provider's catalogue lives in its bridge, so
// only a claude-* id is a definite no there.
func ModelServed(want, typ, base, modelMap, key string) Served {
	if strings.HasSuffix(strings.ToLower(want), "[1m]") {
		want = want[:len(want)-4]
	}
	switch {
	case want == "":
		return ServedUnknown
	case want == "opus" || want == "sonnet" || want == "haiku" || want == "fable":
		return ServedYes
	case typ == "provider":
		if strings.HasPrefix(want, "claude-") {
			return ServedNo
		}
		return ServedUnknown
	case base == "":
		if strings.HasPrefix(want, "claude-") {
			return ServedYes
		}
		return ServedNo
	}
	// Native routes launch without an adapter, so their maps are unused.
	if strings.TrimSuffix(base, "/") == "https://api.anthropic.com" {
		modelMap = ""
	}
	if strings.TrimSpace(key) == "" {
		return ServedUnknown
	}
	listing, ok := ModelsListing(base, key, io.Discard)
	if !ok {
		return ServedUnknown
	}
	return Served(modelInListing(listing, want, modelMap))
}
