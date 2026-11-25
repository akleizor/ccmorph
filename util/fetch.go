package util

import (
	"errors"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akleizor/ccmorph/ctxwindow"
)

// LaunchListing is the per-launch /v1/models listing: GET <base>/v1/models?limit=1000
// with x-api-key and Bearer headers, sending anthropic-version, the body
// undisguised. ok is false on a non-2xx answer or nothing. The ambient HTTPS_PROXY
// is honoured: it is how a bridge reaches a private endpoint.
func LaunchListing(base, key string, maxSeconds int) (body string, ok bool) {
	args := append([]string{"-sS", "-f"}, curlBounds(maxSeconds)...)
	return fetchModels(key, io.Discard, append(args,
		ctxwindow.APIURL(base, "/v1/models?limit=1000"), "-H", "anthropic-version: "+ctxwindow.AnthropicVersion)...)
}

// listingArgs is the one curl line every listing fetch asks with: silent, failing
// an HTTP error, the largest page, no anthropic-version (proxies disguise
// non-Claude ids when they see it) — the shape `ccmorph models` reads, so a
// route answers every reader of its listing with the same body. bounds are the
// caller's own and sit before the URL.
func listingArgs(base string, bounds ...string) []string {
	args := append([]string{"-sS", "-f"}, bounds...)
	return append(args, ctxwindow.APIURL(base, "/v1/models?limit=1000"))
}

// ModelsListing is the listing `ccmorph models` and model-known read: no
// anthropic-version (proxies disguise non-Claude ids when they see it). curl's
// complaint goes to errw.
func ModelsListing(base, key string, errw io.Writer) (body string, ok bool) {
	return fetchModels(key, errw, listingArgs(base, "--max-time", "20")...)
}

// ModelsAnswerWhy is ModelsListing for a person: curl's complaint is read, never
// printed, and status is the HTTP status the endpoint refused the request with,
// 0 when nothing answered; why is then the cause in plain words, "" when
// WhyUnreachable cannot name it (#260).
func ModelsAnswerWhy(base, key string) (body string, status int, why string, ok bool) {
	var complaint strings.Builder
	start := time.Now()
	raw, err := curlKeyed(key, listingArgs(base, "--max-time", "20"), &complaint)
	if err == nil && raw != "" {
		return undisguise(raw), 0, "", true
	}
	if m := refused.FindStringSubmatch(complaint.String()); m != nil {
		status, _ = strconv.Atoi(m[1])
		return "", status, "", false
	}
	return "", 0, WhyUnreachable(base, exitOf(err), complaint.String(), time.Since(start)), false
}

// ListingReply is the one listing request set's model check reads (gh728),
// with what that request met kept instead of asked again: body is the
// undisguised listing, empty when none came back; status is the HTTP status
// the endpoint refused the request with, 0 when the answer was not a refusal;
// answered is whether any HTTP answer arrived. The status curl writes out is
// the response's own word and is kept even when the transfer breaks after the
// status line (exit 18, 56) or curl -f fails an HTTP error (exit 22), so a
// body that did not arrive never turns an answer into silence. The flag is
// the long --write-out with %{response_code}, not -w with %{http_code}: the
// test stubs answer that shape as a status probe, and this is a listing.
func ListingReply(base, key string) (body string, status int, answered bool) {
	var complaint strings.Builder
	args := append(listingArgs(base, "--max-time", "20"), "--write-out", "\n"+statusMarker+"%{response_code}")
	raw, err := curlKeyed(key, args, &complaint)
	payload, code := splitListingReply(raw)
	if err == nil {
		return undisguise(payload), 0, true
	}
	if n, conv := strconv.Atoi(code); conv == nil && n != 0 {
		// Something answered before curl gave up: a refused status is named,
		// anything else is an answer whose listing did not arrive.
		if n >= 400 {
			status = n
		}
		return "", status, true
	}
	if m := refused.FindStringSubmatch(complaint.String()); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	var exit *exec.ExitError
	return "", status, status != 0 || (errors.As(err, &exit) && exit.ExitCode() == 22)
}

// splitListingReply splits the listing answer at the status marker curl writes
// out: the body before it, the HTTP status after it, "" when no marker came
// back — a stub that never answered that shape, or a curl too old for it.
func splitListingReply(raw string) (payload, code string) {
	if i := strings.LastIndex(raw, statusMarker); i >= 0 {
		return strings.TrimSuffix(raw[:i], "\n"), strings.TrimRight(raw[i+len(statusMarker):], "\n")
	}
	return raw, ""
}

func fetchModels(key string, errw io.Writer, curlArgs ...string) (string, bool) {
	body, err := curlKeyed(key, curlArgs, errw)
	if err != nil || body == "" {
		return "", false
	}
	return undisguise(body), true
}

// curlBounds is the one pair of probe bounds: 2 s to connect, maxSeconds whole.
// A bound spelled at each call site drifted; every probe asks here (#636).
func curlBounds(maxSeconds int) []string {
	return curlTimeouts("2", strconv.Itoa(maxSeconds))
}

// curlTimeouts is curl's two bound flags, spelled once: the probe bounds above
// and the reach check's own (#260) both build on it (#636).
func curlTimeouts(connect, max string) []string {
	return []string{"--connect-timeout", connect, "--max-time", max}
}

// ProbeListing is the never-failing probe behind doctor's route checks and the
// launch's native key check (#255): the body and the HTTP status, 000 when
// nothing answered. Each header is sent as a curl -H argument, so a native
// Anthropic check can ask for a version the way the launch route does.
func ProbeListing(base, key string, headers ...string) (body, code string) {
	body, code, _ = probeListing(base, key, false, headers)
	return body, code
}

// ProbeListingWhy is ProbeListing with why: when nothing answered (code "000"
// or ""), the cause in plain words, "" when WhyUnreachable cannot name it
// (#260). doctor's gateway row and the launch's route check read it.
func ProbeListingWhy(base, key string, headers ...string) (body, code, why string) {
	return probeListing(base, key, true, headers)
}

func probeListing(base, key string, name bool, headers []string) (body, code, why string) {
	args := append([]string{"-sS"}, curlBounds(5)...)
	args = append(args, "-w", `\n`+statusMarker+`%{http_code}`)
	for _, h := range headers {
		args = append(args, "-H", h)
	}
	var complaint strings.Builder
	start := time.Now()
	raw, err := curlKeyed(key, append(args, ctxwindow.APIURL(base, "/v1/models?limit=1000")), &complaint)
	body, code = splitProbeAnswer(raw, err)
	if name && ProbeVerdict(code) == ProbeSilent {
		why = WhyUnreachable(base, exitOf(err), complaint.String(), time.Since(start))
	}
	return undisguise(body), code, why
}

// statusMarker is the sentinel the never-failing probes ask curl to append to
// an answer: everything before it is the body, after it the HTTP status.
const statusMarker = "__CCMORPH_STATUS__"

// splitProbeAnswer splits a probe's raw answer at the status marker: the body
// before it with its trailing newline trimmed, the HTTP status after it, "000"
// when nothing answered or no marker came back. The verdict on that status is
// ProbeVerdict's; the third answer this splitter once carried was read nowhere
// (#516).
func splitProbeAnswer(body string, err error) (payload, code string) {
	if i := strings.LastIndex(body, statusMarker); err == nil && i >= 0 {
		return strings.TrimSuffix(body[:i], "\n"), strings.TrimRight(body[i+len(statusMarker):], "\n")
	}
	return "", "000"
}

// ProbeStatus is what a probe's HTTP status says about the credential it
// offered — the verdict, not the answer itself.
type ProbeStatus int

const (
	ProbeAccepted ProbeStatus = iota // a 2xx: the credential was taken
	ProbeRejected                    // 401 or 403: the credential was refused
	ProbeSilent                      // "000" or "": nothing answered
	ProbeOther                       // any other status: no verdict on the credential
)

// ProbeVerdict is the status switch every probe of a credential reads, spelled
// once (#488 gave it one copy in cli; #516 moved it down so doctor's four route
// checks read the same one): a 2xx accepts, 401/403 rejects, "000" or empty
// says nothing answered, and anything else is an answer that is no verdict on
// the credential — #255, #286 and #374 all landed in one of this switch's
// copies. The probe that produces the code stays each caller's own, with the
// header set its branch needs: a native endpoint is asked with the
// anthropic-version header its route sends, a gateway with none, doctor's own
// request.
func ProbeVerdict(code string) ProbeStatus {
	switch {
	case strings.HasPrefix(code, "2") && len(code) == 3:
		return ProbeAccepted
	case code == "401" || code == "403":
		return ProbeRejected
	case code == "000" || code == "":
		return ProbeSilent
	default:
		return ProbeOther
	}
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
	return curlFed(keyedConfig(key), args, errw)
}

// keyedConfig is the curl stdin config carrying both credential headers.
func keyedConfig(key string) string {
	esc := escCred(key)
	return "header = \"x-api-key: " + esc + "\"\nheader = \"Authorization: Bearer " + esc + "\"\n"
}

// curlBearer sends only the Bearer header an OAuth token travels as.
func curlBearer(token string, args []string, errw io.Writer) (string, error) {
	return curlFed("header = \"Authorization: Bearer "+escCred(token)+"\"\n", args, errw)
}

var (
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
	p := probeServed(want, typ, base, modelMap, key, modelsListingFetch)
	if p.decided {
		return p.answer
	}
	return Served(modelInListing(p.listing, p.want, p.modelMap))
}

// ContextServed reports whether the route's listing carries want the way
// `ccmorph models` prints it: with the id as spelled, so a model the map
// rewrites still counts through its target (#370). ModelServed stays the
// launch judgement — the target is what a session runs. The listing is asked
// with the probe's bounds, not the models verb's, because the caller is set's
// post-save warning: a route that will not answer must not hold the save (#449).
func ContextServed(want, typ, base, modelMap, key string) Served {
	p := probeServed(want, typ, base, modelMap, key, boundedListingFetch)
	if p.decided {
		return p.answer
	}
	if modelInListing(p.listing, p.want, "") == 0 {
		return ServedYes
	}
	return Served(modelInListing(p.listing, p.want, p.modelMap))
}

// ServedByListing is ModelServed's last step over a listing the caller already
// fetched: ServedYes when the listing carries want, ServedNo when a readable,
// complete one proves it absent, and ServedUnknown when the body is no listing
// the judgement can read at all — a {} answer, a paged page, a malformed body.
// That third state is the one that saves unchecked (gh728): an endpoint that
// answered is not one that checked.
func ServedByListing(listing, want, mapJSON string) Served {
	return Served(modelInListing(listing, want, mapJSON))
}

// listingFetch asks a route for the model listing a served judgement reads. ok
// is false when no body came back — nothing answered, or an HTTP error curl -f
// failed — which the judgement answers as ServedUnknown; a body that did come
// back is the listing reader's to judge.
type listingFetch func(base, key string) (body string, ok bool)

// modelsListingFetch is the full listing `ccmorph models` reads, with its 20 s
// whole-transfer bound.
func modelsListingFetch(base, key string) (string, bool) {
	return ModelsListing(base, key, io.Discard)
}

// boundedListingFetch asks the listing the models verb reads — the one curl
// line listingArgs builds, so a route answers it with the body `ccmorph models`
// prints — under the never-failing probe's bounds: 2 s to connect, 5 s whole,
// so a route that will not answer cannot hold a caller past them (#449). curl
// -f fails an HTTP error and passes anything else through for the listing's
// reader to judge; the probe's own marker shape is not used here: it asks for a
// status, and a status-only answer is not a listing.
func boundedListingFetch(base, key string) (string, bool) {
	return fetchModels(key, io.Discard, listingArgs(base, curlBounds(5)...)...)
}

// servedProbe is the shared prologue of the two served judgements: the
// judgement a listing is not asked for, the native route's unused map dropped,
// the empty-key answer and the listing fetch. decided says answer is final;
// otherwise listing is the fetched body and want and modelMap are what the
// caller's own last step judges.
type servedProbe struct {
	listing        string
	want, modelMap string
	answer         Served
	decided        bool
}

func probeServed(want, typ, base, modelMap, key string, fetch listingFetch) servedProbe {
	p := servedProbe{}
	p.want, p.answer, p.decided = servedBeforeListing(want, typ, base)
	if p.decided {
		return p
	}
	// Native routes launch without an adapter, so their maps are unused.
	if strings.TrimSuffix(base, "/") == "https://api.anthropic.com" {
		modelMap = ""
	}
	p.modelMap = modelMap
	if strings.TrimSpace(key) == "" {
		p.answer, p.decided = ServedUnknown, true
		return p
	}
	listing, ok := fetch(base, key)
	if !ok {
		p.answer, p.decided = ServedUnknown, true
		return p
	}
	p.listing = listing
	return p
}

// servedBeforeListing is the judgement a listing is not asked for: want minus
// any [1m], the tier aliases, and the provider and bare-route answers. decided
// says the answer is final; otherwise the caller asks the route's listing.
func servedBeforeListing(want, typ, base string) (spelled string, answer Served, decided bool) {
	// The strip is TrimWindowMarker's and the word TierWord's — the one answer
	// each gives everywhere a model is named — so a tier word in either case,
	// with or without its marker, is served here exactly as it resolves
	// elsewhere (#539).
	want = TrimWindowMarker(want)
	word := TierWord(want)
	switch {
	case word == "":
		return want, ServedUnknown, true
	case IsTier(word):
		return want, ServedYes, true
	case typ == "provider":
		if strings.HasPrefix(want, "claude-") {
			return want, ServedNo, true
		}
		return want, ServedUnknown, true
	case base == "":
		if strings.HasPrefix(want, "claude-") {
			return want, ServedYes, true
		}
		return want, ServedNo, true
	}
	return want, ServedUnknown, false
}
