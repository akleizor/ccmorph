package agentproxy

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// got is one request a stub endpoint received.
type got struct {
	path, query, auth, key, beta, session, agent, encoding string
	raw                                                    []byte
	body                                                   map[string]any
}

func stub(t *testing.T, tlsServer bool, org string) (*httptest.Server, chan got) {
	t.Helper()
	ch := make(chan got, 16)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(raw, &body)
		ch <- got{r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Beta"),
			r.Header.Get(SessionHeader), r.Header.Get(AgentHeader), r.Header.Get("Content-Encoding"), raw, body}
		if org != "" {
			w.Header().Set("anthropic-organization-id", org)
		}
		io.WriteString(w, "ok")
	})
	var s *httptest.Server
	if tlsServer {
		s = httptest.NewTLSServer(h)
	} else {
		s = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s, ch
}

type edgeRig struct {
	p      *Proxy
	e      *Edge
	client *http.Client
	bridge string
	first  chan got // api.anthropic.com: the account
	route  chan got // the edge's route
	other  chan got // a profile brought up beside it
	otherU string
	log    *syncBuf
	ca     *CA
}

func newEdgeRig(t *testing.T, mutate func(*Edge, *edgeRig)) *edgeRig {
	t.Helper()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	first, firstCh := stub(t, true, "org-1")
	route, routeCh := stub(t, false, "")
	other, otherCh := stub(t, false, "")
	fu, _ := url.Parse(first.URL)
	old := firstParty
	firstParty = fu
	t.Cleanup(func() { firstParty = old })
	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	ru, _ := url.Parse(route.URL)
	rig := &edgeRig{bridge: t.TempDir(), first: firstCh, route: routeCh, other: otherCh, otherU: other.URL, log: &syncBuf{}, ca: ca}
	rig.e = NewEdge(&Edge{Upstream: ru, Key: "route-key", Tiers: map[string]string{"opus": "route-opus", "fable": "gpt-6-astra"},
		Inference: filepath.Join(rig.bridge, "inference"), Log: rig.log})
	if mutate != nil {
		mutate(rig.e, rig)
	}
	write(t, rig.e.Inference, []byte("route\n"))
	up := x509.NewCertPool()
	up.AddCert(first.Certificate())
	rig.p, err = Start(ca, Options{Edge: rig.e, Dir: rig.e.AgentsDir(), TLS: &tls.Config{RootCAs: up}, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rig.p.Close)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	proxyURL, _ := url.Parse("http://127.0.0.1:" + itoa(rig.p.Port()))
	rig.client = &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return rig
}

func (r *edgeRig) word(t *testing.T, w string) { t.Helper(); write(t, r.e.Inference, []byte(w+"\n")) }

// post is one turn as Claude Code sends it: the session's own credential, the
// header naming its session (and agent), the first message carrying the prompt.
func (r *edgeRig) post(t *testing.T, path, session, agent, model, prompt string) (int, http.Header) {
	t.Helper()
	body := `{"model":"` + model + `","thinking":{"display":"updates"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2; cch=abc;","cache_control":{"type":"ephemeral","scope":"global"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":` + quote(prompt) + `}]}]}`
	return r.send(t, path, session, agent, []byte(body), nil)
}

func (r *edgeRig) send(t *testing.T, path, session, agent string, body []byte, extra map[string]string) (int, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("POST", "https://api.anthropic.com"+path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer ACCOUNT-SECRET")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14")
	if session != "" {
		req.Header.Set(SessionHeader, session)
	}
	if agent != "" {
		req.Header.Set(AgentHeader, agent)
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode, resp.Header
}

func (r *edgeRig) expect(t *testing.T, ch chan got, what string) got {
	t.Helper()
	select {
	case g := <-ch:
		return g
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never received the request", what)
	}
	return got{}
}

func (r *edgeRig) quiet(t *testing.T, ch chan got, what string) {
	t.Helper()
	select {
	case g := <-ch:
		t.Fatalf("%s received a request: %+v", what, g)
	default:
	}
}

const msgs = "/v1/messages?beta=true"

// test_the_account_word_marks_what_it_leaves_first_party, test_account_credential_never_reaches_the_route
func TestTheAccountWordLeavesTheCallOnTheAccountAndTheRouteWordNeverCarriesIt(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	if code, _ := r.post(t, msgs, "s1", "", "claude-opus-5-5", "hi"); code != 200 {
		t.Fatal(code)
	}
	g := r.expect(t, r.first, "the account")
	if g.auth != "Bearer ACCOUNT-SECRET" || !strings.Contains(g.beta, "oauth-") || g.path != "/v1/messages" || g.query != "beta=true" {
		t.Fatalf("the account's call changed: %+v", g)
	}
	if g.body["thinking"].(map[string]any)["display"] != "updates" { // untouched but for its model
		t.Fatalf("an account body was made portable: %v", g.body)
	}
	r.quiet(t, r.route, "the route")

	r.word(t, "route")
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "hi")
	g = r.expect(t, r.route, "the route")
	if strings.Contains(g.auth+g.key+g.beta, "ACCOUNT-SECRET") || strings.Contains(g.beta, "oauth") {
		t.Fatalf("the account's credential reached the route: %+v", g)
	}
	if g.key != "route-key" || g.auth != "Bearer route-key" || g.beta != "fine-grained-tool-streaming-2025-05-14" {
		t.Fatalf("route credential: %+v", g)
	}
	if g.path != "/v1/messages" || g.query != "beta=true" || g.body["model"] != "route-opus" || g.body["thinking"].(map[string]any)["display"] != "omitted" {
		t.Fatalf("routed body or path: %+v", g)
	}
	r.quiet(t, r.first, "the account")
}

// test_a_session_word_overrides_the_shared_switch_for_its_own_requests
func TestASessionWordOverridesTheSharedSwitch(t *testing.T) {
	r := newEdgeRig(t, nil)
	os.MkdirAll(filepath.Join(r.bridge, "sessions"), 0o700)
	write(t, filepath.Join(r.bridge, "sessions", "cse_mine"), []byte("account\n"))
	r.post(t, msgs, headerID("cse_mine"), "", "claude-opus-5-5", "x")
	r.expect(t, r.first, "its own word's account")
	r.post(t, msgs, headerID("cse_other"), "", "claude-opus-5-5", "x")
	r.expect(t, r.route, "the shared route")
	r.post(t, msgs, "", "", "claude-opus-5-5", "x") // no session: the shared switch
	r.expect(t, r.route, "the shared route")
	r.post(t, msgs, "../inference", "", "claude-opus-5-5", "x") // not an id: the shared switch
	r.expect(t, r.route, "the shared route")
	if !strings.Contains(r.log.String(), "account   POST /v1/messages (session)") {
		t.Fatalf("the log does not say whose word it was:\n%s", r.log)
	}
}

// test_a_switch_word_moves_the_turn_only_where_a_profile_was_brought_up
func TestASwitchWordMovesTheTurnOnlyWhereAProfileWasBroughtUp(t *testing.T) {
	r := newEdgeRig(t, func(e *Edge, _ *edgeRig) { e.Tiers = map[string]string{"sonnet": "boot-route-sonnet"} })
	r.word(t, "gw")
	r.post(t, msgs, "s1", "", "sonnet", "x") // never brought up: the route, with a reason
	g := r.expect(t, r.route, "the route")
	if g.body["model"] != "boot-route-sonnet" || !strings.Contains(r.log.String(), "has no endpoint in this proxy") {
		t.Fatalf("%+v\n%s", g, r.log)
	}
	h, _ := json.Marshal(map[string]any{"upstream": r.otherU, "key": "gw-key", "tiers": map[string]string{"sonnet": "claude-sonnet-5-5"}})
	write(t, filepath.Join(r.bridge, "switch-gw.json"), h)
	r.post(t, msgs, "s1", "", "sonnet", "x") // brought up: the turn moves
	g = r.expect(t, r.other, "the brought up profile")
	if g.key != "gw-key" || g.body["model"] != "claude-sonnet-5-5" {
		t.Fatalf("%+v", g)
	}
	none(t, r.bridge, "switch-*")
	if !strings.Contains(r.log.String(), "route     POST /v1/messages via gw") ||
		!strings.Contains(r.log.String(), "asked=gw served-by=127.0.0.1:") {
		t.Fatalf("the reply line:\n%s", r.log)
	}
	r.quiet(t, r.route, "the route")
}

// test_every_session_turn_answers_which_upstream_served_it, end to end: an
// account word answered by the account, a pick the proxy cannot serve a miss.
func TestTheReplyLineAnswersRequestedVersusServed(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	r.expect(t, r.route, "the route")
	r.word(t, "account")
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	r.expect(t, r.first, "the account")
	r.word(t, "ds") // a pick no proxy holds an endpoint for
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	r.expect(t, r.route, "the route")
	var replies []string
	for _, ln := range strings.Split(r.log.String(), "\n") {
		if strings.Contains(ln, "served-by=") {
			replies = append(replies, ln)
		}
	}
	if len(replies) != 3 {
		t.Fatalf("every turn and only the turns:\n%s", r.log)
	}
	for i, want := range []string{"route     <- 200 /v1/messages asked=route served-by=127.0.0.1:", "account   <- 200 /v1/messages asked=account served-by=ANTHROPIC", "miss      <- 200 /v1/messages asked=ds via=route served-by=127.0.0.1:"} {
		if !strings.Contains(replies[i], want) {
			t.Errorf("reply %d: %q lacks %q", i, replies[i], want)
		}
	}
}

// An account turn names the account's own model for a tier the route pinned.
func TestAnAccountTurnReadsARoutePinnedTierBackToTheAccountsModel(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	write(t, filepath.Join(r.bridge, "account-tiers.json"), []byte(`{"fable":"claude-fable-5-2"}`))
	r.post(t, msgs, "s1", "", "gpt-6-astra", "x")
	if g := r.expect(t, r.first, "the account"); g.body["model"] != "claude-fable-5-2" {
		t.Fatalf("%v", g.body)
	}
	r.post(t, msgs, "s1", "", "claude-sonnet-5-5", "x") // a model the phone picked stays
	if g := r.expect(t, r.first, "the account"); g.body["model"] != "claude-sonnet-5-5" {
		t.Fatalf("%v", g.body)
	}
}

// Cross-profile subagents (test_a_picked_subagent_is_served_by_its_pick_through_a_real_proxy and
// test_a_picked_subagent_is_parked_then_sent_to_its_pick): the main thread keeps
// the account's word while a picked subagent reaches the route on its key and
// the pick's model; a later turn is bound; another agent, another session's agent
// and a same-session fan-out never take the pick.
func TestAPickedSubagentIsServedByItsPick(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	if err := LeavePick(r.e.AgentsDir(), "s1", "Fix parse()", Pick{Word: "route", Model: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "Fix parse()") // the main thread: its word, never the pick
	if g := r.expect(t, r.first, "the account"); g.body["model"] != "claude-opus-5-5" {
		t.Fatal(g.body)
	}
	r.post(t, msgs, "s2", "b1", "claude-opus-5-5", "Fix parse()") // another session's agent: never the pick
	r.expect(t, r.first, "the account")
	r.post(t, msgs, "s1", "a1", "claude-opus-5-5", "Fix parse()") // the subagent: claimed, routed
	g := r.expect(t, r.route, "the route")
	if g.key != "route-key" || g.body["model"] != "kimi-k3" || strings.Contains(g.auth, "ACCOUNT") {
		t.Fatalf("%+v", g)
	}
	r.post(t, msgs, "s1", "a3", "claude-opus-5-5", "Fix parse()") // same-session fan-out: the pick is gone
	r.expect(t, r.first, "the account")
	r.post(t, msgs, "s1", "a1", "claude-opus-5-5", "go on") // its later turn: bound
	if g := r.expect(t, r.route, "the route"); g.body["model"] != "kimi-k3" {
		t.Fatal(g.body)
	}
	r.post(t, msgs, "s1", "a2", "claude-opus-5-5", "unrelated") // no pick waits
	r.expect(t, r.first, "the account")
	DropAgent(r.e.AgentsDir(), "a1")
	r.post(t, msgs, "s1", "a1", "claude-opus-5-5", "after stop")
	r.expect(t, r.first, "the account")
	if _, err := os.Stat(r.e.AgentsDir()); err != nil {
		t.Fatal("the router's folder is gone")
	}
}

// test_dead_route_fails_closed_and_key_file_is_consumed: a dead route is an
// error, never a fallback to the subscription.
func TestADeadRouteFailsClosed(t *testing.T) {
	r := newEdgeRig(t, func(e *Edge, _ *edgeRig) {
		ln, _ := net.Listen("tcp4", "127.0.0.1:0")
		e.Upstream, _ = url.Parse("http://" + ln.Addr().String())
		ln.Close()
	})
	code, hdr := r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	if code != http.StatusBadGateway || hdr.Get("anthropic-organization-id") != "" {
		t.Fatalf("a dead route answered %d %v", code, hdr)
	}
	r.quiet(t, r.first, "the account")
}

// test_launcher_proxy_never_falls_back_to_anthropic: whatever the decision
// holds, an inference request addressed to Anthropic goes only under the account word.
func TestNothingReachesAnthropicWithoutTheAccountWord(t *testing.T) {
	r := newEdgeRig(t, nil)
	for name, up := range map[string]string{
		"the route is Anthropic's own address": firstParty.String(),
		"the route is not an http(s) URL":      "file:///etc/passwd",
	} {
		r.e.Upstream, _ = url.Parse(up)
		if code, _ := r.post(t, msgs, "s1", "", "claude-opus-5-5", "x"); code != http.StatusBadGateway {
			t.Errorf("%s: answered %d", name, code)
		}
		r.quiet(t, r.first, "Anthropic ("+name+")")
	}
	r.word(t, "account")
	if code, _ := r.post(t, msgs, "s1", "", "claude-opus-5-5", "x"); code != 200 {
		t.Fatalf("the account word answered %d", code)
	}
	r.expect(t, r.first, "the account")
}

// Non-inference traffic (bridge, OAuth, telemetry) reaches Anthropic untouched.
func TestEverythingElseReachesAnthropicUntouched(t *testing.T) {
	r := newEdgeRig(t, nil)
	body := `{"a":1}`
	r.send(t, "/v1/code/sessions/cse_1/worker/heartbeat", "s1", "", []byte(body), nil)
	g := r.expect(t, r.first, "Anthropic")
	if string(g.raw) != body || g.auth != "Bearer ACCOUNT-SECRET" {
		t.Fatalf("%+v", g)
	}
	r.quiet(t, r.route, "the route")
	if !strings.Contains(r.log.String(), "anthropic POST "+firstParty.Hostname()+"/v1/code/sessions/cse_1/worker/heartbeat") {
		t.Fatal(r.log)
	}
}

// The phone's event uploads are lightened; the model's own transcript never is.
func TestEventUploadsAreSlimmedOnTheWay(t *testing.T) {
	r := newEdgeRig(t, nil)
	long := strings.Repeat("x", textMax*3)
	body := `{"events":[{"payload":{"type":"assistant","message":{"content":[{"type":"thinking","signature":"` + strings.Repeat("S", 900) + `"}]}}},` +
		`{"payload":{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"u","content":"` + long + `"}]}}}]}`
	path := "/v1/code/sessions/cse_01Ab/worker/events"
	r.send(t, path, "s1", "", []byte(body), nil)
	g := r.expect(t, r.first, "Anthropic")
	if len(g.raw) >= len(body)/2 || strings.Contains(string(g.raw), "SSSS") || g.body == nil {
		t.Fatalf("not slimmed: %d of %d bytes", len(g.raw), len(body))
	}
	if !strings.Contains(r.log.String(), "events    ") {
		t.Fatal(r.log)
	}
	// compressed: the slim body goes on uncompressed, and says so
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write([]byte(body))
	zw.Close()
	r.send(t, path, "s1", "", z.Bytes(), map[string]string{"Content-Encoding": "gzip"})
	g = r.expect(t, r.first, "Anthropic")
	if g.encoding != "" || len(g.raw) >= len(body)/2 {
		t.Fatalf("gzip upload: encoding %q, %d bytes", g.encoding, len(g.raw))
	}
	// already slim: the body is sent as it came
	r.send(t, path, "s1", "", []byte(`{"events":[]}`), nil)
	if g = r.expect(t, r.first, "Anthropic"); string(g.raw) != `{"events":[]}` {
		t.Fatalf("%s", g.raw)
	}
	// another path under the same session is never slimmed
	r.send(t, path+"/stream", "s1", "", []byte(body), nil)
	if g = r.expect(t, r.first, "Anthropic"); len(g.raw) != len(body) {
		t.Fatal("a non-events upload was changed")
	}
}

// A handoff for another edge is never touched by a request that asks for its profile.
func TestARequestNeverTakesAnotherEdgesHandoff(t *testing.T) {
	r := newEdgeRig(t, func(e *Edge, _ *edgeRig) { e.Parent, e.ParentStarted = os.Getpid(), "start-me" })
	r.word(t, "gw")
	h := handoff(r.otherU, "gw-key", "h6", 600, "start-600")
	write(t, filepath.Join(r.bridge, "switch-gw.json"), h)
	r.post(t, msgs, "s1", "", "claude-opus-5-5", "x")
	r.expect(t, r.route, "the route")
	if b, _ := os.ReadFile(filepath.Join(r.bridge, "switch-gw.json")); string(b) != string(h) {
		t.Fatal("another edge's handoff was touched")
	}
}

// The CA of the proxy this one replaces is adopted, so sessions that trust its
// certificate keep trusting the edge across the change.
func TestTheEdgeAdoptsTheCAOfTheProxyItReplaces(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "mitmproxy"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().AddDate(10, 0, 0),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pk8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8})
	legacy := filepath.Join(t.TempDir(), "mitmproxy-ca.pem")
	write(t, legacy, append(append([]byte{}, keyPEM...), certPEM...))
	dir := filepath.Join(t.TempDir(), "edge-ca")
	ca, err := LoadEdgeCA(dir, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ca.PEM, certPEM) {
		t.Fatal("the legacy certificate was not adopted")
	}
	leaf, err := ca.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(leaf.Certificate[0])
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	if _, err := c.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Fatalf("a session trusting the old certificate rejects the new proxy: %v", err)
	}
	if again, err := LoadEdgeCA(dir, ""); err != nil || !bytes.Equal(again.PEM, certPEM) {
		t.Fatalf("the adopted CA was not kept: %v", err)
	}
	if ca2, err := LoadEdgeCA(filepath.Join(t.TempDir(), "fresh"), filepath.Join(t.TempDir(), "none")); err != nil || ca2 == nil {
		t.Fatalf("no legacy CA: %v", err) // makes its own
	}
}

// edge-proxy as a process: reads its key and deletes the file, serves, follows
// its edge, says what it holds and takes it back when it ends.
func TestEdgeProxyAsAProcess(t *testing.T) {
	if os.Getenv("EDGE_PROXY_HELPER") == "1" {
		os.Exit(EdgeMain(strings.Split(os.Getenv("EDGE_PROXY_ARGS"), " "), os.Stderr))
	}
	state := t.TempDir()
	keyFile := filepath.Join(state, "route-key.1.abc")
	write(t, keyFile, []byte("sk-route-key"))
	edge := exec.Command("sleep", "30") // the edge the proxy follows
	if err := edge.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { edge.Process.Kill(); edge.Wait() }()
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	started, _ := procStarted(edge.Process.Pid)
	cmd := exec.Command(os.Args[0], "-test.run=^TestEdgeProxyAsAProcess$")
	cmd.Env = append(os.Environ(), "EDGE_PROXY_HELPER=1",
		"EDGE_PROXY_ARGS=--listen 127.0.0.1:"+itoa(port)+" --ca-dir "+filepath.Join(state, "ca"),
		"RC_UPSTREAM=http://127.0.0.1:9", "RC_KEY_FILE="+keyFile, "RC_PARENT_PID="+itoa(edge.Process.Pid),
		"RC_PARENT_STARTED="+started, "RC_INFERENCE="+filepath.Join(state, "inference"), "RC_TIERS={}")
	var errs syncBuf
	cmd.Stderr = &errs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer cmd.Process.Kill()
	waitFor(t, "the proxy listening", func() bool {
		c, err := net.Dial("tcp4", "127.0.0.1:"+itoa(port))
		if err == nil {
			c.Close()
		}
		return err == nil
	})
	if _, err := os.Stat(keyFile); err == nil {
		t.Fatal("the proxy did not consume the route key")
	}
	if _, err := os.Stat(filepath.Join(state, "ca", "ca.pem")); err != nil {
		t.Fatal("no CA")
	}
	held := filepath.Join(state, "held."+itoa(edge.Process.Pid)+".json")
	waitFor(t, "held.<edge>.json", func() bool { _, err := os.Stat(held); return err == nil })
	// a handoff naming the edge is taken within a second and acknowledged
	write(t, filepath.Join(state, "switch-gw.json"), handoff("http://127.0.0.1:1", "k", "h1", edge.Process.Pid, started))
	waitFor(t, "the handoff taken", func() bool {
		b, _ := os.ReadFile(held)
		return strings.Contains(string(b), `"h1"`)
	})
	none(t, state, "switch-*")
	// it ends with its edge, and takes its record with it
	edge.Process.Kill()
	edge.Wait()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatalf("the proxy outlived its edge\n%s", errs.String())
	}
	if _, err := os.Stat(held); err == nil {
		t.Fatal("what it held outlives it")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(25 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The failed starts of test_no_failed_start_ever_leaves_the_route_key_behind,
// from the proxy's side: whatever it refuses, the key file is gone.
func TestEdgeProxyConsumesTheKeyEvenWhenItCannotStart(t *testing.T) {
	state := t.TempDir()
	keyFile := filepath.Join(state, "route-key.1.abc")
	write(t, keyFile, []byte("sk"))
	t.Setenv("RC_UPSTREAM", "http://127.0.0.1:9")
	t.Setenv("RC_KEY_FILE", keyFile)
	t.Setenv("RC_INFERENCE", filepath.Join(state, "inference"))
	if code := EdgeMain([]string{"--listen", "127.0.0.1:0"}, io.Discard); code == 0 { // no --ca-dir
		t.Fatal("started without a CA directory")
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Fatal("the key file stayed after a refused start")
	}
}
