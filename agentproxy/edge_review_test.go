package agentproxy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var z bytes.Buffer
	zw := gzip.NewWriter(&z)
	zw.Write(b)
	zw.Close()
	return z.Bytes()
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// turnBody is one turn's body as Claude Code sends it (as edgeRig.post builds it).
func turnBody(model, prompt string) []byte {
	return []byte(`{"model":"` + model + `","thinking":{"display":"updates"},"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2; cch=abc;","cache_control":{"type":"ephemeral","scope":"global"}}],` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":` + quote(prompt) + `}]}]}`)
}

// --- a compressed inference body (review finding 1) ---------------------------

// A subagent's first turn sent gzip-compressed is read as its sender meant it:
// its prompt claims the pick waiting for it, though the shared word is the
// account, and the routed body is rewritten and sent compressed again.
func TestACompressedSubagentTurnIsClaimedByItsPick(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	if err := LeavePick(r.e.AgentsDir(), "s1", "Fix parse()", Pick{Word: "route", Model: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	z := gz(t, turnBody("claude-opus-5-5", "Fix parse()"))
	if code, _ := r.send(t, msgs, "s1", "a1", z, map[string]string{"Content-Encoding": "gzip"}); code != 200 {
		t.Fatalf("answered %d", code)
	}
	g := r.expect(t, r.route, "the pick's route")
	r.quiet(t, r.first, "the account")
	if g.encoding != "gzip" || g.key != "route-key" {
		t.Fatalf("%+v", g)
	}
	var body map[string]any
	if err := json.Unmarshal(gunzip(t, g.raw), &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "kimi-k3" || body["thinking"].(map[string]any)["display"] != "omitted" {
		t.Fatalf("the compressed body was not rewritten: %v", body)
	}
}

// A compressed body nothing changes goes on byte for byte as it came.
func TestAnUnchangedCompressedBodyGoesOnAsItCame(t *testing.T) {
	r := newEdgeRig(t, nil)
	z := gz(t, []byte(`{"model":"some-model","messages":[]}`))
	r.send(t, msgs, "s1", "", z, map[string]string{"Content-Encoding": "gzip"})
	if g := r.expect(t, r.route, "the route"); !bytes.Equal(g.raw, z) || g.encoding != "gzip" {
		t.Fatalf("an unchanged body was re-encoded: %q %q", g.encoding, g.raw)
	}
}

// A body the edge cannot decode while a pick waits for its session may name
// that pick: it goes nowhere, neither to the account nor to the route.
func TestAnUnreadableBodyWhileAPickWaitsGoesNowhere(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.word(t, "account")
	if err := LeavePick(r.e.AgentsDir(), "s1", "Fix parse()", Pick{Word: "route", Model: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	for name, enc := range map[string]string{"corrupt gzip": "gzip", "a coding the edge does not read": "br"} {
		if code, _ := r.send(t, msgs, "s1", "a1", []byte("\x1f\x8b not really"), map[string]string{"Content-Encoding": enc}); code != http.StatusBadGateway {
			t.Errorf("%s: answered %d", name, code)
		}
	}
	r.quiet(t, r.first, "the account")
	r.quiet(t, r.route, "the route")
}

// --- the fail-closed rule (review finding 2) ----------------------------------

// An inference body over the read cap is never buffered whole. Its routing
// decision stands all the same: a routed turn still goes to the route on the
// route's key (its body as it came), and a turn a waiting pick may claim goes
// nowhere.
func TestAnInferenceBodyOverTheCapKeepsItsRoutingDecision(t *testing.T) {
	old := inferenceMax
	inferenceMax = 500
	t.Cleanup(func() { inferenceMax = old })
	r := newEdgeRig(t, nil)
	big := turnBody("claude-opus-5-5", strings.Repeat("y", 1000))
	r.send(t, msgs, "s1", "", big, nil)
	g := r.expect(t, r.route, "the route")
	if !bytes.Equal(g.raw, big) || g.key != "route-key" || strings.Contains(g.auth, "ACCOUNT") {
		t.Fatalf("%+v", g)
	}
	r.word(t, "account")
	if err := LeavePick(r.e.AgentsDir(), "s1", "Fix parse()", Pick{Word: "route", Model: "kimi-k3"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := r.send(t, msgs, "s1", "a1", turnBody("claude-opus-5-5", "Fix parse()"+strings.Repeat(" ", 1000)), nil); code != http.StatusBadGateway {
		t.Fatalf("a turn a pick may claim answered %d", code)
	}
	r.quiet(t, r.first, "the account")
	r.quiet(t, r.route, "the route")
}

// An inference turn is refused Anthropic without the account word whatever
// path its endpoint puts in front of /v1/messages.
func TestAPrefixedFirstPartyBaseIsStillAnthropic(t *testing.T) {
	r := newEdgeRig(t, nil)
	r.e.Upstream, _ = url.Parse(firstParty.String() + "/gateway")
	if code, _ := r.post(t, msgs, "s1", "", "claude-opus-5-5", "x"); code != http.StatusBadGateway {
		t.Errorf("the route word on a prefixed Anthropic base answered %d", code)
	}
	r.e.Upstream, _ = url.Parse(r.otherU)
	h, _ := json.Marshal(map[string]any{"upstream": firstParty.String() + "/gateway/", "key": "gw-key", "tiers": map[string]string{}})
	write(t, filepath.Join(r.bridge, "switch-gw.json"), h)
	r.word(t, "gw")
	if code, _ := r.post(t, msgs, "s1", "", "claude-opus-5-5", "x"); code != http.StatusBadGateway {
		t.Errorf("a profile on a prefixed Anthropic base answered %d", code)
	}
	r.quiet(t, r.first, "Anthropic")
}

// --- the CA a restarted edge serves with (review finding 3) -------------------

// legacyCA writes the combined PEM a replaced proxy kept, valid until notAfter.
func legacyCA(t *testing.T, notAfter time.Time) (path string, certPEM []byte) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "mitmproxy"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pk8, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pk8})
	path = filepath.Join(t.TempDir(), "mitmproxy-ca.pem")
	write(t, path, append(keyPEM, certPEM...))
	return path, certPEM
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logLines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// A legacy CA that exists but cannot be adopted stops the edge with the reason
// and the fix; it is never replaced by a new root running sessions reject.
func TestALegacyCAThatCannotBeAdoptedIsAnError(t *testing.T) {
	expired, _ := legacyCA(t, time.Now().Add(-time.Minute))
	garbage := filepath.Join(t.TempDir(), "mitmproxy-ca.pem")
	write(t, garbage, []byte("not a PEM"))
	unreadable := t.TempDir() // a directory where the file should be
	for name, legacy := range map[string]string{"garbage": garbage, "unreadable": unreadable, "expired": expired} {
		dir := filepath.Join(t.TempDir(), "edge-ca")
		ca, err := LoadEdgeCA(dir, legacy, t.Logf)
		if err == nil {
			t.Errorf("%s: a new CA was made in its place", name)
			continue
		}
		if ca != nil || !strings.Contains(err.Error(), legacy) || !strings.Contains(err.Error(), "remove") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(dir, caCertFile)); err == nil {
			t.Errorf("%s: a CA was left in %s", name, dir)
		}
	}
}

// A legacy CA still valid but near its end is adopted and kept, and the log
// says when it ends and what to do before then.
func TestALegacyCANearItsEndIsKeptAndSaidSo(t *testing.T) {
	legacy, certPEM := legacyCA(t, time.Now().AddDate(0, 0, 10))
	dir := filepath.Join(t.TempDir(), "edge-ca")
	for i := 0; i < 2; i++ { // the first start adopts it, the next one keeps it
		var log logLines
		ca, err := LoadEdgeCA(dir, legacy, log.logf)
		if err != nil || !bytes.Equal(ca.PEM, certPEM) {
			t.Fatalf("start %d: the legacy CA was not kept: %v", i, err)
		}
		if !strings.Contains(log.String(), "expires") || !strings.Contains(log.String(), legacy) {
			t.Fatalf("start %d: the log does not say so: %q", i, log.String())
		}
	}
}

// With no legacy CA at all the edge makes its own, silently.
func TestNoLegacyCAMakesTheEdgesOwn(t *testing.T) {
	var log logLines
	dir := filepath.Join(t.TempDir(), "edge-ca")
	ca, err := LoadEdgeCA(dir, filepath.Join(t.TempDir(), "absent.pem"), log.logf)
	if err != nil || ca == nil || log.String() != "" {
		t.Fatalf("%v %q", err, log.String())
	}
	if again, err := LoadEdgeCA(dir, "", log.logf); err != nil || !bytes.Equal(again.PEM, ca.PEM) {
		t.Fatalf("the edge's own CA was not kept: %v", err)
	}
}

// --- held.<pid>.json (review finding 4) ----------------------------------------

// Every publication of held.<pid>.json is one step: a snapshot taken before a
// take never lands over the take's own record.
func TestAnOlderHeldSnapshotNeverLandsOverATake(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	write(t, filepath.Join(bridge, "switch-gw.json"), handoff("http://127.0.0.1:54505", "k", "h1", 500, "start-500"))
	paused, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.testHook = func(stage string) {
		if stage == "held-snapshot" {
			once.Do(func() { close(paused); <-release }) // the first publication, its snapshot empty
		}
	}
	first, took := make(chan struct{}), make(chan struct{})
	go func() { e.PublishHeld(); close(first) }()
	<-paused
	go func() { e.Take("gw"); close(took) }()
	select { // the take's publication must wait for the one in progress
	case <-took:
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-first
	<-took
	if got := heldProfiles(t, bridge, 500); got["gw"] != "h1" {
		t.Fatalf("an older snapshot landed over the take: %v", got)
	}
}

// A shutdown waits for a take in progress, so its key never stays on disk in a
// taken.* file, and nothing is published after the record is forgotten.
func TestAShutdownWaitsForATakeAndLeavesNothingBehind(t *testing.T) {
	e, bridge := proxyOf(t, 500, "start-500")
	if err := e.PublishHeld(); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bridge, "switch-gw.json"), handoff("http://127.0.0.1:54505", "k", "h1", 500, "start-500"))
	moved, release := make(chan struct{}), make(chan struct{})
	e.testHook = func(stage string) {
		if stage == "moved" { // the key is on disk under taken.*
			close(moved)
			<-release
		}
	}
	took, stopped := make(chan struct{}), make(chan struct{})
	go func() { e.Take("gw"); close(took) }()
	<-moved
	go func() { e.Stop(); e.ForgetHeld(); close(stopped) }() // EdgeMain's last steps
	select {
	case <-stopped:
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	<-took
	<-stopped
	none(t, bridge, "taken.*")
	none(t, bridge, "held.*")
	e.testHook = nil
	write(t, filepath.Join(bridge, "switch-oli.json"), handoff("http://127.0.0.1:54506", "k", "h2", 500, "start-500"))
	e.Take("oli")
	e.PublishHeld()
	none(t, bridge, "held.*")
	if _, err := os.Stat(filepath.Join(bridge, "switch-oli.json")); err != nil {
		t.Fatal("a stopped proxy took a handoff")
	}
}

// --- event uploads under a budget (review findings 6 and 7) --------------------

func slimmable() []byte {
	return []byte(`{"events":[{"payload":{"type":"assistant","message":{"content":[{"type":"thinking","signature":"` +
		strings.Repeat("S", 3000) + `"}]}}}]}`)
}

// An events upload over the cap, on the wire or once decoded, is never read
// whole: it goes on untouched.
func TestAnEventsUploadOverTheCapGoesOnUntouched(t *testing.T) {
	old := eventsMax
	eventsMax = 2000
	t.Cleanup(func() { eventsMax = old })
	r := newEdgeRig(t, nil)
	path := "/v1/code/sessions/cse_01Ab/worker/events"
	body := slimmable()
	r.send(t, path, "s1", "", body, nil)
	if g := r.expect(t, r.first, "Anthropic"); !bytes.Equal(g.raw, body) {
		t.Fatalf("an upload over the cap was changed: %d of %d bytes", len(g.raw), len(body))
	}
	z := gz(t, body) // small on the wire, over the cap inflated
	if len(z) >= int(eventsMax) {
		t.Fatal("the test body compresses too little")
	}
	r.send(t, path, "s1", "", z, map[string]string{"Content-Encoding": "gzip"})
	if g := r.expect(t, r.first, "Anthropic"); !bytes.Equal(g.raw, z) || g.encoding != "gzip" {
		t.Fatalf("an upload over the cap inflated was changed: %q %d of %d bytes", g.encoding, len(g.raw), len(z))
	}
	r.quiet(t, r.route, "the route")
}

// Image work is bounded: at most len(imageSlots) sips run at once, a wait for
// a slot ends with the upload's budget, and a sips that overruns it is killed.
func TestImageWorkIsBoundedInTimeAndConcurrency(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("images are shrunk with macOS sips")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	fake := filepath.Join(dir, "sips")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ntouch "+marker+"\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sips
	sips = fake
	t.Cleanup(func() { sips = old })
	data := base64.StdEncoding.EncodeToString([]byte("an image"))

	for i := 0; i < cap(imageSlots); i++ { // every slot taken
		imageSlots <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, _, ok := sipsShrink(ctx, data, "image/png")
	cancel()
	for i := 0; i < cap(imageSlots); i++ {
		<-imageSlots
	}
	if ok {
		t.Fatal("an image was shrunk without a slot")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("sips ran with every slot taken")
	}

	start := time.Now()
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, ok := sipsShrink(ctx, data, "image/png"); ok {
		t.Fatal("an overrun sips counted as a shrink")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("sips never ran")
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("an overrun sips held the upload %v", took)
	}
}

// --- other hosts at the edge ---------------------------------------------------

// A CONNECT to any host but api.anthropic.com:443 is an opaque tunnel at the
// edge too: the proxy never terminates its TLS, never reads its bytes, and a
// path that would be inference on Anthropic is nothing to it.
func TestAnotherHostIsAnOpaqueTunnelAtTheEdge(t *testing.T) {
	r := newEdgeRig(t, nil)
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "tunnelled") }))
	t.Cleanup(other.Close)
	pool := x509.NewCertPool()
	pool.AddCert(other.Certificate())
	proxyURL, _ := url.Parse("http://127.0.0.1:" + itoa(r.p.Port()))
	c := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := c.Post(other.URL+"/v1/messages", "application/json", bytes.NewReader(turnBody("claude-opus-5-5", "x")))
	if err != nil {
		t.Fatalf("the tunnel was decrypted: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.TLS == nil || !resp.TLS.PeerCertificates[0].Equal(other.Certificate()) || string(b) != "tunnelled" {
		t.Fatalf("the proxy answered for the host: %q", b)
	}

	// Raw bytes, not even TLS, come back as they went.
	echo, _ := net.Listen("tcp4", "127.0.0.1:0")
	t.Cleanup(func() { echo.Close() })
	go func() {
		if c, err := echo.Accept(); err == nil {
			io.Copy(c, c)
			c.Close()
		}
	}()
	conn, err := net.Dial("tcp4", proxyURL.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", echo.Addr(), echo.Addr())
	br := bufio.NewReader(conn)
	cr, err := http.ReadResponse(br, nil)
	if err != nil || cr.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", cr, err)
	}
	const raw = "\x16\x03\x01 not a handshake, never read\n"
	io.WriteString(conn, raw)
	got := make([]byte, len(raw))
	if _, err := io.ReadFull(br, got); err != nil || string(got) != raw {
		t.Fatalf("the tunnel changed the bytes: %q %v", got, err)
	}
	r.quiet(t, r.first, "Anthropic")
	r.quiet(t, r.route, "the route")
	if strings.Contains(r.log.String(), "/v1/messages") {
		t.Fatalf("the proxy read a tunnelled request:\n%s", r.log)
	}
}

// --- readiness (review finding 8) ----------------------------------------------

// A proxy that cannot bind its port never says it is ready: an old proxy on
// that port answers it, and only the READY line tells the new one apart.
func TestEdgeProxyNeverReportsReadyOnAPortItCouldNotBind(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0") // the old proxy, still on the fixed port
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	state := t.TempDir()
	keyFile := filepath.Join(state, "route-key.1.abc")
	write(t, keyFile, []byte("sk"))
	t.Setenv("RC_UPSTREAM", "http://127.0.0.1:9")
	t.Setenv("RC_KEY_FILE", keyFile)
	t.Setenv("RC_INFERENCE", filepath.Join(state, "inference"))
	var out, errs bytes.Buffer
	code := EdgeMain([]string{"--listen", held.Addr().String(), "--ca-dir", filepath.Join(state, "ca")}, &out, &errs)
	if code == 0 || strings.Contains(out.String(), "READY") {
		t.Fatalf("exit %d, stdout %q", code, out.String())
	}
	if !strings.Contains(errs.String(), "address already in use") {
		t.Fatalf("the reason is not said: %q", errs.String())
	}
}
