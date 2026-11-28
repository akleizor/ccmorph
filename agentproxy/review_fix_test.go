package agentproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// proxyWithPick starts a plain proxy whose picked target is base, behind upstream.
func proxyWithPick(t *testing.T, base string, upstream *url.URL) (*Proxy, *http.Client) {
	t.Helper()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	bu, _ := url.Parse(base)
	p, err := Start(ca, Options{Dir: filepath.Join(t.TempDir(), "agents"), Logf: t.Logf, Upstream: upstream,
		Resolve: func(Pick) (Target, error) { return Target{Base: bu, Key: "k"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	pu, _ := url.Parse("http://127.0.0.1:" + itoa(p.Port()))
	return p, &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
}

func inferenceReq(t *testing.T, agent, session, body string) *http.Request {
	t.Helper()
	req, _ := http.NewRequest("POST", "http://api.anthropic.com/v1/messages?beta=true&secret=q", strings.NewReader(body))
	req.Header.Set(AgentHeader, agent)
	req.Header.Set(SessionHeader, session)
	return req
}

// NO_PROXY exempts a target from the session's upstream proxy, and a failed
// call answers an Anthropic-shaped JSON error that names the host only.
func TestNoProxyExemptsATargetAndAFailureIsAnAPIError(t *testing.T) {
	hits := make(chan string, 4)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- r.URL.String()
		w.WriteHeader(http.StatusTeapot)
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	t.Setenv("NO_PROXY", "gw.invalid")
	p, c := proxyWithPick(t, "http://gw.invalid:1/gw", upURL)
	if err := LeavePick(p.Dir, "s1", "do it", Pick{Word: "ds"}); err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(inferenceReq(t, "a1", "s1", `{"model":"m","messages":[{"role":"user","content":"do it"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	select {
	case h := <-hits:
		t.Fatalf("a NO_PROXY host went through the upstream proxy: %s", h)
	default:
	}
	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	var e struct {
		Type  string `json:"type"`
		Error struct{ Type, Message string }
	}
	if json.Unmarshal(b, &e) != nil || e.Type != "error" || e.Error.Type != "api_error" ||
		!strings.HasPrefix(e.Error.Message, "agent proxy: gw.invalid:1: ") || strings.Contains(string(b), "secret") {
		t.Fatalf("not an API error naming only the host: %s", b)
	}
}

// A body that cannot be read answers 502 (retried) and is never sent on.
func TestAnUnreadableBodyOnAPickedAgentAnswers502(t *testing.T) {
	hits := make(chan string, 4)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits <- r.URL.Path }))
	t.Cleanup(gw.Close)
	p, c := proxyWithPick(t, gw.URL, nil)
	if err := LeavePick(p.Dir, "s1", "do it", Pick{Word: "ds"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(p.Dir, "s1", "a1", []byte(`{"messages":[{"role":"user","content":"do it"}]}`), time.Now()); !err {
		t.Fatal("claim failed")
	}
	req := inferenceReq(t, "a1", "s1", "not gzip at all")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
	select {
	case h := <-hits:
		t.Fatalf("an unreadable body was sent on: %s", h)
	default:
	}
}

func TestHopDefaultsThePortFromTheScheme(t *testing.T) {
	for in, want := range map[string]string{
		"http://proxy.example":      "proxy.example:80",
		"https://proxy.example":     "proxy.example:443",
		"http://proxy.example:3128": "proxy.example:3128",
		"http://u:p@[::1]":          "[::1]:80",
	} {
		u, _ := url.Parse(in)
		got, _, err := hop(u)
		if err != nil || got != want {
			t.Errorf("hop(%s)=%q,%v want %q", in, got, err, want)
		}
	}
	u, _ := url.Parse("socks5://proxy.example:1080")
	if _, _, err := hop(u); err == nil || !strings.Contains(err.Error(), "socks5") {
		t.Fatalf("an unsupported scheme was not refused clearly: %v", err)
	}
}

// An https:// upstream proxy is spoken to over TLS: the handshake is attempted
// (the test server's certificate is not trusted, so it fails as a certificate
// error, not as a plaintext CONNECT written into a TLS port).
func TestDialTunnelSpeaksTLSToAnHTTPSUpstream(t *testing.T) {
	// dialTunnel consults NO_PROXY (netroute.Exempt) before honoring `via`, so
	// the gate's hostile NO_PROXY='*' would dial example.test direct and the
	// TLS handshake under test would never happen; the test pins its env (#774).
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(s.Close)
	via, _ := url.Parse(s.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialTunnel(ctx, "example.test:443", via, nil)
	if err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("expected a TLS certificate error, got %v", err)
	}
}

// The proxy hop trusts the session's extra CAs, as the transport does: an
// https:// upstream signed by such a CA carries the CONNECT.
func TestDialTunnelTrustsTheSessionsCAsForAnHTTPSUpstream(t *testing.T) {
	// Same pin as the TLS test above: NO_PROXY='*' would dial direct past the
	// trusted-CA handshake this test is about (#774).
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.Close)
	via, _ := url.Parse(s.URL)
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialTunnel(ctx, "example.test:443", via, &tls.Config{RootCAs: pool})
	if err != nil {
		t.Fatalf("a proxy signed by a trusted session CA was refused: %v", err)
	}
	c.Close()
}

// A NO_PROXY host dials direct even when a proxy was given.
func TestDialTunnelDialsANoProxyHostDirect(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("NO_PROXY", "tunnel.test")
	via, _ := url.Parse("http://127.0.0.1:1") // nothing listens: a proxied dial would fail
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 127.0.0.1 is loopback, therefore exempt as well.
	c, err := dialTunnel(ctx, ln.Addr().String(), via, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// Bytes the client sends right behind CONNECT (its TLS hello) are not lost.
type coalesce struct {
	net.Conn
	br   *bufio.Reader
	held []byte
	n    int
	skip bool
}

func (c *coalesce) Write(b []byte) (int, error) {
	c.n++
	if c.n == 1 {
		c.held = append(c.held, b...)
		return len(b), nil
	}
	_, err := c.Conn.Write(append(c.held, b...))
	c.held = nil
	return len(b), err
}

func (c *coalesce) Read(b []byte) (int, error) {
	if !c.skip {
		for {
			line, err := c.br.ReadString('\n')
			if err != nil {
				return 0, err
			}
			if line == "\r\n" {
				break
			}
		}
		c.skip = true
	}
	return c.br.Read(b)
}

func TestMITMKeepsBytesBufferedBehindConnect(t *testing.T) {
	_, c, _, _, _ := harness(t)
	pu := c.Transport.(*http.Transport).Proxy
	req, _ := http.NewRequest("GET", "https://api.anthropic.com/", nil)
	purl, _ := pu(req)
	raw, err := net.Dial("tcp", purl.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	cc := &coalesce{Conn: raw, br: bufio.NewReader(raw)}
	cc.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"))
	tc := tls.Client(cc, &tls.Config{InsecureSkipVerify: true, ServerName: "api.anthropic.com"})
	if err := tc.Handshake(); err != nil {
		t.Fatalf("the hello sent behind CONNECT was lost: %v", err)
	}
}

func TestTheLeafIsReissuedNearExpiry(t *testing.T) {
	ca, err := LoadCA(filepath.Join(t.TempDir(), "ca"))
	if err != nil {
		t.Fatal(err)
	}
	p := &Proxy{ca: ca}
	first, err := p.leafFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := p.leafFor(nil); again != first {
		t.Fatal("a fresh leaf was re-minted")
	}
	old := *first.Leaf
	old.NotAfter = time.Now().Add(time.Hour)
	p.leaf = &tls.Certificate{Certificate: first.Certificate, PrivateKey: first.PrivateKey, Leaf: &old}
	renewed, err := p.leafFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(renewed.Leaf.NotAfter) < 20*24*time.Hour {
		t.Fatalf("a leaf within a day of expiry was served: %v", renewed.Leaf.NotAfter)
	}
}

func TestPicksInADirWithGlobMetacharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a[b]*c")
	if err := LeavePick(dir, "s1", "p", Pick{Word: "ds"}); err != nil {
		t.Fatal(err)
	}
	if !HasPending(dir, "s1") {
		t.Fatal("HasPending missed a pick in a dir holding glob metacharacters")
	}
}

// Parallel first requests of one agent all take its pick.
func TestParallelFirstRequestsOfOneAgentAllTakeItsPick(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Gw", "1")
	}))
	t.Cleanup(gw.Close)
	p, c := proxyWithPick(t, gw.URL, nil)
	for i := 0; i < 15; i++ {
		agent := "a" + itoa(i)
		if err := LeavePick(p.Dir, "s1", "do "+agent, Pick{Word: "ds"}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		missed := make(chan string, 2)
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := c.Do(inferenceReq(t, agent, "s1", `{"messages":[{"role":"user","content":"do `+agent+`"}]}`))
				if err != nil {
					missed <- err.Error()
					return
				}
				resp.Body.Close()
				if resp.Header.Get("X-Gw") == "" {
					missed <- "stayed on the session's route"
				}
			}()
		}
		wg.Wait()
		select {
		case m := <-missed:
			t.Fatalf("%s: %s", agent, m)
		default:
		}
	}
}

func TestPickModelIsResolvedThroughTiers(t *testing.T) {
	tiers := map[string]string{"opus": "gpt-6[1m]"}
	for in, want := range map[string]string{
		"opus": "gpt-6", "claude-opus-5-5": "gpt-6", "haiku": "haiku", "kimi-k3[1m]": "kimi-k3", "": "",
	} {
		if got := pickModel(in, tiers); got != want {
			t.Errorf("pickModel(%q)=%q want %q", in, got, want)
		}
	}
}

// A pick naming an endpoint the edge does not hold takes the route as it is:
// the pick's model is not forced onto the route.
func TestALostEndpointPickDoesNotForceItsModelOnTheRoute(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	if err := LeavePick(r.e.AgentsDir(), "s1", "Fix parse()", Pick{Word: "ghost", Model: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	r.post(t, msgs, "s1", "a1", "claude-opus-5-5", "Fix parse()")
	g := r.expect(t, r.route, "the route")
	if g.body["model"] == "kimi-k3" {
		t.Fatalf("the lost endpoint's model was forced: %v", g.body["model"])
	}
}
