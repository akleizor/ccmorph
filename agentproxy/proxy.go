package agentproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
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

// Proxy is one session's agent proxy.
type Proxy struct {
	// Dir holds the picks (the protocol in picks.go); Close removes it.
	Dir string
	// Resolve is where a pick is served; an error leaves the request on the
	// session's own route.
	Resolve func(Pick) (Target, error)
	// Logf records a routing decision; never a credential.
	Logf func(format string, args ...any)
	// Origin is where a request that names no host goes: the session's own
	// base URL when it is not first party. nil for an account session.
	Origin *url.URL

	ca   *CA
	leaf *tls.Certificate
	ln   net.Listener
	srv  *http.Server
	rp   *httputil.ReverseProxy
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
}

// Start listens on a loopback port and serves until Close.
func Start(dir string, ca *CA, resolve func(Pick) (Target, error), logf func(string, ...any)) (*Proxy, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	leaf, err := ca.Leaf(anthropicHost)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "pending"), 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &Proxy{Dir: dir, Resolve: resolve, Logf: logf, ca: ca, leaf: leaf, ln: ln}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment     // the launcher's own: the user's proxy, never this one
	tr.TLSClientConfig = netroute.ExtraCAs() // the system's trust and the launcher's extra CA
	tr.ForceAttemptHTTP2 = true
	p.rp = &httputil.ReverseProxy{
		Transport:     tr,
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
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	p.srv = &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	go p.srv.Serve(ln)
	return p, nil
}

// Port is the loopback port the proxy listens on.
func (p *Proxy) Port() int { return p.ln.Addr().(*net.TCPAddr).Port }

// Close stops the proxy and removes its pick dir.
func (p *Proxy) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p.srv.Shutdown(ctx)
	p.srv.Close()
	os.RemoveAll(p.Dir)
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
		u.Scheme, u.Host = p.Origin.Scheme, p.Origin.Host
	default:
		http.Error(w, "agent proxy: no origin for "+r.URL.Path, http.StatusBadGateway)
		return
	}
	p.forward(w, r, &u)
}

// forward sends one request on: to the agent's pick when its agent has one,
// else where it was going.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, u *url.URL) {
	d := &decision{url: u}
	if agent := r.Header.Get(AgentHeader); agent != "" && isInference(u.Path) {
		pick, ok := Bound(p.Dir, agent)
		if !ok && HasPending(p.Dir) {
			body, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			d.body = body
			pick, ok = Claim(p.Dir, agent, body, time.Now())
		}
		if ok {
			p.route(r, d, pick)
		}
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, d)))
}

func (p *Proxy) route(r *http.Request, d *decision, pick Pick) {
	t, err := p.Resolve(pick)
	if err != nil {
		p.Logf("agent proxy: %s cannot be served (%v); the agent stays on the session's route", pick.Word, err)
		return
	}
	if d.body == nil {
		body, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return
		}
		d.body = body
	}
	d.body = rewriteBody(d.body, pick.Model, t)
	out := *d.url
	out.Scheme, out.Host = t.Base.Scheme, t.Base.Host
	d.url, d.target, d.pick = &out, &t, pick
	p.Logf("agent proxy: route %s %s via %s", r.Method, out.Path, pick.Word)
}

func isInference(path string) bool {
	return path == "/v1/messages" || strings.HasPrefix(path, "/v1/messages/")
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
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		p.serveTLS(conn)
		return
	}
	up, err := dialTunnel(r.Context(), r.Host)
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
	conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{*p.leaf}, NextProtos: []string{"http/1.1"}})
	if conn.Handshake() != nil {
		conn.Close()
		return
	}
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

// dialTunnel reaches addr for a tunnel, through the launcher's own HTTPS
// proxy when it names one.
func dialTunnel(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	via, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
	if err != nil || via == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	c, err := d.DialContext(ctx, "tcp", via.Host)
	if err != nil {
		return nil, err
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

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
