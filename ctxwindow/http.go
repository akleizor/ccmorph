package ctxwindow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/akleizor/ccmorph/pycompat"
)

const (
	// OvershootChars is what an opt-in probe sends: characters, NOT a token count.
	OvershootChars = 6_000_000
	probeTimeout   = 10
	listTimeout    = 5
	userAgent      = "ccmorph-context-probe"
	runTimeout     = 30
)

// Request is one HTTP exchange.
type Request struct {
	Method, URL string
	Header      [][2]string
	Body        []byte
}

// HTTPError is urllib.error.HTTPError: a response with a status of 400 or more.
type HTTPError struct {
	Code int
	Body []byte
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP Error %d", e.Code) }

// URLError is urllib.error.URLError: the request never produced a response.
type URLError struct{ Reason error }

func (e *URLError) Error() string { return "<urlopen error " + reasonText(e.Reason) + ">" }
func (e *URLError) Unwrap() error { return e.Reason }

func reasonText(err error) string {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return pycompat.OSErrorText(errno)
	}
	return err.Error()
}

// crossOriginError is a refused redirect; urllib raises it as a URLError whose
// reason is text, which asStr() shows inside <urlopen error ...>.
type crossOriginError struct{ msg string }

func (e *crossOriginError) Error() string { return e.msg }

// Opener performs requests; tests substitute their own.
type Opener interface {
	Open(ctx context.Context, req Request, timeout time.Duration) ([]byte, error)
}

type sameOriginClient struct{ client *http.Client }

func origin(u *url.URL) string { return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) }

// BuildOpener never carries the credential to another origin: Go, like urllib,
// copies request headers (x-api-key included) onto a redirect, so a cross-origin
// redirect fails as a transport error and a same-origin one is followed. It
// reads HTTPS_PROXY/NO_PROXY from the environment, so an ambient proxy exported
// by whatever bridge the operator runs reaches the probe unchanged.
func BuildOpener() Opener {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = http.ProxyFromEnvironment
	return &sameOriginClient{&http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if origin(req.URL) != origin(via[0].URL) {
				return &crossOriginError{fmt.Sprintf("cross-origin redirect refused: %s -> %s", via[len(via)-1].URL, req.URL)}
			}
			return nil
		},
	}}
}

// progress resets the idle timer whenever bytes move, so the timeout bounds
// each wait on the socket (connect, first byte, every read) the way urllib's
// timeout did, not the whole exchange. The run's own deadline still arrives
// through ctx and bounds the total.
type progress struct {
	io.ReadCloser
	timer *time.Timer
	idle  time.Duration
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.ReadCloser.Read(b)
	if n > 0 {
		p.timer.Reset(p.idle)
	}
	return n, err
}

func (c *sameOriginClient) Open(ctx context.Context, r Request, timeout time.Duration) ([]byte, error) {
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idle atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		idle.Store(true)
		cancel()
	})
	defer timer.Stop()
	var body io.Reader
	if r.Body != nil {
		body = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(rctx, r.Method, r.URL, body)
	if err != nil {
		return nil, &URLError{err}
	}
	if r.Body != nil {
		// A slow upload is a slow socket operation too. The wrapper keeps
		// ContentLength set, so the request is never sent chunked.
		req.Body = &progress{io.NopCloser(bytes.NewReader(r.Body)), timer, timeout}
	}
	for _, h := range r.Header {
		req.Header.Set(h[0], h[1])
	}
	resp, err := c.client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if idle.Load() {
			err = context.DeadlineExceeded
		}
		return nil, &URLError{err}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(&progress{resp.Body, timer, timeout})
	if err != nil {
		if idle.Load() {
			err = context.DeadlineExceeded
		}
		return nil, &URLError{err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{resp.StatusCode, data}
	}
	return data, nil
}

// defaultOpener is the opener a nil Opener argument means; tests replace it.
var defaultOpener Opener = BuildOpener()

func pick(o Opener) Opener {
	if o != nil {
		return o
	}
	return defaultOpener
}

// IsTimeout reports a timeout however it is wrapped: the whole safety property
// (never read the clock as a ceiling) rests on recognising it.
func IsTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, os.ErrDeadlineExceeded)
}

// WhyUnreachable words an error as something a reader can act on: an exception
// class is what the code caught, never what went wrong.
func WhyUnreachable(err error, base string) string {
	host := base
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	if host == "" {
		host = base
	}
	var he *HTTPError
	if errors.As(err, &he) {
		if he.Code == 401 || he.Code == 403 {
			return fmt.Sprintf("%s: model listing denied (HTTP %d); check credentials and gateway access rules", host, he.Code)
		}
		return fmt.Sprintf("%s: model listing failed (HTTP %d)", host, he.Code)
	}
	// URLError wraps the real cause, sometimes more than once; the useful
	// sentence is in the innermost one.
	cause := err
	for {
		var ue *URLError
		if !errors.As(cause, &ue) || ue.Reason == nil {
			break
		}
		cause = ue.Reason
	}
	var dns *net.DNSError
	var errno syscall.Errno
	var cross *crossOriginError
	var detail string
	switch {
	case errors.As(cause, &cross):
		detail = fmt.Sprintf("did not answer at %s (<urlopen error %s>)", host, cross.msg)
	case IsTimeout(cause):
		detail = fmt.Sprintf("did not answer at %s (timed out)", host)
	case errors.As(cause, &dns):
		detail = fmt.Sprintf("could not resolve %s (DNS)", host)
	case errors.As(cause, &errno):
		text := pycompat.OSErrorText(errno)
		text = text[strings.Index(text, "] ")+2:]
		detail = fmt.Sprintf("did not answer at %s (%s)", host, text)
	default:
		detail = fmt.Sprintf("could not be reached at %s (%s)", host, cause)
	}
	return detail + "; check the endpoint and network access"
}

var sdkBytes = regexp.MustCompile(`0x((?:[0-9a-fA-F]{2})+)`)

func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		sb.WriteRune(r) // an invalid byte decodes to U+FFFD, one per byte
		b = b[size:]
	}
	return sb.String()
}

// DecodeHexBodies appends the decoded text of any 0x... blob, keeping the
// original: a gateway may hand back the upstream body as a hex blob.
func DecodeHexBodies(detail string) string {
	for _, m := range sdkBytes.FindAllStringSubmatch(detail, -1) {
		raw := make([]byte, len(m[1])/2)
		for i := range raw {
			raw[i] = unhex(m[1][2*i])<<4 | unhex(m[1][2*i+1])
		}
		if text := decodeReplace(raw); text != "" {
			detail = detail + " " + text
		}
	}
	return detail
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// APIURL joins a path onto a base the way the native Anthropic client does:
// /v1/messages is never stripped, other /v1 paths are when the base already
// ends in a version.
func APIURL(base, path string) string {
	trimmed := strings.TrimRight(base, "/")
	u, err := url.Parse(trimmed)
	if err != nil {
		return trimmed + path
	}
	prefix := strings.TrimRight(u.EscapedPath(), "/")
	if path != "/v1/messages" && versionSuffix.MatchString(prefix) && strings.HasPrefix(path, "/v1") {
		path = path[3:]
	}
	netloc := u.Host
	if u.User != nil {
		netloc = u.User.String() + "@" + netloc
	}
	return u.Scheme + "://" + netloc + prefix + path
}

var versionSuffix = regexp.MustCompile(`(?i)/v[0-9]+$`)

func payload(nChars int) string {
	var sb strings.Builder
	n := 0
	for i := 0; n < nChars; i++ {
		s := fmt.Sprintf("The quick brown fox jumps over the lazy dog number %d. ", i)
		sb.WriteString(s)
		n += len(s)
	}
	return sb.String()
}

func pad(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func padLeft(s string, width int) string {
	if n := utf8.RuneCountInString(s); n < width {
		return strings.Repeat(" ", width-n) + s
	}
	return s
}

func commas(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}
