package agentproxy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type seen struct {
	auth, key, beta, agent, path string
	body                         map[string]any
}

func recorder(t *testing.T, tlsServer bool, org string) (*httptest.Server, chan seen) {
	t.Helper()
	got := make(chan seen, 4)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Beta"), r.Header.Get(AgentHeader), r.URL.Path, body}
		if org != "" {
			w.Header().Set("anthropic-organization-id", org)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	})
	if tlsServer {
		s := httptest.NewTLSServer(h)
		t.Cleanup(s.Close)
		return s, got
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, got
}

// harness: a "first party" TLS server stands in for api.anthropic.com, a plain
// gateway serves the picked profile, and a client that trusts the proxy's CA
// talks to https://api.anthropic.com through the proxy, as Claude Code does.
func harness(t *testing.T) (*Proxy, *http.Client, chan seen, chan seen, *httptest.Server) {
	t.Helper()
	first, firstGot := recorder(t, true, "org-1")
	gw, gwGot := recorder(t, false, "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	u, _ := url.Parse(first.URL)
	old := firstParty
	firstParty = u
	t.Cleanup(func() { firstParty = old })

	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	// The gateway's base carries a path, as https://host/anthropic does.
	gwURL, _ := url.Parse(gw.URL + "/gw")
	// The proxy's outbound trust is the session's extra CA: the test server's.
	up := x509.NewCertPool()
	up.AddCert(first.Certificate())
	p, err := Start(ca, Options{Dir: filepath.Join(t.TempDir(), "agents"), TLS: &tls.Config{RootCAs: up},
		Resolve: func(p Pick) (Target, error) { return Target{Base: gwURL, Key: "gw-key-" + p.Word}, nil },
		Logf:    t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.PEM)
	pool.AddCert(first.Certificate())
	proxyURL, _ := url.Parse("http://127.0.0.1:" + itoa(p.Port()))
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return p, client, firstGot, gwGot, first
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func send(t *testing.T, c *http.Client, agent, prompt string) {
	t.Helper()
	body := `{"model":"claude-opus-5-5","thinking":{"display":"updates"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2; cch=abc; cc_turn_index=3;","cache_control":{"type":"ephemeral","scope":"global"}}],"tools":[{"name":"Read","input_schema":{"type":"object"}},{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"text","text":` + quote(prompt) + `}]}]}`
	req, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-ant-oat-session")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20,fine-grained-tool-streaming-2025-05-14")
	if agent != "" {
		req.Header.Set(AgentHeader, agent)
	}
	req.Header.Set(SessionHeader, "s1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "message_stop") {
		t.Fatalf("stream lost: %q", b)
	}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestMainThreadPassesThroughUntouched(t *testing.T) {
	_, c, first, gw, _ := harness(t)
	send(t, c, "", "hello")
	got := <-first
	if got.auth != "Bearer sk-ant-oat-session" || !strings.Contains(got.beta, "oauth-") {
		t.Fatalf("session credential changed: %+v", got)
	}
	if len(got.body["tools"].([]any)) != 2 || got.body["model"] != "claude-opus-5-5" || got.body["thinking"].(map[string]any)["display"] != "updates" {
		t.Fatalf("body changed: %v", got.body)
	}
	select {
	case g := <-gw:
		t.Fatalf("gateway was called: %+v", g)
	default:
	}
}

func TestPickedAgentGoesToItsProfile(t *testing.T) {
	p, c, first, gw, _ := harness(t)
	if err := LeavePick(p.Dir, "s1", "  write the report \n", Pick{Word: "ds", Model: "deepseek-v5"}); err != nil {
		t.Fatal(err)
	}
	send(t, c, "agent1", "write the report")
	got := <-gw
	if got.path != "/gw/v1/messages" {
		t.Fatalf("the target's base path is lost: %q", got.path)
	}
	if got.auth != "Bearer gw-key-ds" || got.key != "gw-key-ds" || strings.Contains(got.beta, "oauth-") {
		t.Fatalf("credential: %+v", got)
	}
	if got.body["model"] != "deepseek-v5" || got.body["thinking"].(map[string]any)["display"] != "omitted" {
		t.Fatalf("body: %v", got.body)
	}
	if tools := got.body["tools"].([]any); len(tools) != 1 || tools[0].(map[string]any)["name"] != "Read" {
		t.Fatalf("server tools kept: %v", tools)
	}
	sys := got.body["system"].([]any)[0].(map[string]any)
	if strings.Contains(sys["text"].(string), "cch=") || sys["cache_control"].(map[string]any)["scope"] != nil {
		t.Fatalf("first-party fields kept: %v", sys)
	}
	// The binding holds for the agent's later turns; another agent stays home.
	send(t, c, "agent1", "a later turn")
	<-gw
	send(t, c, "agent2", "write the report")
	if g := <-first; g.agent != "agent2" {
		t.Fatalf("agent2 went elsewhere: %+v", g)
	}
	DropAgent(p.Dir, "agent1")
	send(t, c, "agent1", "after stop")
	<-first
}

// #959: a pick names the session that left it, so two sessions sending one
// prompt leave two picks, a claim reads only its own session's folder, and a
// request that names no session claims nothing.
func TestAPickIsClaimedOnlyByItsSessionsAgent(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"messages":[{"content":"Fix parse()"}]}`)
	for session, word := range map[string]string{"s1": "gw1", "s2": "gw2"} {
		if err := LeavePick(dir, session, "Fix parse()", Pick{Word: word, Model: "m-" + word}); err != nil {
			t.Fatal(err)
		}
	}
	if p, ok := Claim(dir, "", "x", body, time.Now()); ok {
		t.Fatalf("a claim with no session bound %+v", p)
	}
	if HasPending(dir, "") {
		t.Fatal("a pick with no session to name parked a request")
	}
	if p, ok := Claim(dir, "s3", "a3", body, time.Now()); ok {
		t.Fatalf("a foreign session's agent bound %+v", p)
	}
	if p, ok := Claim(dir, "s1", "a1", body, time.Now()); !ok || p.Word != "gw1" {
		t.Fatalf("session 1's agent bound %+v %v", p, ok)
	}
	if p, ok := Claim(dir, "s2", "a2", body, time.Now()); !ok || p.Word != "gw2" {
		t.Fatalf("session 2's agent bound %+v %v", p, ok)
	}
	if err := LeavePick(dir, "", "Fix parse()", Pick{Word: "gw"}); err == nil {
		t.Fatal("a pick with no session to name was left")
	}
}

// The adapter ccmorph starts for a picked profile is served on loopback;
// the session's own HTTPS_PROXY cannot serve it — a real proxy refuses the
// absolute URI or resolves it against its own host. A loopback target must
// therefore dial direct even when an upstream is configured.
func TestALoopbackTargetIsDialedDirectly(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	adapter, got := recorder(t, false, "")
	upstreamHits := make(chan string, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits <- r.Method + " " + r.URL.String()
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)

	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse(adapter.URL + "/gw")
	p, err := Start(ca, Options{Dir: filepath.Join(t.TempDir(), "agents"), Logf: t.Logf,
		Upstream: upURL,
		Resolve:  func(Pick) (Target, error) { return Target{Base: base, Key: "k"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := LeavePick(p.Dir, "s1", "  write the report \n", Pick{Word: "ds", Model: "deepseek-v5"}); err != nil {
		t.Fatal(err)
	}

	proxyURL, _ := url.Parse("http://127.0.0.1:" + itoa(p.Port()))
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
	req, _ := http.NewRequest("POST", "http://api.anthropic.com/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"write the report"}]}`))
	req.Header.Set(AgentHeader, "agent1")
	req.Header.Set(SessionHeader, "s1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case hit := <-upstreamHits:
		t.Fatalf("the upstream proxy was asked for a loopback target: %s", hit)
	default:
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "message_stop") {
		t.Fatalf("the loopback target never saw the request: %d %q", resp.StatusCode, b)
	}
	if s := <-got; s.path != "/gw/v1/messages" {
		t.Fatalf("the target's base path is lost: %q", s.path)
	}
	// A remote target still chains the upstream: a second agent without a
	// pick stays on the session's route, which is the proxy's address.
	req2, _ := http.NewRequest("POST", "http://api.anthropic.com/v1/messages?beta=true",
		strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"a different prompt"}]}`))
	req2.Header.Set(AgentHeader, "agent2")
	req2.Header.Set(SessionHeader, "s1")
	resp2, err := c.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if hit := <-upstreamHits; hit != "POST http://api.anthropic.com/v1/messages?beta=true" {
		t.Fatalf("a remote target stopped chaining the upstream: %q", hit)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1":          true,
		"127.0.0.53":         true,
		"::1":                true,
		"localhost":          true,
		"LOCALHOST":          true,
		"localhost.":         true,
		"127.0.0.1.evil.com": false,
		"localhost.evil.com": false,
		"api.anthropic.com":  false,
		"10.0.0.8":           false,
		"":                   false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q)=%v want %v", host, got, want)
		}
	}
}

func TestOtherHostsAreTunnelledNotDecrypted(t *testing.T) {
	_, c, first, _, srv := harness(t)
	// The test server's own certificate answers: the proxy never read the bytes.
	resp, err := c.Get(srv.URL + "/v1/other")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || !resp.TLS.PeerCertificates[0].Equal(srv.Certificate()) {
		t.Fatal("tunnel was decrypted")
	}
	<-first
}

func TestCAIsKeptAndPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := LoadCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadCA(dir)
	if err != nil || string(a.PEM) != string(b.PEM) {
		t.Fatal("CA not reused")
	}
	if fi, _ := os.Stat(filepath.Join(dir, caKeyFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode())
	}
}

func TestServedModel(t *testing.T) {
	cases := []struct{ pick, asked, want string }{
		{"", "claude-opus-5-5", "claude-opus-5-5"},
		{"deepseek-v5", "claude-opus-5-5", "deepseek-v5"},
		{"opus", "claude-opus-5-5", "claude-opus-5-5"},
		{"opus", "claude-sonnet-5-5", "opus"},
		{"gpt-x[1m]", "claude-opus-5-5", "gpt-x"},
	}
	for _, c := range cases {
		if got := servedModel(c.pick, c.asked, nil); got != c.want {
			t.Errorf("servedModel(%q,%q)=%q want %q", c.pick, c.asked, got, c.want)
		}
	}
	if got := servedModel("opus", "claude-opus-5-5", map[string]string{"opus": "claude-opus-5-1[1m]"}); got != "claude-opus-5-1" {
		t.Errorf("pinned tier: %q", got)
	}
}

func TestAGatewaySessionKeepsItsBasePath(t *testing.T) {
	gw, got := recorder(t, false, "")
	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse(gw.URL + "/anthropic")
	p, err := Start(ca, Options{Dir: filepath.Join(t.TempDir(), "agents"), Origin: origin, Logf: t.Logf,
		Resolve: func(Pick) (Target, error) { return Target{}, errNotServedTest }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	resp, err := http.Post("http://127.0.0.1:"+itoa(p.Port())+"/v1/messages?beta=true", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if g := <-got; g.path != "/anthropic/v1/messages" {
		t.Fatalf("the session's base path is lost: %q", g.path)
	}
}

var errNotServedTest = io.EOF

func TestAnAPIKeyOnFirstPartyIsNotSentAsABearer(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-ant-oat-session")
	Target{Base: &url.URL{Scheme: "https", Host: anthropicHost}, Key: "sk-ant-api"}.setAuth(h)
	if h.Get("Authorization") != "" || h.Get("X-Api-Key") != "sk-ant-api" {
		t.Fatalf("first party gets the key alone: %v", h)
	}
}

func TestLaunchersStartingTogetherKeepOneMatchingCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	pems := make(chan string, 8)
	for i := 0; i < 8; i++ {
		go func() {
			ca, err := LoadCA(dir)
			if err != nil {
				pems <- "error: " + err.Error()
				return
			}
			pems <- string(ca.PEM)
		}()
	}
	first := <-pems
	for i := 1; i < 8; i++ {
		if p := <-pems; p != first {
			t.Fatalf("launchers kept different CAs: %.40q vs %.40q", first, p)
		}
	}
	if _, err := readCA(dir, renewBefore); err != nil {
		t.Fatal(err)
	}
}

func TestAMismatchedCAIsReplaced(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	if _, err := LoadCA(a); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(b); err != nil {
		t.Fatal(err)
	}
	key, _ := os.ReadFile(filepath.Join(b, caKeyFile))
	os.WriteFile(filepath.Join(a, caKeyFile), key, 0o600)
	if _, err := readCA(a, renewBefore); err == nil {
		t.Fatal("a key from another CA is accepted")
	}
	if _, err := LoadCA(a); err != nil {
		t.Fatalf("a mismatched pair is not replaced: %v", err)
	}
}

func TestACompressedAgentTurnInASessionIsClaimedAndStaysCompressed(t *testing.T) {
	type got struct {
		enc  string
		body []byte
	}
	seenc := make(chan got, 1)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seenc <- got{r.Header.Get("Content-Encoding"), b}
	}))
	defer gw.Close()
	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("http://127.0.0.1:1") // the session's own route: never reached
	gwURL, _ := url.Parse(gw.URL)
	dir := filepath.Join(t.TempDir(), "agents")
	p, err := Start(ca, Options{Dir: dir, Origin: origin, Logf: t.Logf,
		Resolve: func(Pick) (Target, error) { return Target{Base: gwURL, Key: "k"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	const prompt = "Reply with ALPHA."
	plain := `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"` + prompt + `"}]}`
	if err := LeavePick(dir, "s1", prompt, Pick{Word: "gw", Model: "picked-model"}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "http://127.0.0.1:"+itoa(p.Port())+"/v1/messages", bytes.NewReader(encodeBody([]byte(plain), gzipCoding)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set(AgentHeader, "agent-1")
	req.Header.Set(SessionHeader, "s1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var g got
	select {
	case g = <-seenc:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent's turn never reached its pick's endpoint")
	}
	h := http.Header{}
	h.Set("Content-Encoding", g.enc)
	body, _, err := decodeBody(g.body, h, 1<<20)
	if g.enc != "gzip" || err != nil || !strings.Contains(string(body), `"model":"picked-model"`) {
		t.Fatalf("the gzipped first turn was not claimed and sent on gzipped: enc=%q err=%v %.200s", g.enc, err, body)
	}
}
