package agentproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akleizor/ccmorph/netroute"
)

// Options is one session's proxy: fixed for its life, so a session that
// ends takes its proxy with it and the next one starts its own.
type Options struct {
	// Dir holds the picks (the protocol in picks.go); Close removes it.
	Dir string
	// Resolve is where a pick is served; an error leaves the request on the
	// session's own route.
	Resolve func(Pick) (Target, error)
	// Logf records a routing decision; never a credential.
	Logf func(format string, args ...any)
	// Origin is where a request that names no host goes: the session's own
	// base URL, path included, when it is not first party. nil for an
	// account session.
	Origin *url.URL
	// Upstream is the proxy the session itself was given (its HTTPS_PROXY),
	// which every outbound call and tunnel takes; nil dials directly.
	Upstream *url.URL
	// TLS is the trust outbound calls use: the system's plus the session's
	// own extra CA. nil is Go's default.
	TLS *tls.Config
	// Listen is the address to serve on; "" is a free loopback port.
	Listen string
	// Edge makes this the Remote Control edge's proxy: every inference request
	// is decided by its switch words (edge.go) and Dir is the edge's own agents
	// folder, which Close leaves where it is.
	Edge *Edge
}

// Proxy is one session's agent proxy.
type Proxy struct {
	Options

	ca     *CA
	leafMu sync.Mutex
	leaf   *tls.Certificate
	ln     net.Listener
	srv    *http.Server
	rp     *httputil.ReverseProxy
}

// firstParty is where a decrypted api.anthropic.com request goes; a test
// points it at its own server.
var firstParty = &url.URL{Scheme: "https", Host: anthropicHost}

type ctxKey struct{}

type decision struct {
	url    *url.URL
	target *Target
	pick   Pick
	body   []byte
	// The launcher's read of body: decoded, and the coding it came in.
	plain  []byte
	coding coding
	// The edge's decision about the request, and the address the connection
	// that answered was dialled for ("" when none can be read).
	edge   *edgeState
	dialed string
}

// dialedConn is a connection that remembers the address it was dialled for:
// the evidence of which endpoint answered (verdict).
type dialedConn struct {
	net.Conn
	addr string
}

// dialedAddr is the address conn was dialled for, through the TLS layer a
// connection to an https endpoint wears; "" when it is not one this proxy
// dialled to its destination.
func dialedAddr(conn net.Conn) string {
	if tc, ok := conn.(*tls.Conn); ok {
		conn = tc.NetConn()
	}
	if dc, ok := conn.(*dialedConn); ok {
		return dc.addr
	}
	return ""
}

// Start listens on a loopback port and serves until Close.
func Start(ca *CA, o Options) (*Proxy, error) {
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	dir := o.Dir
	leaf, err := ca.Leaf(anthropicHost)
	if err != nil {
		return nil, err
	}
	if dir != "" {
		if err := os.MkdirAll(filepath.Join(dir, "pending"), 0o700); err != nil {
			return nil, err
		}
	}
	listen := o.Listen
	if listen == "" {
		listen = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp4", listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{Options: o, ca: ca, leaf: leaf, ln: ln}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = func(r *http.Request) (*url.URL, error) {
		if o.Upstream == nil {
			return nil, nil
		}
		return netroute.Proxy(o.Upstream, r)
	}
	tr.TLSClientConfig = o.TLS
	tr.ForceAttemptHTTP2 = true
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &dialedConn{Conn: c, addr: addr}, nil
	}
	p.rp = &httputil.ReverseProxy{
		Transport:     failClosed{tr},
		FlushInterval: -1, // SSE passes through live
		Rewrite: func(pr *httputil.ProxyRequest) {
			d := pr.In.Context().Value(ctxKey{}).(*decision)
			pr.Out.URL = d.url
			pr.Out.Host = d.url.Host
			if d.target != nil {
				d.target.setAuth(pr.Out.Header)
			}
			if d.body != nil {
				pr.Out.Body = io.NopCloser(bytes.NewReader(d.body))
				pr.Out.ContentLength = int64(len(d.body))
				pr.Out.Header.Set("Content-Length", strconv.Itoa(len(d.body)))
			}
		},
		ModifyResponse: func(r *http.Response) error {
			d := r.Request.Context().Value(ctxKey{}).(*decision)
			if d.edge != nil {
				if d.edge.asked != "" {
					host, port := hostPort(r.Request.URL)
					miss, line := judge(d.edge, r.StatusCode, r.Request.URL.Path,
						r.Header.Get("anthropic-organization-id") != "", host, port, d.dialed)
					p.Edge.logf("%s", line)
					p.Edge.CountServed(miss, line) // kept without a debug log (GH-977)
				}
				return nil
			}
			if d.target != nil {
				served := r.Request.URL.Host
				if r.Header.Get("anthropic-organization-id") != "" && !d.target.FirstParty() {
					served = "ANTHROPIC (not routed!)"
				}
				p.Logf("agent proxy: <- %d %s via %s served-by=%s", r.StatusCode, r.Request.URL.Path, d.pick.Word, served)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.Logf("agent proxy: %s %s: %v", r.Method, r.URL.Host, err)
			writeAPIError(w, http.StatusBadGateway, "agent proxy: "+r.URL.Host+": "+err.Error())
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	return p, nil
}

// writeAPIError answers in the shape Anthropic's API does, so the client shows
// the cause instead of a bare status.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

// leafFor is the certificate served for api.anthropic.com: minted on first use
// and again when it is within a day of expiring, so a long run never serves a
// stale one.
func (p *Proxy) leafFor(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.leafMu.Lock()
	defer p.leafMu.Unlock()
	if p.leaf != nil && p.leaf.Leaf != nil && time.Until(p.leaf.Leaf.NotAfter) > 24*time.Hour {
		return p.leaf, nil
	}
	leaf, err := p.ca.Leaf(anthropicHost)
	if err != nil {
		return nil, err
	}
	p.leaf = leaf
	return leaf, nil
}

// Port is the loopback port the proxy listens on.
func (p *Proxy) Port() int { return p.ln.Addr().(*net.TCPAddr).Port }

// Close stops the proxy and removes its pick dir.
func (p *Proxy) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.srv.Shutdown(ctx)
	p.srv.Close()
	if p.Edge == nil { // an edge's agents folder is the router's, and outlives the proxy
		os.RemoveAll(p.Dir)
	}
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	u := *r.URL
	switch {
	case u.IsAbs(): // a plain-HTTP forward-proxy request
	case p.Origin != nil:
		u = *join(p.Origin, r.URL)
	default:
		http.Error(w, "agent proxy: no origin for "+r.URL.Path, http.StatusBadGateway)
		return
	}
	p.forward(w, r, &u)
}

// forward sends one request on: to the agent's pick when its agent has one,
// else where it was going. At the edge every inference request is decided by
// its switch words instead (edge.go).
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, u *url.URL) {
	d := &decision{url: u}
	if p.Edge != nil {
		defer func() {
			if v := recover(); v != nil { // a decision that fails closes the request
				p.Logf("agent proxy: %s %s: %v", r.Method, r.URL.Path, v)
				w.WriteHeader(http.StatusBadGateway)
			}
		}()
		if err := p.Edge.decide(r, u, d); err != nil {
			p.Logf("agent proxy: %s %s: %v", r.Method, u.Path, err)
			w.WriteHeader(http.StatusBadGateway)
			return
		}
	} else if agent := r.Header.Get(AgentHeader); agent != "" && isInference(strings.TrimPrefix(u.Path, originPath(p.Origin))) {
		session := r.Header.Get(SessionHeader)
		pick, ok := Bound(p.Dir, agent)
		if !ok && HasPending(p.Dir, session) {
			// A pick waits for this agent: a body that cannot be read goes
			// nowhere, rather than to the session's route with the pick unmet.
			if err := d.read(r); err != nil {
				p.Logf("agent proxy: %s %s: %v", r.Method, u.Path, err)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			pick, ok = Claim(p.Dir, session, agent, d.plain, time.Now())
		}
		if !ok {
			// A parallel first request of this agent may have claimed the pick
			// between the check above and the claim.
			pick, ok = Bound(p.Dir, agent)
		}
		if ok {
			if err := p.route(r, d, pick); err != nil {
				p.Logf("agent proxy: %s %s: %v", r.Method, u.Path, err)
				writeAPIError(w, http.StatusBadGateway, "agent proxy: the request body cannot be read: "+err.Error())
				return
			}
		}
	}
	ctx := context.WithValue(r.Context(), ctxKey{}, d)
	if d.edge != nil {
		// Through the session's own proxy the connection is the proxy's, which
		// proves nothing about the endpoint: no evidence is recorded then.
		direct := p.Upstream == nil || netroute.Exempt(d.url)
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(i httptrace.GotConnInfo) {
				if direct {
					d.dialed = dialedAddr(i.Conn)
				}
			}})
	}
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}

// failClosed is the transport's last word on the edge's decision: an inference
// request addressed to Anthropic itself goes only under the account word.
type failClosed struct{ next http.RoundTripper }

func (f failClosed) RoundTrip(r *http.Request) (*http.Response, error) {
	if d, ok := r.Context().Value(ctxKey{}).(*decision); ok {
		if err := refuseFirstParty(r.URL, d); err != nil {
			return nil, err
		}
	}
	return f.next.RoundTrip(r)
}

// read buffers the request body as it came (body) and decoded (plain).
func (d *decision) read(r *http.Request) error {
	wire, whole, err := readUpTo(r, inferenceMax)
	if err != nil {
		return err
	}
	if !whole { // r.Body still sends every byte as it came
		return errTooLarge
	}
	d.body = wire
	d.plain, d.coding, err = decodeBody(wire, r.Header, inferenceMax)
	return err
}

// route points the request at pick's target. An error means the body could not
// be read or decoded: the request is answered 502, as an unread pending pick is,
// never sent to the session's route with a bound agent's history.
func (p *Proxy) route(r *http.Request, d *decision, pick Pick) error {
	t, err := p.Resolve(pick)
	if err != nil {
		p.Logf("agent proxy: %s cannot be served (%v); the agent stays on the session's route", pick.Word, err)
		return nil
	}
	if d.plain == nil {
		if err := d.read(r); err != nil {
			return err
		}
	}
	// The body is read decoded and goes on in the coding it came in: byte for
	// byte when nothing changed.
	if out := rewriteBody(d.plain, pick.Model, t); !bytes.Equal(out, d.plain) {
		d.body = encodeBody(out, d.coding)
	}
	out := join(t.Base, &url.URL{Path: strings.TrimPrefix(d.url.Path, originPath(p.Origin)), RawQuery: d.url.RawQuery})
	d.url, d.target, d.pick = out, &t, pick
	p.Logf("agent proxy: route %s %s via %s", r.Method, out.Path, pick.Word)
	return nil
}

// join is the request's path and query under base: a base with a path
// (https://host/anthropic) keeps it in front of /v1/messages.
func join(base, req *url.URL) *url.URL {
	u := *base
	u.Path = strings.TrimSuffix(base.Path, "/") + req.Path
	u.RawPath = ""
	u.RawQuery = req.RawQuery
	return &u
}

// originPath is the session's own base path, which a routed request leaves
// behind for its target's.
func originPath(o *url.URL) string {
	if o == nil {
		return ""
	}
	return strings.TrimSuffix(o.Path, "/")
}

func isInference(path string) bool {
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
}

// isLoopbackHost reports whether a request target (a bare host, as
// url.URL.Hostname gives it) names this machine. The adapters ccmorph
// starts for picked profiles are served on loopback, and the session's
// upstream proxy cannot serve them: a real proxy refuses the absolute URI
// or resolves it against its own host, so those dial direct.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// connect answers CONNECT: api.anthropic.com is served here over TLS with the
// CA's certificate; any other host is a tunnel whose bytes are never read.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host, port = r.Host, "443"
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if strings.EqualFold(strings.TrimSuffix(host, "."), anthropicHost) && port == "443" {
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		// Bytes the client sent behind the CONNECT are in buf, not the socket.
		if buf.Reader.Buffered() > 0 {
			conn = &bufConn{Conn: conn, r: buf.Reader}
		}
		p.serveTLS(conn)
		return
	}
	up, err := dialTunnel(r.Context(), r.Host, p.Upstream, p.TLS)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() {
		if n := buf.Reader.Buffered(); n > 0 {
			b, _ := buf.Reader.Peek(n)
			up.Write(b)
		}
		io.Copy(up, conn)
		closeWrite(up)
	}()
	io.Copy(conn, up)
	closeWrite(conn)
	conn.Close()
	up.Close()
}

func closeWrite(c net.Conn) {
	if t, ok := c.(interface{ CloseWrite() error }); ok {
		t.CloseWrite()
	}
}

// serveTLS serves the HTTP requests a client sends inside one decrypted
// CONNECT to api.anthropic.com.
func (p *Proxy) serveTLS(raw net.Conn) {
	conn := tls.Server(raw, &tls.Config{GetCertificate: p.leafFor, NextProtos: []string{"http/1.1"}})
	raw.SetDeadline(time.Now().Add(30 * time.Second))
	if conn.Handshake() != nil {
		conn.Close()
		return
	}
	raw.SetDeadline(time.Time{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := *r.URL
			u.Scheme, u.Host = firstParty.Scheme, firstParty.Host
			p.forward(w, r, &u)
		}),
		ReadHeaderTimeout: 5 * time.Minute,
	}
	srv.Serve(&oneConn{conn: conn})
}

// oneConn is a listener that hands out one connection and then ends when that
// connection closes.
type oneConn struct {
	conn net.Conn
	once sync.Once
	done chan struct{}
	mu   sync.Mutex
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	done := l.done
	l.mu.Unlock()
	var c net.Conn
	l.once.Do(func() { c = &closeConn{Conn: l.conn, done: done} })
	if c != nil {
		return c, nil
	}
	<-done
	return nil, net.ErrClosed
}
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Addr() net.Addr { return l.conn.LocalAddr() }

type closeConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *closeConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { close(c.done) })
	return err
}

// dialTunnel reaches addr for a tunnel, through via when the session was
// given a proxy.
func dialTunnel(ctx context.Context, addr string, via *url.URL, trust *tls.Config) (net.Conn, error) {
	var d net.Dialer
	if via != nil && netroute.Exempt(&url.URL{Scheme: "https", Host: addr}) {
		via = nil // NO_PROXY and loopback dial direct, as the transport does
	}
	if via == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	hostport, secure, err := hop(via)
	if err != nil {
		return nil, err
	}
	c, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return nil, err
	}
	if secure {
		// The proxy hop trusts what the transport's does: the system's roots
		// plus the session's extra CAs.
		cfg := &tls.Config{}
		if trust != nil {
			cfg = trust.Clone()
		}
		cfg.ServerName = via.Hostname()
		tc := tls.Client(c, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: http.Header{}}
	if u := via.User; u != nil {
		pw, _ := u.Password()
		req.SetBasicAuth(u.Username(), pw)
		req.Header.Set("Proxy-Authorization", req.Header.Get("Authorization"))
		req.Header.Del("Authorization")
	}
	if err := req.Write(c); err != nil {
		c.Close()
		return nil, err
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		c.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		c.Close()
		return nil, &net.OpError{Op: "connect", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	if br.Buffered() > 0 {
		return &bufConn{Conn: c, r: br}, nil
	}
	return c, nil
}

// hop is where a tunnel's upstream proxy is dialled: its address with the
// scheme's default port when the URL names none, and whether it speaks TLS.
func hop(via *url.URL) (hostport string, secure bool, err error) {
	hostport = via.Host
	port := ""
	switch strings.ToLower(via.Scheme) {
	case "http":
		port = "80"
	case "https":
		secure, port = true, "443"
	default:
		return "", false, fmt.Errorf("upstream proxy scheme %q is not supported (http or https)", via.Scheme)
	}
	if via.Port() == "" {
		hostport = net.JoinHostPort(via.Hostname(), port)
	}
	return hostport, secure, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
