package util

import (
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// LaunchListing is the per-launch /v1/models listing: GET <base>/v1/models?limit=1000
// with x-api-key and Bearer headers, sending anthropic-version, the body
// undisguised. ok is false on a non-2xx answer or nothing. The ambient HTTPS_PROXY
// is honoured: it is how a bridge reaches a private endpoint.
func LaunchListing(base, key string, maxSeconds int) (body string, ok bool) {
	return fetchModels(key, io.Discard, "-sS", "-f", "--connect-timeout", "2", "--max-time", strconv.Itoa(maxSeconds),
		apiURL(base, "/v1/models?limit=1000"), "-H", "anthropic-version: 2023-06-01")
}

// ModelsListing is the listing `ccmorph models` and model-known read: no
// anthropic-version (proxies disguise non-Claude ids when they see it). curl's
// complaint goes to errw.
func ModelsListing(base, key string, errw io.Writer) (body string, ok bool) {
	return fetchModels(key, errw, "-sS", "-f", "--max-time", "20", apiURL(base, "/v1/models?limit=1000"))
}

// ModelsAnswer is ModelsListing for a person: curl's complaint is read, never
// printed, and status is the HTTP status the endpoint refused the request with,
// 0 when nothing answered.
func ModelsAnswer(base, key string) (body string, status int, ok bool) {
	var complaint strings.Builder
	body, ok = ModelsListing(base, key, &complaint)
	if !ok {
		if m := refused.FindStringSubmatch(complaint.String()); m != nil {
			status, _ = strconv.Atoi(m[1])
		}
	}
	return body, status, ok
}

func fetchModels(key string, errw io.Writer, curlArgs ...string) (string, bool) {
	body, err := curlKeyed(key, curlArgs, errw)
	if err != nil || body == "" {
		return "", false
	}
	return undisguise(body), true
}

// ProbeListing is the never-failing probe behind doctor's route checks and the
// launch's native key check (#255): the body and the HTTP status, 000 when
// nothing answered. Each header is sent as a curl -H argument, so a native
// Anthropic check can ask for a version the way the launch route does.
func ProbeListing(base, key string, headers ...string) (body, code string) {
	args := []string{"-sS", "--connect-timeout", "2", "--max-time", "5", "-w", `\n__CCMORPH_STATUS__%{http_code}`}
	for _, h := range headers {
		args = append(args, "-H", h)
	}
	body, err := curlKeyed(key, append(args, apiURL(base, "/v1/models?limit=1000")), io.Discard)
	code = "000"
	if i := strings.LastIndex(body, "__CCMORPH_STATUS__"); err == nil && i >= 0 {
		body, code = strings.TrimSuffix(body[:i], "\n"), strings.TrimRight(body[i+len("__CCMORPH_STATUS__"):], "\n")
	} else {
		body = ""
	}
	return undisguise(body), code
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
	esc := escCred(key)
	return curlFed("header = \"x-api-key: "+esc+"\"\nheader = \"Authorization: Bearer "+esc+"\"\n", args, errw)
}

// curlBearer sends only the Bearer header an OAuth token travels as.
func curlBearer(token string, args []string, errw io.Writer) (string, error) {
	return curlFed("header = \"Authorization: Bearer "+escCred(token)+"\"\n", args, errw)
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
