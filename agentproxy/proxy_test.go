package agentproxy

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	auth, key, beta, agent string
	body                   map[string]any
}

func recorder(t *testing.T, tlsServer bool, org string) (*httptest.Server, chan seen) {
	t.Helper()
	got := make(chan seen, 4)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		got <- seen{r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Beta"), r.Header.Get(AgentHeader), body}
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
	// The proxy's outbound trust is the launcher's extra CA: the test server's.
	caFile := filepath.Join(t.TempDir(), "up.pem")
	os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: first.Certificate().Raw}), 0o600)
	t.Setenv("NODE_EXTRA_CA_CERTS", caFile)
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
	gwURL, _ := url.Parse(gw.URL)
	p, err := Start(filepath.Join(t.TempDir(), "agents"), ca, func(p Pick) (Target, error) {
		return Target{Base: gwURL, Key: "gw-key-" + p.Word}, nil
	}, t.Logf)
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
	if err := LeavePick(p.Dir, "  write the report \n", Pick{Word: "ds", Model: "deepseek-v5"}); err != nil {
		t.Fatal(err)
	}
	send(t, c, "agent1", "write the report")
	got := <-gw
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
