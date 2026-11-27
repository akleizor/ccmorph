package agentproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/akleizor/ccmorph/ordjson"
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
//	served.<edge pid>.json this edge run's asked-vs-served outcome (edgeserved.go)
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
	// heldMu makes each publication of held.<pid>.json one step, its snapshot
	// and its write together, so an older snapshot never lands over a newer.
	// stopped (set under takeMu and heldMu both) ends takes and publications
	// for good: what Stop leaves is final.
	heldMu  sync.Mutex
	stopped bool
	pollers sync.WaitGroup
	ownTag  string
	handoff map[string]endpoint
	held    map[string]any
	pins    loopPins      // each conversation's route for the loop it is in (edgeloop.go)
	served  servedOutcome // the asked-vs-served count of this edge run (edgeserved.go)
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
// rc-hosts reads it to see a proxy that restarted and holds nothing. The
// proxy publishes once before it listens (EdgeMain), and after every take.
func (e *Edge) PublishHeld() error {
	if e.Inference == "" || e.Parent == 0 {
		return nil
	}
	e.heldMu.Lock()
	defer e.heldMu.Unlock()
	if e.stopped {
		return nil
	}
	e.mu.Lock()
	profiles := make(map[string]any, len(e.held))
	for k, v := range e.held {
		profiles[k] = v
	}
	e.mu.Unlock()
	e.hook("held-snapshot")
	var started any
	if e.ParentStarted != "" {
		started = e.ParentStarted
	}
	b, _ := json.Marshal(map[string]any{"edge": e.Parent, "edge_started": started, "profiles": profiles})
	path := e.heldPath()
	return paths.AtomicWrite(paths.WriteIn{Path: path, Temp: filepath.Base(path) + ".*.tmp", Data: b, Mode: 0o600})
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
	if e.stopped {
		return // the proxy is going: a handoff left waiting is --switch's to remove
	}
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
	b, err := paths.ReadRegularFile(path) // nonblocking: a FIFO under the name is not ours, and never waited on
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
// record, and with it the outcome it kept (served.<pid>.json, the same rule).
// held.<pid>.json is named by the edge's pid, which the OS may have
// given to a replacement edge whose proxy writes its own record there at any
// moment. So the record is moved aside first and checked where it lies: what
// is checked is what is removed, and a record that is not ours goes back,
// unless a newer one stands there already.
func (e *Edge) ForgetHeld() {
	if e.Inference == "" || e.Parent == 0 {
		return
	}
	e.forgetOwned(e.heldPath(), "forget-checked")
	e.forgetOwned(e.servedPath(), "")
}

func (e *Edge) forgetOwned(path, stage string) {
	if !e.ownedHeld(path) {
		return
	}
	if stage != "" {
		e.hook(stage)
	}
	aside := fmt.Sprintf("%s.%d.gone", path, os.Getpid())
	if os.Rename(path, aside) != nil {
		return
	}
	if !e.ownedHeld(aside) {
		os.Link(aside, path)
	}
	os.Remove(aside)
}

// Stop ends this proxy's handoff work for good, before its last cleanup: it
// waits for a take in progress to finish (its taken.* file, which holds a key,
// is gone when it does) and for the pollers Run started (close Run's done
// first), and from then on nothing takes a handoff or writes held.<pid>.json,
// so ForgetHeld after it is the last word.
func (e *Edge) Stop() {
	e.takeMu.Lock()
	e.heldMu.Lock()
	e.stopped = true
	e.heldMu.Unlock()
	e.takeMu.Unlock()
	e.pollers.Wait()
}

// Run takes handoffs every half second and follows its edge: when Parent no
// longer runs it calls stop. Its pollers end when done is closed (Stop waits
// for them). What the proxy holds is published before it listens (EdgeMain).
func (e *Edge) Run(done <-chan struct{}, stop func()) {
	if e.Inference != "" {
		e.pollers.Add(1)
		go func() {
			defer e.pollers.Done()
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
		e.pollers.Add(1)
		go func() {
			defer e.pollers.Done()
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
	inference  bool   // the request as it came was a first-party inference turn
	// The body as its sender meant it, decoded once (coding.go), and the coding
	// it came in, so a changed body goes on encoded the same way.
	plain     []byte
	coding    coding
	decoded   bool
	undecoded error // why the body could not be decoded, once that is known
}

// The most of a body the edge reads, on the wire and decoded. The inference
// cap is far above Anthropic's own request limit; an events upload past its
// cap is forwarded untouched, unslimmed. Variables so a test can lower them.
var (
	inferenceMax int64 = 256 << 20
	eventsMax    int64 = 64 << 20
)

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
	// Whatever is decided below, this is an inference turn: the transport's last
	// check refuses it Anthropic unless the account word was chosen, wherever
	// the path it is sent on leads.
	es.inference = true
	agents := e.AgentsDir()
	if agent := r.Header.Get(AgentHeader); agent != "" && agents != "" {
		if pick, ok := Bound(agents, agent); ok {
			return e.route(r, u, d, pick.Word, " (agent)", pick.Model)
		}
		if session := r.Header.Get(SessionHeader); HasPending(agents, session) {
			// Its prompt, in the body, may name a pick. A body the edge cannot
			// read may hold one, so it goes nowhere.
			plain, err := e.plainBody(r, d, inferenceMax)
			if err != nil {
				return fmt.Errorf("a pick waits for this session and the request body cannot be read: %w", err)
			}
			if pick, ok := Claim(agents, session, agent, plain, time.Now()); ok {
				return e.route(r, u, d, pick.Word, " (agent)", pick.Model)
			}
		}
	}
	return e.route(r, u, d, "", "", "")
}

// errUnread is a body that could not be read off the wire: the request ends.
type errUnread struct{ error }

// plainBody is the body decoded, once, read only up to limit on the wire and
// decoded alike. A longer body is left to go on as it came; an error from the
// wire is an errUnread.
func (e *Edge) plainBody(r *http.Request, d *decision, limit int64) ([]byte, error) {
	es := d.edge
	if es.decoded || es.undecoded != nil {
		return es.plain, es.undecoded
	}
	wire := d.body
	if wire == nil {
		var whole bool
		var err error
		if wire, whole, err = readUpTo(r, limit); err != nil {
			return nil, errUnread{err}
		} else if !whole {
			es.undecoded = errTooLarge
			return nil, errTooLarge
		}
		d.body = wire
	}
	plain, c, err := decodeBody(wire, r.Header, limit)
	if err != nil {
		es.undecoded = err
		return nil, err
	}
	es.plain, es.coding, es.decoded = plain, c, true
	return plain, nil
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
		target, via = e.pinned(r, d, target, via)
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
		return e.rewrite(r, d, func(p *ordjson.Obj) bool { c := rule.setModel(p); return setPick(p, model) || c })
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
	if err := e.rewrite(r, d, func(p *ordjson.Obj) bool { c := normalizeBody(p, rule); return setPick(p, model) || c }); err != nil {
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

// pinned is where this request goes once its conversation's loop is honoured:
// the word as read now (target, via) unless the request continues a tool loop
// that began on another route, which stays there. A new turn takes the word
// held now and pins it. The body is read only when the pin and the word differ.
func (e *Edge) pinned(r *http.Request, d *decision, target, via string) (string, string) {
	session := r.Header.Get(SessionHeader)
	if session == "" {
		return target, via
	}
	key, now := pinKey(session, r.Header.Get(AgentHeader)), time.Now()
	if p, ok := e.pins.get(key, now); ok {
		if p.target == target {
			e.pins.set(key, target, via, now)
			return target, via
		}
		if plain, err := e.plainBody(r, d, inferenceMax); err == nil {
			if v, perr := ordjson.Parse(plain); perr == nil {
				if o, ok := v.(*ordjson.Obj); ok && midLoop(o) {
					e.pins.set(key, p.target, p.via, now)
					e.logf("loop      %s stays on %s until its next turn (switch to %s waits)", session, p.target, target)
					return p.target, p.via
				}
			}
		}
	}
	e.pins.set(key, target, via, now)
	return target, via
}

// setPick names the model outright when an agent's pick does.
func setPick(p *ordjson.Obj, model string) bool {
	if model == "" {
		return false
	}
	if cur, _ := str(p, "model"); cur == model {
		return false
	}
	p.Set("model", model)
	return true
}

// rewrite reads the body, lets change edit it in place, and keeps the result
// as the body to send, encoded as the body came. A body that is not a JSON
// object, or that the edge cannot decode, goes on as it was.
func (e *Edge) rewrite(r *http.Request, d *decision, change func(*ordjson.Obj) bool) error {
	content, err := e.plainBody(r, d, inferenceMax)
	if err != nil {
		var unread errUnread
		if errors.As(err, &unread) {
			return unread.error
		}
		e.logf("body      %s %s sent as it came: %v", r.Method, r.URL.Path, err)
		return nil
	}
	if len(content) == 0 {
		return nil
	}
	v, perr := ordjson.Parse(content)
	if p, ok := v.(*ordjson.Obj); perr == nil && ok && change(p) {
		d.body = encodeBody(ordjson.Marshal(p, false), d.edge.coding)
	}
	return nil
}

// imageBudget is how long one upload's image work may take, the wait for a
// slot included; an image it has not reached by then goes on as it was.
var imageBudget = 20 * time.Second

// slimUpload lightens an events upload for the phone. Its body is read only up
// to eventsMax, on the wire and decoded alike: a longer one goes on untouched.
// A slimmed body goes on in the coding it came in.
func (e *Edge) slimUpload(r *http.Request, d *decision) error {
	wire, whole, err := readUpTo(r, eventsMax)
	if err != nil {
		return err
	}
	if !whole {
		e.logf("events    over %d bytes: sent as it came", eventsMax)
		return nil
	}
	d.body = wire
	if len(wire) == 0 {
		return nil
	}
	raw, c, err := decodeBody(wire, r.Header, eventsMax)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			e.logf("events    over %d bytes decoded: sent as it came", eventsMax)
		}
		return nil
	}
	v, err := ordjson.Parse(raw)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), imageBudget)
	defer cancel()
	shrink := func(data, mediaType string) (string, string, bool) { return sipsShrink(ctx, data, mediaType) }
	if !slimEvents(v, shrink) {
		return nil
	}
	slim := ordjson.Marshal(v, false)
	d.body = encodeBody(slim, c)
	e.logf("events    %d -> %d bytes", len(raw), len(slim))
	return nil
}

// --- the reply -----------------------------------------------------------------

// reached is whether the connection that answered is the endpoint the request
// was sent to: the same host and the same port. dialed is the address that
// connection was dialled for (host:port as the transport was asked for it), so
// a named upstream is compared by its name, never by the IP it resolved to,
// and a literal (a loopback adapter, an IP upstream) by its address.
func reached(host string, port int, dialed string) bool {
	h, p, err := net.SplitHostPort(dialed)
	if err != nil {
		return false
	}
	if n, err := strconv.Atoi(p); err != nil || n != port {
		return false
	}
	return sameHost(host, h)
}

// sameHost compares two hosts: an IP literal by its address (::1 in any
// spelling), a name as written, case and a final dot aside.
func sameHost(a, b string) bool {
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	if ia != nil || ib != nil {
		return ia != nil && ib != nil && ia.Equal(ib)
	}
	return strings.EqualFold(strings.TrimSuffix(a, "."), strings.TrimSuffix(b, "."))
}

// verdict is the log line for one turn's reply: requested-vs-served, so the
// profile the pick set must be the one that replies, and a turn answered by
// anything else says so, never a quiet success. Anthropic stamps its
// organization on every response; that is the proof of which upstream
// answered. A routed call must not carry it; an account call must. A profile
// the proxy holds no endpoint for falls back to the edge's route, and the reply
// is then the route's, not the pick's: a miss, with the fallback's own warning
// above it. dialed is the address the answering connection was dialled for,
// "" when there is none to read (the turn left through the session's own
// proxy): a routed turn is then "unverified", never a match.
func verdict(es *edgeState, status int, path string, firstParty bool, host string, port int, dialed string) string {
	_, line := judge(es, status, path, firstParty, host, port, dialed)
	return line
}

// judge is verdict with its decision: whether the turn was a miss (served by
// anything but the endpoint its pick chose) and the line that says so. The
// outcome record counts the decision, never the line's words (GH-977).
func judge(es *edgeState, status int, path string, firstParty bool, host string, port int, dialed string) (bool, string) {
	served := dialed
	if served == "" {
		served = net.JoinHostPort(host, strconv.Itoa(port))
	}
	var kind, extra, where string
	if es.account {
		kind, where = "account   ", "ANTHROPIC"
		if !firstParty {
			kind, where = "miss      ", served+" (no Anthropic signature)"
		}
	} else {
		if es.via != es.asked {
			extra = " via=" + es.via
		}
		where = served
		if firstParty {
			where = "ANTHROPIC (not routed!)"
		}
		switch {
		case es.via != es.asked || firstParty:
			kind = "miss      "
		case dialed == "":
			kind = "unverified "
		case !reached(host, port, dialed):
			kind = "miss      "
		default:
			kind = "route     "
		}
	}
	return kind == "miss      ", fmt.Sprintf("%s<- %d %s asked=%s%s served-by=%s", kind, status, path, es.asked, extra, where)
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
// Anthropic itself only when the account word says so. Whether it is one is
// what the request was as it came (decide), never only the path it is sent
// on: a base with a path in front (https://api.anthropic.com/gateway) is
// still Anthropic.
func refuseFirstParty(u *url.URL, d *decision) error {
	if d.edge == nil || d.edge.account || !(d.edge.inference || isInference(u.Path)) {
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
