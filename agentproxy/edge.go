package agentproxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akleizor/ccmorph/util/paths"
)

// Edge is the Remote Control edge's side of the proxy: the same proxy as a
// session's agent proxy, run once by `ccmorph-rc <route> --edge` for every
// phone session. A Remote Control session signs in to claude.ai and so talks
// to api.anthropic.com; the edge moves only its inference (/v1/messages) to
// the word that holds for it, and lightens the event uploads the phone
// downloads. Everything it reads is a file `ccmorph-rc` writes beside the
// switch file (Inference):
//
//	inference            the word every host's turns follow: "account", "route"
//	                     or a profile name, read on every turn
//	sessions/<cse id>    one session's own word (--switch --session)
//	account-tiers.json   the account's {alias: model}, for its own turns
//	switch-<profile>.json  a handoff: a profile's endpoint and key, taken within
//	                     a second by the proxy of the edge it names only
//	held.<edge pid>.json   what this proxy holds ({profile: handoff id}, no key)
//	agents/              the router's picks (picks.go)
//
// A request is decided in this process before any connection is made, so a
// word the proxy cannot serve, a body it cannot read or a decision that
// panics ends as a 502: nothing reaches Anthropic on the account's credential
// unless the account word says so.
type Edge struct {
	// Upstream and Key are the route: where the word "route" (and every word
	// this proxy holds no endpoint for) goes. Tiers is the route's own
	// {alias: model} (the session's env pins them).
	Upstream *url.URL
	Key      string
	Tiers    map[string]string
	// Inference is the switch file; everything else is read beside it. "" runs
	// the route only.
	Inference string
	// Parent is the edge's pid (the proxy exits with it) and ParentStarted that
	// process's start, the pair a handoff must name for this proxy to take it.
	Parent        int
	ParentStarted string
	// Log, when set, gets one line per request: method and path only.
	Log io.Writer

	testHook func(stage string) // a test seam: called between a take's steps
	logMu    sync.Mutex
	mu       sync.Mutex // switched and held
	takeMu   sync.Mutex // one take at a time, so held.<pid>.json never loses one
	ownTag   string
	handoff  map[string]endpoint
	held     map[string]any
}

// endpoint is a profile `ccmorph-rc <profile> --switch` brought up beside the edge.
type endpoint struct {
	upstream, key string
	tiers         map[string]string
}

// NewEdge is an edge that holds nothing yet.
func NewEdge(e *Edge) *Edge {
	e.handoff = map[string]endpoint{}
	e.held = map[string]any{}
	if started, ok := procStarted(os.Getpid()); ok {
		e.ownTag = startTag(started)
	} else {
		e.ownTag = startTag("")
	}
	return e
}

func (e *Edge) bridge() string { return filepath.Dir(e.Inference) }

// AgentsDir is where the router leaves picks for this edge's subagents.
func (e *Edge) AgentsDir() string {
	if e.Inference == "" {
		return ""
	}
	return filepath.Join(e.bridge(), "agents")
}

func (e *Edge) sessionsDir() string {
	if e.Inference == "" {
		return ""
	}
	return filepath.Join(e.bridge(), "sessions")
}

func (e *Edge) logf(format string, args ...any) {
	if e.Log == nil {
		return
	}
	e.logMu.Lock()
	defer e.logMu.Unlock()
	fmt.Fprintf(e.Log, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

// accountTiers is the account profile's {alias: model}, written beside the
// switch by --switch; none when absent.
func (e *Edge) accountTiers() map[string]string {
	if e.Inference == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(e.bridge(), "account-tiers.json"))
	if err != nil {
		return nil
	}
	return stringMap(b)
}

func stringMap(b []byte) map[string]string {
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// --- handoffs ----------------------------------------------------------------

func (e *Edge) heldPath() string {
	return filepath.Join(e.bridge(), "held."+strconv.Itoa(e.Parent)+".json")
}

// PublishHeld says which handoffs this proxy holds (no key): held.<parent
// pid>.json, replaced whole. --switch waits for its handoff's id here, and
// rc-hosts reads it to see a proxy that restarted and holds nothing.
func (e *Edge) PublishHeld() error {
	if e.Inference == "" || e.Parent == 0 {
		return nil
	}
	e.mu.Lock()
	profiles := make(map[string]any, len(e.held))
	for k, v := range e.held {
		profiles[k] = v
	}
	e.mu.Unlock()
	var started any
	if e.ParentStarted != "" {
		started = e.ParentStarted
	}
	b, _ := json.Marshal(map[string]any{"edge": e.Parent, "edge_started": started, "profiles": profiles})
	path := e.heldPath()
	err := paths.AtomicWrite(paths.WriteIn{Path: path, Temp: filepath.Base(path) + ".*.tmp", Data: b, Mode: 0o600})
	return err
}

// mine is whether handoff h is for this proxy's own edge: that pid and that start.
func (e *Edge) mine(h map[string]any) bool {
	if e.Parent == 0 {
		return true
	}
	if v, ok := h["edge"]; ok {
		if f, isNum := v.(float64); !isNum || int(f) != e.Parent {
			return false
		}
	}
	if v, ok := h["edge_started"]; ok {
		s, isStr := v.(string)
		if (!isStr && v != nil) || s != e.ParentStarted {
			return false
		}
	}
	return true
}

// handoffEndpoint is the endpoint a parsed handoff carries. Without upstream
// and key there is nothing to route to: such a file holds a credential for
// nothing, and the take removes it without recording.
func handoffEndpoint(h map[string]any) (endpoint, bool) {
	up, ok1 := h["upstream"].(string)
	key, ok2 := h["key"].(string)
	if !ok1 || !ok2 {
		return endpoint{}, false
	}
	tiers := map[string]string{}
	if raw, ok := h["tiers"].(map[string]any); ok {
		for k, v := range raw {
			if s, ok := v.(string); ok {
				tiers[k] = s
			}
		}
	}
	return endpoint{up, key, tiers}, true
}

func parseHandoff(b []byte) map[string]any {
	var h map[string]any
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return h
}

// endpoint is the endpoint a profile was brought up with, taking a handoff
// waiting for it first: a new handoff replaces the one held (the profile was
// brought up again).
func (e *Edge) endpoint(profile string) (endpoint, bool) {
	if e.Inference != "" && switchWord.MatchString(profile) {
		e.Take(profile)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	ep, ok := e.handoff[profile]
	return ep, ok
}

// Take takes profile's handoff if one waits for this proxy: the file is
// renamed to taken.<pid>.<start tag>.<profile>.json, and what this proxy
// records, holds and removes is the one publication it moved (a publication
// replacing the handoff atomically between the look and the move is the one
// taken, and decides). The record lands before held.<pid>.json names the
// handoff, which is the "taken" --switch waits for, so a profile is never
// acknowledged before it is served. A handoff for another edge is never
// touched; one that routes nothing (does not parse, or lacks upstream or key)
// is removed with no record, whichever edge it claims, so its key is not
// re-polled every half second; one moved and found to be another edge's is
// put back unless a newer stands there.
func (e *Edge) Take(profile string) {
	if e.Inference == "" {
		return
	}
	hand := filepath.Join(e.bridge(), "switch-"+profile+".json")
	taken := filepath.Join(e.bridge(), fmt.Sprintf("taken.%d.%s.%s.json", os.Getpid(), e.ownTag, profile))
	e.takeMu.Lock()
	defer e.takeMu.Unlock()
	b, err := os.ReadFile(hand)
	if err != nil {
		return
	}
	h := parseHandoff(b)
	if _, ok := handoffEndpoint(h); ok && !e.mine(h) {
		return // another edge's proxy is the one --switch waits for
	}
	e.hook("peeked")
	if os.Rename(hand, taken) != nil {
		return // withdrawn, or taken by another proxy: nothing here changed
	}
	defer os.Remove(taken) // the key lives in memory only from here
	e.hook("moved")
	mb, err := os.ReadFile(taken)
	if err != nil {
		return
	}
	moved := parseHandoff(mb)
	ep, ok := handoffEndpoint(moved)
	if !ok {
		return // routes nothing: removed, unrecorded
	}
	if !e.mine(moved) {
		os.Link(taken, hand) // another edge's, moved by a race: back, unless a newer stands
		return
	}
	e.mu.Lock()
	e.handoff[profile] = ep
	e.held[profile] = moved["id"]
	e.mu.Unlock()
	e.PublishHeld() // an unwritten word never loses the take
}

// takeAll takes every handoff waiting, within a second of its writing, so the
// key never waits on disk for a request (one session's profile may be idle for
// days); --switch waits for this and removes a handoff no proxy took.
func (e *Edge) takeAll() {
	files, _ := filepath.Glob(filepath.Join(globQuote(e.bridge()), "switch-*.json"))
	for _, f := range files {
		name := filepath.Base(f)
		e.Take(name[len("switch-") : len(name)-len(".json")])
	}
}

// ownedHeld is whether the held record at path is this proxy's: it names this edge's start.
func (e *Edge) ownedHeld(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var h map[string]any
	if json.Unmarshal(b, &h) != nil {
		return false
	}
	v := h["edge_started"]
	s, isStr := v.(string)
	if v != nil && !isStr {
		return false
	}
	return s == e.ParentStarted
}

// ForgetHeld removes what this proxy holds when it goes, but only its own
// record. held.<pid>.json is named by the edge's pid, which the OS may have
// given to a replacement edge whose proxy writes its own record there at any
// moment. So the record is moved aside first and checked where it lies: what
// is checked is what is removed, and a record that is not ours goes back,
// unless a newer one stands there already.
func (e *Edge) ForgetHeld() {
	if e.Inference == "" || e.Parent == 0 {
		return
	}
	held := e.heldPath()
	if !e.ownedHeld(held) {
		return
	}
	e.hook("forget-checked")
	aside := fmt.Sprintf("%s.%d.gone", held, os.Getpid())
	if os.Rename(held, aside) != nil {
		return
	}
	if !e.ownedHeld(aside) {
		os.Link(aside, held)
	}
	os.Remove(aside)
}

// Run publishes what this proxy holds (nothing yet, whatever one before it
// held), then takes handoffs every half second and follows its edge: when
// Parent no longer runs it calls stop. It returns when done is closed.
func (e *Edge) Run(done <-chan struct{}, stop func()) {
	e.PublishHeld()
	if e.Inference != "" {
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					e.takeAll()
				}
			}
		}()
	}
	if e.Parent > 0 {
		go func() {
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if err := syscall.Kill(e.Parent, 0); errors.Is(err, syscall.ESRCH) {
						stop()
						return
					}
				}
			}
		}()
	}
}

// --- deciding a request ------------------------------------------------------

// edgeState is what the edge decided about one request, kept for its reply.
type edgeState struct {
	asked, via string // the word the pick set; what routed it (the route after a fallback)
	account    bool   // the account word: a first-party call left on the session's login
	gzipped    bool   // the body was sent compressed and goes on uncompressed
}

// decide is the edge's whole decision about one request, made before any
// connection is: where it goes, whose credential it carries, and what its body
// is. An error ends the request as a 502.
func (e *Edge) decide(r *http.Request, u *url.URL, d *decision) error {
	es := &edgeState{}
	d.edge = es
	first := isFirst(u)
	if !first || !isInference(u.Path) {
		if !first {
			return nil
		}
		e.logf("anthropic %s %s%s", r.Method, u.Hostname(), u.Path)
		if eventsPath.MatchString(u.Path) {
			return e.slimUpload(r, d)
		}
		return nil
	}
	agents := e.AgentsDir()
	if agent := r.Header.Get(AgentHeader); agent != "" && agents != "" {
		if pick, ok := Bound(agents, agent); ok {
			return e.route(r, u, d, pick.Word, " (agent)", pick.Model)
		}
		if session := r.Header.Get(SessionHeader); HasPending(agents, session) {
			// Its prompt, in the body, may name a pick.
			body, err := readAll(r)
			if err != nil {
				return err
			}
			d.body = body
			if pick, ok := Claim(agents, session, agent, body, time.Now()); ok {
				return e.route(r, u, d, pick.Word, " (agent)", pick.Model)
			}
		}
	}
	return e.route(r, u, d, "", "", "")
}

func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return []byte{}, nil
	}
	b, err := io.ReadAll(r.Body)
	r.Body.Close()
	if b == nil {
		b = []byte{}
	}
	return b, err
}

// body is the request's body, read once.
func body(r *http.Request, d *decision) ([]byte, error) {
	if d.body != nil {
		return d.body, nil
	}
	b, err := readAll(r)
	d.body = b
	return b, err
}

// route sends an inference request where its word says: the agent's pick when
// given (word), else its session's own word, else the switch every host shares.
func (e *Edge) route(r *http.Request, u *url.URL, d *decision, word, via, model string) error {
	es := d.edge
	target := word
	if target == "" {
		if own := sessionTarget(e.sessionsDir(), r.Header.Get(SessionHeader)); own != "" {
			target, via = own, " (session)"
		} else {
			target = inferenceTarget(e.Inference)
		}
	}
	if target == "account" {
		// A legitimate first-party call: left untouched on the session's own
		// login. Only its model may change: a tier the route pinned (fable ->
		// the route's model) is read back to the account's own model for that
		// alias.
		es.asked, es.via, es.account = "account", "account", true
		e.logf("account   %s %s%s", r.Method, u.Path, via)
		rule := modelRule{tiers: e.accountTiers(), routeTiers: e.Tiers}
		if len(rule.tiers) == 0 && model == "" {
			return nil
		}
		return e.rewrite(r, d, func(p *obj) bool { c := rule.setModel(p); return setPick(p, model) || c })
	}
	es.asked = target
	up, key, tiers, isRoute := e.Upstream, e.Key, e.Tiers, true
	if target != "route" {
		if ep, ok := e.endpoint(target); ok {
			base, err := url.Parse(ep.upstream)
			if err != nil {
				return err
			}
			up, key, tiers, isRoute = base, ep.key, ep.tiers, false
		} else {
			// Which repair is true depends on the state dir, so lostEndpointNote
			// decides what this line may name (a session's own word is repaired
			// with --session).
			if e.Log != nil && e.Inference != "" {
				session := ""
				if via == " (session)" {
					session = r.Header.Get(SessionHeader)
				}
				e.logf("%s", lostEndpointNote(target, e.bridge(), e.Parent, session, e.ParentStarted))
			}
			target = "route"
		}
	}
	if up == nil || (up.Scheme != "http" && up.Scheme != "https") || up.Hostname() == "" {
		return fmt.Errorf("route upstream is not an http(s) URL: %v", up)
	}
	rule := modelRule{tiers: tiers, routeTiers: e.Tiers, isRoute: isRoute}
	if err := e.rewrite(r, d, func(p *obj) bool { c := normalizeBody(p, rule); return setPick(p, model) || c }); err != nil {
		return err
	}
	d.url = join(up, &url.URL{Path: u.Path, RawQuery: u.RawQuery})
	d.target = &Target{Base: up, Key: key, Tiers: tiers}
	es.via = target
	note := ""
	if target != "route" {
		note = " via " + target
	}
	e.logf("route     %s %s%s%s", r.Method, u.Path, note, via)
	return nil
}

// setPick names the model outright when an agent's pick does.
func setPick(p *obj, model string) bool {
	if model == "" {
		return false
	}
	if cur, _ := p.str("model"); cur == model {
		return false
	}
	p.set("model", model)
	return true
}

// rewrite reads the body, lets change edit it in place, and keeps the result
// as the body to send. A body that is not a JSON object goes on as it was.
func (e *Edge) rewrite(r *http.Request, d *decision, change func(*obj) bool) error {
	content, err := body(r, d)
	if err != nil || len(content) == 0 {
		return err
	}
	v, perr := parseJSON(content)
	if p, ok := v.(*obj); perr == nil && ok {
		d.body = reencode(content, p, change(p))
	}
	return nil
}

// slimUpload lightens an events upload for the phone.
func (e *Edge) slimUpload(r *http.Request, d *decision) error {
	content, err := body(r, d)
	if err != nil || len(content) == 0 {
		return err
	}
	raw, compressed := content, false
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(content))
		if err != nil {
			return nil
		}
		if raw, err = io.ReadAll(zr); err != nil {
			return nil
		}
		compressed = true
	}
	v, err := parseJSON(raw)
	if err != nil || !slimEvents(v, sipsShrink) {
		return nil
	}
	slim, err := encodeJSON(v)
	if err != nil {
		return nil
	}
	e.logf("events    %d -> %d bytes", len(raw), len(slim))
	d.body = slim
	d.edge.gzipped = compressed
	return nil
}

// --- the reply -----------------------------------------------------------------

// reached is whether the connection that answered is the endpoint the request
// was sent to: the same host and the same port. A literal (a loopback adapter,
// an IP upstream) is compared by its address; a name is compared by port, the
// resolved address being all a connection knows of it. An unreadable connection
// cannot prove a mismatch and leaves the destination as the evidence.
func reached(host string, port int, answered net.Addr) bool {
	a, ok := answered.(*net.TCPAddr)
	if !ok || a == nil {
		return true
	}
	if a.Port != port {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.Equal(a.IP)
	}
	return true
}

// verdict is the log line for one turn's reply: requested-vs-served, so the
// profile the pick set must be the one that replies, and a turn answered by
// anything else says so, never a quiet success. Anthropic stamps its
// organization on every response; that is the proof of which upstream
// answered. A routed call must not carry it; an account call must. A profile
// the proxy holds no endpoint for falls back to the edge's route, and the reply
// is then the route's, not the pick's: a miss, with the fallback's own warning
// above it.
func verdict(es *edgeState, status int, path string, firstParty bool, host string, port int, answered net.Addr) string {
	served := net.JoinHostPort(host, strconv.Itoa(port))
	if a, ok := answered.(*net.TCPAddr); ok && a != nil && net.ParseIP(host) != nil {
		served = a.String()
	}
	var ok bool
	var extra, where string
	if es.account {
		ok = firstParty
		where = "ANTHROPIC"
		if !firstParty {
			where = served + " (no Anthropic signature)"
		}
	} else {
		ok = es.via == es.asked && !firstParty && reached(host, port, answered)
		if es.via != es.asked {
			extra = " via=" + es.via
		}
		where = served
		if firstParty {
			where = "ANTHROPIC (not routed!)"
		}
	}
	kind := "route     "
	if !ok {
		kind = "miss      "
	} else if es.account {
		kind = "account   "
	}
	return fmt.Sprintf("%s<- %d %s asked=%s%s served-by=%s", kind, status, path, es.asked, extra, where)
}

func hostPort(u *url.URL) (string, int) {
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if p := u.Port(); p != "" {
		port, _ = strconv.Atoi(p)
	}
	return u.Hostname(), port
}

// isFirst is whether u addresses Anthropic's own API: api.anthropic.com, or
// wherever a decrypted request to it is sent (a test points that elsewhere).
func isFirst(u *url.URL) bool {
	if isFirstPartyHost(u.Hostname()) {
		return true
	}
	h, p := hostPort(u)
	fh, fp := hostPort(firstParty)
	return strings.EqualFold(h, fh) && p == fp
}

// refuseFirstParty is the fail-closed rule: an inference request reaches
// Anthropic itself only when the account word says so.
func refuseFirstParty(u *url.URL, d *decision) error {
	if d.edge == nil || d.edge.account || !isInference(u.Path) {
		return nil
	}
	if isFirst(u) {
		return errors.New("an inference request addressed to Anthropic without the account word is refused")
	}
	return nil
}

func (e *Edge) hook(stage string) {
	if e.testHook != nil {
		e.testHook(stage)
	}
}
