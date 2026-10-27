// Package shim is the id-uniquifier relay that sits between Claude Code and an
// LLM gateway.
//
// Root cause of a run of session deaths traced on a private gateway: its
// Anthropic translation mints DETERMINISTIC tool_use ids (Bash_0, Edit_115).
// Claude Code's transcript repair treats a repeated id as a corrupted duplicate
// and strips the block from the model's context, planting "[Tool use
// interrupted]" placeholders in its place; the model then spirals and finally
// parrots the placeholder as its own reply, poisoning every later turn.
//
// This shim kills the seed at the wire: every tool_use id in a response is
// rewritten to a globally unique value before Claude Code sees it. Claude Code
// echoes the rewritten id in its tool_result, so the history stays
// self-consistent; the upstream treats ids as opaque.
//
// Everything else passes through: non-SSE responses are byte-identical unless
// they are JSON that needs an edit; SSE streams are filtered line by line so
// streaming latency is unchanged. Ids are toolu_shim_<per-process-random>_<n>;
// the random half is load-bearing (see uniqueID).
//
// Requests keep the client's schema: body, metadata, the User-Agent and every
// header go upstream exactly as sent, except one per-profile opt-in:
// --random-device-id swaps the device_id inside metadata.user_id for a random
// 64-hex value minted once per session. Per-profile rewrites come from Hooks,
// compiled into a private main that calls Main.
package shim

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func parseInbound(raw string) (*inbound, error) {
	var v struct {
		SHA256 string `json:"sha256"`
		Key    string `json:"key"`
	}
	if err := jsonv2.Unmarshal([]byte(raw), &v, jsontext.AllowDuplicateNames(true)); err != nil {
		return nil, err
	}
	if v.SHA256 == "" || v.Key == "" {
		return nil, errors.New("sha256 and key are required")
	}
	return &inbound{v.SHA256, v.Key}, nil
}

// Hooks are the per-profile request rewrites; every field is optional. A hook
// replacing a header just sets it (http.Header canonicalises the name).
type Hooks struct {
	// UserAgent rewrites the outbound User-Agent from the client's.
	UserAgent func(clientUA string) string
	// RepairRequest gets the request body (a valid JSON object it must not
	// modify in place). Returning true with new bytes means those bytes are the
	// body, re-serialised compactly with key order kept; it must not reorder keys
	// itself (see Object, Array and Apply for splicing). A panic leaves the
	// request unchanged.
	RepairRequest func(body []byte) ([]byte, bool)
	// OutboundHeaders mutates the outgoing headers; session is the Claude Code
	// session id ("" when none was sent).
	OutboundHeaders func(h http.Header, session string)
	// TranslatorUserAgent is printed by the translator-user-agent subcommand: the
	// adapter's passthrough entries use it instead of the client's frozen UA.
	TranslatorUserAgent func() string
	// ResendBody is the bytes to send once more after a 400, or nil. The shim
	// does not call it yet; the Python hook contract carried it for the adapter.
	ResendBody func(request, response []byte) []byte
}

var hopByHop = map[string]bool{
	"host": true, "content-length": true, "connection": true, "transfer-encoding": true,
	"keep-alive": true, "proxy-authenticate": true, "proxy-authorization": true, "te": true,
	"trailer": true, "upgrade": true,
}

// ccmorph's own tags (any x-ccmorph-* header) end at the shim: the launcher writes
// them into ANTHROPIC_CUSTOM_HEADERS, and a session started before a tag was
// retired still carries it.
// bigBody is the request size past which the shim returns freed memory to the OS.
const bigBody = 1 << 20

const internalPrefix = "x-ccmorph-"

var (
	taskRE    = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	attemptRE = regexp.MustCompile(`^[0-9]{1,3}$`)
	outcomes  = []string{"accepted", "rejected", "escalated", "parked"}
)

type taskTags struct {
	task    *string
	attempt int
}

var noTags = taskTags{nil, 1}

// tagsOf are the whitelisted task/attempt tags from the request headers.
func tagsOf(h http.Header) taskTags {
	t := noTags
	if task := strings.TrimSpace(h.Get("x-ccmorph-task")); taskRE.MatchString(task) {
		t.task = &task
	}
	if a := strings.TrimSpace(h.Get("x-ccmorph-attempt")); attemptRE.MatchString(a) {
		if n, _ := strconv.Atoi(a); n > 0 && n < 1000 {
			t.attempt = n
		}
	}
	return t
}

const (
	eofRetryNeedle = "unexpected EOF"
	eofRetryPath   = "/v1/messages"
	eofRetries     = 1
	// Transport retry, on only with --retry-transport. The adapter sets it when
	// the shim fronts a provider directly; behind the translator, cliproxyapi's
	// own request-retry owns retries, so attempts must not multiply. The shim
	// resends the same bytes to the same target, and only before the client has
	// any bytes. A stall is not retried: it already waited the stall timeout, and
	// Claude Code retries that itself.
	transportRetries = 2
)

func retryStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504, 529:
		return true
	}
	return false
}

var errStall = errors.New("upstream stalled")

// stallError is a stall during connect or send: Python wrapped those in
// URLError, so they answer 502 (not 504) and are not retried.
var errConnectStall = errors.New("upstream connect stalled")

func (s *Server) stallMessage() string {
	return fmt.Sprintf("gateway sent nothing for %.0fs; ended by ccmorph so the session can retry", s.Stall.Seconds())
}

var idPrefix = "toolu_shim_" + randHex(4) + "_"

func randRead(b []byte) { rand.Read(b) }

func randHex(n int) string {
	b := make([]byte, n)
	randRead(b)
	return hex.EncodeToString(b)
}

// Server is one shim: the relay in front of Target.
type Server struct {
	Target         string
	Routes         *ModelRoutes
	Inbound        *inbound
	PreserveIDs    bool
	RandomDeviceID bool
	RetryTransport bool
	Hooks          Hooks
	Breaker        *Breaker
	Stall          time.Duration
	RetryBase      time.Duration
	Sleep          func(time.Duration) // injected in tests: the retry waits must not cost real time

	loopback, remote *http.Client
	devicesMu        sync.Mutex
	devices          map[string]string
}

type inbound struct{ sha256, key string }

// NewServer builds a Server with its HTTP clients.
func NewServer(target string) *Server {
	s := &Server{Target: target, Stall: 300 * time.Second, RetryBase: 500 * time.Millisecond,
		Sleep: time.Sleep, devices: map[string]string{}}
	s.setClients()
	return s
}

func (s *Server) setClients() {
	mk := func(proxy func(*http.Request) (*url.URL, error)) *http.Client {
		return &http.Client{
			CheckRedirect: checkRedirect,
			Transport: &http.Transport{
				Proxy:             proxy,
				DisableKeepAlives: true, // one connection per request, as urllib did
				DialContext:       (&net.Dialer{Timeout: s.Stall}).DialContext,
			},
		}
	}
	// Loopback target (the translator): no proxy, whatever http_proxy says (same
	// reason as --noproxy '*' in ccmorph.sh). A remote target and the catalogue
	// honour the environment.
	s.loopback = mk(nil)
	s.remote = mk(http.ProxyFromEnvironment)
}

// checkRedirect never carries the credential to another origin: Go, like urllib,
// copies request headers (x-api-key/Authorization) onto a redirect target.
// Same-origin redirects follow where urllib followed; a cross-origin one fails
// as a transport error; every other 3xx is the response.
func checkRedirect(req *http.Request, via []*http.Request) error {
	prev := via[len(via)-1]
	if !strings.EqualFold(req.URL.Scheme, prev.URL.Scheme) || !strings.EqualFold(req.URL.Host, prev.URL.Host) {
		return fmt.Errorf("cross-origin redirect refused: %s -> %s", prev.URL, req.URL)
	}
	code := req.Response.StatusCode
	if (code >= 301 && code <= 303 && (prev.Method == "GET" || prev.Method == "HEAD" || prev.Method == "POST")) ||
		((code == 307 || code == 308) && (prev.Method == "GET" || prev.Method == "HEAD")) {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return http.ErrUseLastResponse
}

// --- circuit breaker --------------------------------------------------------------

const (
	breakerFailures = 3   // requests that still failed after their retries...
	breakerWindow   = 300 // ...within this many seconds open the breaker...
	breakerOpen     = 600 // ...for this long; then the next real request probes it.
)

// Breaker is the circuit for the one provider this shim fronts (one shim per
// launch). It counts requests that still failed after their retries, never single
// attempts, so a blip cannot take a single-provider route offline. While open the
// shim answers a readable 503 without calling out. After breakerOpen the next
// real request is the probe (no synthetic call is ever made): success closes the
// breaker, failure reopens it.
type Breaker struct {
	clock    func() float64
	mu       sync.Mutex
	failures []float64
	opened   *float64
	probing  bool
}

func NewBreaker() *Breaker {
	start := time.Now()
	return &Breaker{clock: func() float64 { return time.Since(start).Seconds() + 1e6 }}
}

// wait is nil when a request may go upstream, else the seconds until one may.
func (b *Breaker) wait() *float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.opened == nil {
		return nil
	}
	left := breakerOpen - (b.clock() - *b.opened)
	if left > 0 || b.probing {
		left = math.Max(left, 1)
		return &left
	}
	b.probing = true
	return nil
}

func (b *Breaker) record(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	switch {
	case ok:
		b.failures, b.opened, b.probing = nil, nil, false
	case b.probing:
		b.opened, b.probing = &now, false
	default:
		kept := b.failures[:0]
		for _, t := range b.failures {
			if now-t < breakerWindow {
				kept = append(kept, t)
			}
		}
		b.failures = append(kept, now)
		if len(b.failures) >= breakerFailures {
			b.failures, b.opened = nil, &now
		}
	}
}

func breakerMessage(seconds float64) string {
	return fmt.Sprintf("the provider failed %d requests within %d min; ccmorph holds calls to it for %.0fs more, then tries one",
		breakerFailures, breakerWindow/60, seconds)
}

// retryDelay is full jitter: retry 1 waits up to RetryBase, retry 2 up to twice that.
func (s *Server) retryDelay(retry int) time.Duration {
	return time.Duration(mrand.Float64() * float64(s.RetryBase) * math.Pow(2, float64(retry-1)))
}

// --- request handling ---------------------------------------------------------------

type request struct {
	w       http.ResponseWriter
	r       *http.Request
	rc      *http.ResponseController
	path    string // request target without the query
	session string
	header  http.Header // the (possibly inbound-swapped) client headers
}

func (rq *request) flush() error { return rq.rc.Flush() }

// fail ends the exchange with a bodiless status the client can finish reading.
// Under HTTP/1.1 keep-alive a bodiless response with neither content-length nor
// a close hangs the client forever.
func (rq *request) fail(status int) {
	h := rq.w.Header()
	h.Set("Content-Length", "0")
	h.Set("Connection", "close")
	rq.w.WriteHeader(status)
}

// errorBody ends the exchange with an Anthropic-shaped error Claude Code can read.
func (rq *request) errorBody(status int, body []byte) {
	h := rq.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Connection", "close")
	rq.w.WriteHeader(status)
	rq.w.Write(body)
}

func apiError(kind, message string) []byte { return errorJSON(kind, message) }

// ServeHTTP relays GET, POST, PUT and DELETE; anything else is unsupported.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "POST", "PUT", "DELETE":
	default:
		http.Error(w, fmt.Sprintf("Unsupported method (%q)", r.Method), http.StatusNotImplemented)
		return
	}
	rq := &request{w: w, r: r, rc: http.NewResponseController(w), header: r.Header}
	rq.path, _, _ = strings.Cut(r.RequestURI, "?")
	s.relay(rq)
}

func bearerKey(h http.Header) string {
	if k := h.Get("x-api-key"); k != "" {
		return k
	}
	scheme, token, _ := strings.Cut(strings.TrimSpace(h.Get("Authorization")), " ")
	if strings.EqualFold(scheme, "bearer") {
		return strings.TrimSpace(token)
	}
	return ""
}

func shaMatches(got, want string) bool {
	sum := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(want)) == 1
}

// swapInbound: a sandboxed client holds only the per-launch token; check it, then
// send the real key onward so the gateway key never enters the sandbox.
func (s *Server) swapInbound(h http.Header) bool {
	if !shaMatches(bearerKey(h), s.Inbound.sha256) {
		return false
	}
	h.Del("x-api-key")
	h.Del("Authorization")
	h.Set("x-api-key", s.Inbound.key)
	h.Set("Authorization", "Bearer "+s.Inbound.key)
	return true
}

func allDigits(v string) bool {
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return v != ""
}

// readBody reads the request body whole. An ABSENT Content-Length means no body.
// A PRESENT but unparseable one is rejected, and an empty value is malformed too,
// not a synonym for absent: either way the body would desync and poison the next
// request on the reused connection.
func readBody(rq *request) ([]byte, bool) {
	if vals, present := rq.r.Header["Content-Length"]; present && !allDigits(strings.TrimSpace(vals[0])) {
		rq.errorBody(http.StatusBadRequest, []byte("invalid Content-Length"))
		return nil, false
	}
	n := rq.r.ContentLength
	if n == 0 {
		return nil, true
	}
	var buf bytes.Buffer
	if n > 0 {
		buf.Grow(int(min(n, 64<<20)))
	}
	if _, err := io.Copy(&buf, rq.r.Body); err != nil {
		return nil, false // the client died mid-body: nothing to answer
	}
	if buf.Len() == 0 {
		return nil, true
	}
	return buf.Bytes(), true
}

func (s *Server) relay(rq *request) {
	if s.Inbound != nil && !s.swapInbound(rq.header) {
		rq.fail(401)
		return
	}
	body, ok := readBody(rq)
	if !ok {
		return
	}
	if len(body) >= bigBody {
		// Multi-MB bodies leave the heap several times their size after the GC
		// runs; a session-long shim must hand that back, not sit on it.
		defer debug.FreeOSMemory()
	}
	var answerAs string
	var requested *string
	rq.session = rq.header.Get("x-claude-code-session-id")
	isPost := rq.r.Method == "POST"
	isMessagePost := isPost && rq.path == pathMessages
	isModelPost := isMessagePost
	if !isModelPost && isPost && rq.path == pathCount && s.Routes != nil && len(s.Routes.modelMap) > 0 {
		if m, ok := requestedModel(body); ok {
			_, isModelPost = s.Routes.mappingTarget(m)
		}
	}
	var sidecar *UsageSidecar
	if len(body) > 0 && s.Routes != nil && isModelPost {
		key := bearerKey(rq.header)
		if !shaMatches(key, s.Routes.keySHA256) {
			rq.fail(401)
			return
		}
		if isMessagePost {
			sidecar = s.Routes.usage
		}
		// The client's choice, before any explicit map or family resolution.
		if sidecar != nil || s.Routes.command == nil {
			if m, ok := requestedModel(body); ok {
				requested = &m
			}
		}
		if sidecar != nil {
			sidecar = sidecar.tagged(tagsOf(rq.header))
		}
		body, answerAs = s.resolve(body, key)
		if sidecar != nil {
			if m, ok := requestedModel(body); ok {
				sidecar.served = &m
			}
		}
		if msg := s.Routes.ensure(body, key, requested, rq.path); msg != "" {
			rq.errorBody(400, errorJSON("invalid_request_error", msg))
			return
		}
	}
	// Request hooks (if any) read every POST body, whatever the path; the
	// device-id swap only the two metadata endpoints.
	swap := s.RandomDeviceID && isPost && (rq.path == pathMessages || rq.path == pathCount)
	if len(body) > 0 && (swap || s.Hooks.RepairRequest != nil || s.Hooks.OutboundHeaders != nil) {
		body = s.cleanRequest(body, swap, &rq.session)
	}
	retrying := s.RetryTransport && isMessagePost
	limit := eofRetries
	if retrying {
		limit = transportRetries
	}
	retries := 0
	var breaker *Breaker
	if retrying {
		breaker = s.Breaker
	}
	if breaker != nil {
		// The open breaker's answer is the whole exchange: no call goes out.
		if held := breaker.wait(); held != nil {
			rq.errorBody(503, apiError("api_error", breakerMessage(*held)))
			return
		}
	}
	var up *upstream
	var data []byte
	var isStream bool
	for {
		var err error
		up, err = s.open(rq, body)
		if err != nil {
			if !errors.Is(err, errStall) && retrying && retries < limit && !errors.Is(err, errConnectStall) {
				retries++
				s.Sleep(s.retryDelay(retries))
				continue
			}
			if breaker != nil {
				breaker.record(false)
			}
			if errors.Is(err, errStall) {
				rq.errorBody(504, apiError("api_error", s.stallMessage()))
			} else {
				rq.fail(502)
			}
			return
		}
		isStream = strings.Contains(up.resp.Header.Get("Content-Type"), "text/event-stream")
		// Read only non-SSE bodies before sending downstream headers. That makes
		// the exact EOF 500 safely replayable; healthy SSE streams keep their
		// original low-latency line-by-line path.
		if !isStream {
			data, err = io.ReadAll(up)
			if err != nil {
				up.close()
				if breaker != nil {
					breaker.record(false)
				}
				if errors.Is(err, errStall) {
					rq.errorBody(504, apiError("api_error", s.stallMessage()))
				} else {
					rq.fail(502)
				}
				return
			}
			var failed bool
			if retrying {
				failed = retryStatus(up.resp.StatusCode)
			} else {
				failed = rq.path != "" && strings.HasSuffix(rq.path, eofRetryPath) &&
					up.resp.StatusCode == 500 && bytes.Contains(data, []byte(eofRetryNeedle))
			}
			if failed && retries < limit {
				up.close()
				retries++
				if retrying {
					s.Sleep(s.retryDelay(retries))
				}
				continue
			}
			break
		}
		data = nil
		if retrying && retryStatus(up.resp.StatusCode) && retries < limit {
			// A failing status that declares itself a stream, with nothing sent
			// downstream yet: only a delivered stream's bytes are unreplayable.
			up.close()
			retries++
			s.Sleep(s.retryDelay(retries))
			continue
		}
		break
	}
	if breaker != nil {
		breaker.record(!retryStatus(up.resp.StatusCode))
	}
	if sidecar != nil {
		sidecar.retries = retries
	}
	defer up.close()
	h := rq.w.Header()
	for k, vs := range up.resp.Header {
		if !hopByHop[strings.ToLower(k)] {
			for _, v := range vs {
				h.Add(k, v)
			}
		}
	}
	if isStream {
		h.Set("Connection", "close") // EOF is the client's only completion signal
		rq.w.WriteHeader(up.resp.StatusCode)
		rq.flush()
		s.streamSSE(rq, up, sidecar, requested, answerAs)
		return
	}
	if strings.Contains(up.resp.Header.Get("Content-Type"), "json") {
		var out []byte
		if up.resp.StatusCode >= 400 {
			out = unwrapGatewayError(data)
		}
		if out == nil {
			out, _ = s.rewrite(data, answerAs, true)
		}
		data = out
	}
	h.Set("Content-Length", strconv.Itoa(len(data)))
	rq.w.WriteHeader(up.resp.StatusCode)
	rq.w.Write(data)
	// After the client has its bytes: the response is the product, the row is
	// bookkeeping.
	if sidecar != nil {
		sidecar.response(data, requested)
	}
}

// streamSSE relays the stream. Every exit closes the client connection (the
// header said so): an SSE response carries no content-length.
func (s *Server) streamSSE(rq *request, up *upstream, sidecar *UsageSidecar, requested *string, answerAs string) {
	readErr, _ := s.relayStream(rq, &thinkingStream{r: bufio.NewReaderSize(up, 64<<10)}, sidecar, requested, answerAs)
	// A client that hung up mid-stream (cancelled turn, /clear, quit) is routine.
	if errors.Is(readErr, errStall) {
		// The upstream went quiet mid-reply. Say so as a stream error event, the
		// same shape an overloaded Anthropic stream ends with.
		event := errorJSON("api_error", s.stallMessage())
		rq.w.Write(append(append([]byte("event: error\ndata: "), event...), '\n', '\n'))
		rq.flush()
	}
}

// --- upstream ----------------------------------------------------------------------

// upstream is one open upstream response whose body reads are each bounded by the
// stall timeout.
type upstream struct {
	resp   *http.Response
	timer  *time.Timer
	ctx    context.Context
	cancel context.CancelCauseFunc
	stall  time.Duration
}

func (u *upstream) Read(p []byte) (int, error) {
	u.timer.Reset(u.stall)
	n, err := u.resp.Body.Read(p)
	u.timer.Stop()
	if err != nil && errors.Is(context.Cause(u.ctx), errStall) {
		err = errStall
	}
	return n, err
}

func (u *upstream) close() {
	u.timer.Stop()
	u.resp.Body.Close()
	u.cancel(nil)
}

// headers for the upstream request: the client's, minus hop-by-hop and ccmorph's
// own tags, with the hooks applied.
func (s *Server) outboundHeaders(rq *request) http.Header {
	out := http.Header{}
	for k, vs := range rq.header {
		lk := strings.ToLower(k)
		if hopByHop[lk] || lk == "accept-encoding" || strings.HasPrefix(lk, internalPrefix) {
			continue
		}
		out[k] = []string{vs[len(vs)-1]}
	}
	if s.Hooks.UserAgent != nil {
		out.Set("User-Agent", s.Hooks.UserAgent(rq.header.Get("User-Agent")))
	}
	if s.Hooks.OutboundHeaders != nil {
		s.Hooks.OutboundHeaders(out, rq.session)
	}
	// Force identity: a gzipped response fails to parse and would be passed
	// through UNREWRITTEN and silently: the shim's whole job skipped because the
	// client happened to offer compression. Loopback hop, so it costs nothing.
	out.Set("Accept-Encoding", "identity")
	return out
}

// open sends one upstream request. HTTP error statuses are responses; transport
// errors come back as errors so the caller can return one finite 502 (or 504 for
// a stall once the request was written).
func (s *Server) open(rq *request, body []byte) (*upstream, error) {
	ctx, cancel := context.WithCancelCause(context.Background())
	timer := time.AfterFunc(s.Stall, func() { cancel(errStall) })
	var wrote atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteRequest:         func(httptrace.WroteRequestInfo) { wrote.Store(true); timer.Reset(s.Stall) },
		GotFirstResponseByte: func() { timer.Reset(s.Stall) },
	})
	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, rq.r.Method, s.Target+rq.r.RequestURI, reader)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	req.Header = s.outboundHeaders(rq)
	if _, ok := req.Header["User-Agent"]; !ok {
		req.Header["User-Agent"] = []string{""} // no Go default: send what the client sent
	}
	client := s.remote
	if u, err := url.Parse(s.Target); err == nil {
		switch u.Hostname() {
		case "127.0.0.1", "localhost", "::1":
			client = s.loopback
		}
	}
	resp, err := client.Do(req)
	if err != nil {
		timer.Stop()
		stalled := errors.Is(context.Cause(ctx), errStall)
		cancel(nil)
		var ne net.Error
		switch {
		case stalled && wrote.Load():
			return nil, errStall
		case stalled || (errors.As(err, &ne) && ne.Timeout()):
			return nil, fmt.Errorf("%w: %v", errConnectStall, err)
		}
		return nil, err
	}
	timer.Stop()
	up := &upstream{resp: resp, timer: timer, ctx: ctx, cancel: cancel, stall: s.Stall}
	return up, nil
}

// resolve is (body to send, Anthropic id to answer as). It never fails.
func (s *Server) resolve(body []byte, key string) ([]byte, string) {
	d := parseDoc(body)
	if d == nil {
		return body, ""
	}
	asked, ok := d.str("model")
	if !ok {
		return body, ""
	}
	base := strings.TrimSuffix(asked, "[1m]")
	// One lookup, on the id this request will be routed by: the window marker is
	// stripped from it, the shell strips it before it hands a map its model and
	// refuses a key that carries it, so a second lookup on the exact spelling
	// could only pick a target nothing else resolved.
	if mapped, ok := s.Routes.mappingTarget(asked); ok {
		// Explicit maps are final: do not substitute even a newer Claude target.
		return withModel(d, strings.TrimSuffix(mapped, "[1m]")), base
	}
	if _, _, ok := claudeVersion(base); !ok {
		return body, ""
	}
	served := s.Routes.catalogue.served(key, base)
	chosen := resolveModel(base, served)
	if chosen == base {
		if len(served) > 0 && !served[base] {
			fmt.Fprintf(os.Stderr, "gateway-shim: %s is not served and no same-family model of "+
				"that version or newer is; sending it unchanged\n", base)
		}
		return body, base
	}
	fmt.Fprintf(os.Stderr, "gateway-shim: %s is not served; sending newest same-family %s\n", base, chosen)
	return withModel(d, chosen+asked[len(base):]), chosen
}
