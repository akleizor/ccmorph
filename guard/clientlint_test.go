package guard

import (
	"errors"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestEveryProductionHTTPClientTrustsTheSessionsExtraCA extends #822's guard to
// the whole module. A production *http.Client (or an http.Transport) that
// honours the proxy environment but not NODE_EXTRA_CA_CERTS fails its TLS
// handshake inside a Remote Control session before any request leaves; the
// adapter guard could not see shim or ctxwindow, one package outside its walk.
//
// The guard proves trust where a client or transport is built and then keeps
// that trust out of reach, so it never follows a value through the program:
//   - a client literal names its Transport: a call to netroute.NewTransport, a
//     transport literal, or a local Go binds only to netroute.NewTransport;
//   - a transport literal is direct (Proxy absent or nil) or carries
//     TLSClientConfig: netroute.ExtraCAs() inline, with no DialTLS override;
//   - a transport's TLSClientConfig is never named outside such a literal, so
//     no code can read, share, edit or replace the CA a transport trusts;
//   - Proxy, DialTLS, DialTLSContext and a client's Transport are never
//     rewritten, nor a whole client or transport, however the write is made;
//   - http.Client and http.Transport are named only as a pointer or a literal's
//     type, a client or transport value lives only in a local declared with a
//     value (a copy), never where it can start out zero, and
//     http.DefaultTransport, DefaultClient, Get, Head, Post and PostForm are
//     not named at all.
//
// Every name is Go's own (go/types). netroute/'s NewTransport, NewClient and
// ExtraCAs bodies are the constructor the issue names, proven by
// netroute/ca_test.go; every other line of the module is judged (#829).
func TestEveryProductionHTTPClientTrustsTheSessionsExtraCA(t *testing.T) {
	got, err := inspectTree("..")
	if err != nil {
		t.Fatal(err)
	}
	refused, allowed := 0, 0
	for _, x := range got {
		if !x.allowed {
			refused++
			t.Errorf("%s: %s", x.pos, x.why)
			continue
		}
		allowed++
	}
	if refused == 0 && allowed == 0 {
		t.Fatalf("the guard found no http client construction under ..: it is not reading the module")
	}
}

// TestTheWholeModuleGuardSeesEveryBypass pins each rule with the cases that
// cross it and the cases that must not. Each row is a whole file; "refused"
// needs at least one refused finding, "allowed" a finding and none refused,
// "none" no finding at all.
func TestTheWholeModuleGuardSeesEveryBypass(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		// A client is proved by its own Transport, never by its scope.
		{"bare client", guardSrc("var c = &http.Client{}"), "refused"},
		{"timeout-only client", guardSrc("var c = &http.Client{Timeout: time.Second}"), "refused"},
		{"nil transport", guardSrc("var c = &http.Client{Transport: nil}"), "refused"},
		{"sibling initializer", guardSrc("var a, b = caClient(), &http.Client{}"), "refused"},
		{"unrelated builder call", guardSrc("func f() *http.Client {\n\t_ = netroute.NewClient(1)\n\treturn &http.Client{}\n}"), "refused"},
		{"bare client beside a builder", "package p\n\nimport \"net/http\"\n\nfunc f() { _ = netroute.NewClient(1); _ = &http.Client{} }\n", "refused"},
		{"method borrowing the name", guardSrc("type bypass struct{}\n\nfunc (bypass) caClient() *http.Client { return &http.Client{} }"), "refused"},
		{"builder inline", guardSrc("func f() *http.Client { return &http.Client{Transport: netroute.NewTransport()} }"), "allowed"},
		{"builder inline, parenthesised", guardSrc("func f() *http.Client { return &http.Client{Transport: (netroute.NewTransport())} }"), "allowed"},
		{"direct client", guardSrc("var c = &http.Client{Transport: &http.Transport{Proxy: nil}}"), "allowed"},
		{"direct client with no Proxy key", guardSrc("var c = &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}"), "allowed"},
		{"client on a proxied transport literal", guardSrc("var c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}"), "refused"},
		{"client on a CA transport literal", guardSrc("func f() *http.Client {\n\treturn &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: netroute.ExtraCAs()}}\n}"), "allowed"},
		{"client on a custom round tripper", guardSrc("type logRT struct{ inner http.RoundTripper }\n\nfunc (l logRT) RoundTrip(r *http.Request) (*http.Response, error) { return l.inner.RoundTrip(r) }\n\nvar c = &http.Client{Transport: logRT{netroute.NewTransport()}}"), "refused"},
		{"client on a transport parameter", guardSrc("func f(tr *http.Transport) *http.Client { return &http.Client{Transport: tr} }"), "refused"},
		{"client on a helper's transport", guardSrc("func mk() *http.Transport { return netroute.NewTransport() }\n\nvar c = &http.Client{Transport: mk()}"), "refused"},
		{"client on another client's transport", guardSrc("var c = &http.Client{Transport: netroute.NewClient(time.Second).Transport}"), "refused"},
		{"client from the builder", guardSrc("func f() *http.Client { return netroute.NewClient(time.Second) }"), "none"},
		{"positional client", guardSrc("var c = &http.Client{netroute.NewTransport(), nil, nil, 0}"), "refused"},

		// A local proves a transport only when Go binds it to the builder and
		// to nothing else, however it is written to.
		{"builder bound", guardSrc("func f() *http.Client {\n\ttr := netroute.NewTransport()\n\treturn &http.Client{Transport: tr}\n}"), "allowed"},
		{"builder bound by var", guardSrc("func f() *http.Client {\n\tvar tr = netroute.NewTransport()\n\treturn &http.Client{Transport: tr}\n}"), "allowed"},
		{"builder bound twice", guardSrc("func f() *http.Client {\n\ttr := netroute.NewTransport()\n\ttr = netroute.NewTransport()\n\treturn &http.Client{Transport: tr}\n}"), "allowed"},
		{"an interface-typed local bound to the builder", guardSrc("func f() *http.Client {\n\tvar rt http.RoundTripper = netroute.NewTransport()\n\treturn &http.Client{Transport: rt}\n}"), "allowed"},
		{"a closure dials its parent's builder-bound local", guardSrc("func f() func() *http.Client {\n\ttr := netroute.NewTransport()\n\treturn func() *http.Client { return &http.Client{Transport: tr} }\n}"), "allowed"},
		{"builder rebound to a parameter", guardSrc("func f(o *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\ttr = o\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder rebound inside a closure", guardSrc("func f(o *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\tfunc() { tr = o }()\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder bound in one branch only", guardSrc("func f(x bool) *http.Client {\n\tvar tr *http.Transport\n\tif x {\n\t\ttr = netroute.NewTransport()\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"parameter rebound in a branch", guardSrc("func f(tr *http.Transport, x bool) *http.Client {\n\tif x {\n\t\ttr = netroute.NewTransport()\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder local's address taken", guardSrc("func f(set func(**http.Transport)) *http.Client {\n\ttr := netroute.NewTransport()\n\tset(&tr)\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder local range-assigned", guardSrc("func f(ts []*http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\tfor _, tr = range ts {\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder local from a tuple", guardSrc("func f(mk func() (*http.Transport, error)) *http.Client {\n\ttr := netroute.NewTransport()\n\ttr, _ = mk()\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder local redeclared from a tuple", guardSrc("func f(mk func() (*http.Transport, error)) *http.Client {\n\ttr := netroute.NewTransport()\n\ttr, err := mk()\n\t_ = err\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"builder local in a select receive", guardSrc("func f(ch chan *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\tselect {\n\tcase tr = <-ch:\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a package variable bound to the builder", guardSrc("var tr = netroute.NewTransport()\n\nvar c = &http.Client{Transport: tr}"), "refused"},
		{"a package variable behind a block-local builder", guardSrc("var tr http.RoundTripper\n\nfunc f() *http.Client {\n\t{\n\t\ttr := netroute.NewTransport()\n\t\t_ = tr\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a builder function value", guardSrc("func f() *http.Client {\n\tmk := netroute.NewTransport\n\treturn &http.Client{Transport: mk()}\n}"), "refused"},
		{"a type switch variable", guardSrc("func f(x interface{}) *http.Client {\n\tswitch tr := x.(type) {\n\tcase *http.Transport:\n\t\treturn &http.Client{Transport: tr}\n\t}\n\treturn nil\n}"), "refused"},

		// A transport literal is direct or carries the CA inline.
		{"proxied transport literal", guardSrc("var t = &http.Transport{Proxy: http.ProxyFromEnvironment}"), "refused"},
		{"proxied transport with the CA", guardSrc("func f() *http.Transport {\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: netroute.ExtraCAs()}\n}"), "allowed"},
		{"proxied transport with the CA and a TLS dialer", guardSrc("func f(d func(ctx context.Context, n, a string) (net.Conn, error)) *http.Transport {\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: netroute.ExtraCAs(), DialTLSContext: d}\n}"), "refused"},
		{"proxied transport with the CA from a local", guardSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}\n}"), "refused"},
		{"proxied transport with an edited copy of the CA", tlsSrc("func f() *http.Transport {\n\tcfg := *netroute.ExtraCAs()\n\tcfg.InsecureSkipVerify = true\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &cfg}\n}"), "refused"},
		{"proxied transport with its own config", tlsSrc("func f() *http.Transport {\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &tls.Config{}}\n}"), "refused"},
		{"a parameter named nil is no direct route", fileSrc("import (\n\t\"net/http\"\n\t\"net/url\"\n)\n\nfunc f(nil func(*http.Request) (*url.URL, error)) *http.Transport { return &http.Transport{Proxy: nil} }\n"), "refused"},
		{"a proxy parameter with the CA", fileSrc("import (\n\t\"net/http\"\n\t\"net/url\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nfunc f(p func(*http.Request) (*url.URL, error)) *http.Transport {\n\treturn &http.Transport{Proxy: p, TLSClientConfig: netroute.ExtraCAs()}\n}\n"), "allowed"},
		{"proxied transport handed out", guardSrc("func f(use func(http.RoundTripper)) { use(&http.Transport{Proxy: http.ProxyFromEnvironment}) }"), "refused"},
		{"direct transport handed out", guardSrc("func f(use func(http.RoundTripper)) { use(&http.Transport{}) }"), "allowed"},
		{"elided slice element", guardSrc("var cs = []*http.Client{{}}"), "refused"},
		{"elided map element", guardSrc(`var ts = map[string]*http.Transport{"a": {Proxy: http.ProxyFromEnvironment}}`), "refused"},

		// http.DefaultTransport and the DefaultClient helpers are refused by
		// name: the only CA-aware clone is netroute.NewTransport's own.
		{"clone without the CA", guardSrc("func f() *http.Client {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"clone then the CA", guardSrc("func f() *http.Transport {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\ttr.TLSClientConfig = netroute.ExtraCAs()\n\treturn tr\n}"), "refused"},
		{"DefaultTransport returned bare", guardSrc("func clone() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }"), "refused"},
		{"DefaultTransport as a round tripper", guardSrc("var c = &http.Client{Transport: http.DefaultTransport}"), "refused"},
		{"DefaultClient", guardSrc("func f(r *http.Request) { _, _ = http.DefaultClient.Do(r) }"), "refused"},
		{"http.Get", guardSrc(`func f() { _, _ = http.Get("https://models.dev") }`), "refused"},
		{"http.Head", guardSrc(`func f() { _, _ = http.Head("https://models.dev") }`), "refused"},
		{"http.Post", guardSrc(`func f() { _, _ = http.Post("https://models.dev", "text/plain", nil) }`), "refused"},
		{"http.PostForm", guardSrc(`func f() { _, _ = http.PostForm("https://models.dev", nil) }`), "refused"},
		{"http.Get as a value", guardSrc("var get = http.Get"), "refused"},

		// A file's own names: an alias is the same package, a local that hides
		// an import is no package, and only this module's netroute is trusted.
		{"aliased client", fileSrc("import web \"net/http\"\n\nfunc f() { _ = &web.Client{} }\n"), "refused"},
		{"aliased client value", fileSrc("import web \"net/http\"\n\nvar c = web.Client{}\n"), "refused"},
		{"aliased new", fileSrc("import web \"net/http\"\n\nvar c = new(web.Client)\n"), "refused"},
		{"aliased clone", fileSrc("import web \"net/http\"\n\nfunc f() *web.Transport { return web.DefaultTransport.(*web.Transport).Clone() }\n"), "refused"},
		{"aliased transport literal", fileSrc("import web \"net/http\"\n\nvar t = &web.Transport{Proxy: web.ProxyFromEnvironment}\n"), "refused"},
		{"net/http imported twice", "package p\n\nimport (\n\t\"net/http\"\n\tweb \"net/http\"\n)\n\nvar a *http.Client\nvar c = &web.Client{}\n", "refused"},
		{"dot import of net/http", fileSrc("import . \"net/http\"\n\nvar c = &Client{}\n"), "refused"},
		{"dot import of netroute", "package p\n\nimport (\n\t\"net/http\"\n\n\t. \"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar c = &http.Client{Transport: NewTransport()}\n", "refused"},
		{"a foreign netroute is no proof", "package p\n\nimport (\n\t\"net/http\"\n\n\tnetroute \"example.com/fake\"\n)\n\nvar c = &http.Client{Transport: netroute.NewTransport()}\n", "refused"},
		{"netroute never imported", "package p\n\nimport \"net/http\"\n\nvar c = &http.Client{Transport: netroute.NewTransport()}\n", "refused"},
		{"aliased client on the builder", "package p\n\nimport (\n\tweb \"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar c = &web.Client{Transport: netroute.NewTransport()}\n", "allowed"},
		{"aliased netroute builder", "package p\n\nimport (\n\t\"net/http\"\n\n\tnr \"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar c = &http.Client{Transport: nr.NewTransport()}\n", "allowed"},
		{"a package named netroute is not the module's", "package netroute\n\nimport \"net/http\"\n\nfunc NewTransport() *http.Transport { return nil }\n\nfunc NewClient() *http.Client { return &http.Client{Transport: NewTransport()} }\n", "refused"},
		{"a local named like the builder inside netroute", "package netroute\n\nimport \"net/http\"\n\nfunc f() *http.Client {\n\tNewTransport := func() *http.Transport { return &http.Transport{} }\n\treturn &http.Client{Transport: NewTransport()}\n}\n", "refused"},
		{"shadowed netroute alias", "package route\n\nimport (\n\t\"net/http\"\n\tnr \"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar safe = nr.NewClient(0)\n\ntype factory struct{ tr http.RoundTripper }\n\nfunc (f factory) NewTransport() http.RoundTripper { return f.tr }\n\nfunc wrap(nr factory) *http.Client { return &http.Client{Transport: nr.NewTransport()} }\n\nvar broken = wrap(factory{})\n", "refused"},
		{"shadowed net/http alias", "package p\n\nimport web \"net/http\"\n\ntype Client struct{}\n\nfunc f() {\n\tweb := struct{ Client int }{}\n\t_ = web.Client\n\tvar c Client\n\t_ = c\n}\n", "none"},
		{"nested block does not shadow", "package p\n\nimport web \"net/http\"\n\nfunc f() {\n\t{ web := struct{ Client int }{}; _ = web.Client }\n\t_ = &web.Client{}\n}\n", "refused"},
		{"a local shadows inside its block", "package p\n\nimport web \"net/http\"\n\nfunc f() {\n\tweb := struct{ Client int }{}\n\t_ = web.Client\n}\n", "none"},
		{"an initializer sees the import", "package p\n\nimport web \"net/http\"\n\nfunc f() {\n\tweb := web.Client{}\n\t_ = web\n}\n", "refused"},
		{"if init does not leak", "package p\n\nimport web \"net/http\"\n\nfunc f() {\n\tif web := 1; web > 0 {\n\t\t_ = web\n\t}\n\t_ = &web.Client{}\n}\n", "refused"},
		{"if init shadows inside its body", "package p\n\nimport (\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\ntype factory struct{}\n\nfunc (factory) NewTransport() http.RoundTripper { return nil }\n\nfunc f(x bool) *http.Client {\n\tif netroute := (factory{}); x {\n\t\treturn &http.Client{Transport: netroute.NewTransport()}\n\t}\n\treturn nil\n}\n", "refused"},

		// http.Client and http.Transport are named only as a pointer or as a
		// literal's type: anything else hides the transport a client dials.
		{"new client", guardSrc("var c = new(http.Client)"), "refused"},
		{"zero-value client", guardSrc("func f() {\n\tvar c http.Client\n\t_ = c\n}"), "refused"},
		{"a client copied to change its redirect policy", guardSrc("func f(client *http.Client) *http.Client {\n\tprobe := *client\n\tprobe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }\n\treturn &probe\n}"), "none"},
		{"a client copied by var", guardSrc("func f() *http.Client {\n\tvar c = *netroute.NewClient(time.Second)\n\treturn &c\n}"), "none"},
		{"a client copy rewritten whole", guardSrc("func f() *http.Client {\n\tc := *netroute.NewClient(time.Second)\n\tc = http.Client{Timeout: time.Second}\n\treturn &c\n}"), "refused"},
		{"a client copy overwritten through its address", guardSrc("func f(o *http.Client) *http.Client {\n\tc := *netroute.NewClient(time.Second)\n\tp := &c\n\t*p = *o\n\treturn &c\n}"), "refused"},
		{"a client copy's transport rewired", guardSrc("func f(rt http.RoundTripper) *http.Client {\n\tc := *netroute.NewClient(time.Second)\n\tc.Transport = rt\n\treturn &c\n}"), "refused"},
		{"a client copy from a tuple", guardSrc("func f(get func() (http.Client, error)) {\n\tc, _ := get()\n\t_ = c\n}"), "refused"},
		{"a zero-value transport local", guardSrc("func f() *http.Transport {\n\tvar t http.Transport\n\treturn &t\n}"), "refused"},
		{"a client from a map", guardSrc("func f(m map[string]http.Client) *http.Client {\n\tc := m[\"a\"]\n\treturn &c\n}"), "refused"},
		{"a client value result", guardSrc("func mk() (c http.Client) { return }"), "refused"},
		{"value field", guardSrc("type s struct{ c http.Client }"), "refused"},
		{"embedded client", guardSrc("type s struct{ http.Client }"), "refused"},
		{"value parameter", guardSrc("func f(c http.Client) {}"), "refused"},
		{"client type alias", guardSrc("type C = http.Client\n\nvar c = &C{}"), "refused"},
		{"defined client type", guardSrc("type C http.Client\n\nvar c = &C{}"), "refused"},
		{"transport type alias", guardSrc("type T = http.Transport\n\nvar t = &T{Proxy: http.ProxyFromEnvironment}"), "refused"},
		{"type argument", guardSrc("func mk[T any]() *T { return new(T) }\n\nvar c = mk[http.Client]()"), "refused"},
		{"inferred type argument", guardSrc("func zero[T any](p *T) {\n\tvar z T\n\t*p = z\n}\n\nfunc f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tzero(c)\n\treturn c\n}"), "refused"},
		{"an inferred type argument through a value", guardSrc("func set[T any](p *T, v T) { *p = v }\n\nfunc f(o *http.Client) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tset(c, *o)\n\treturn c\n}"), "refused"},
		{"a generic type over a transport value", guardSrc("type box[T any] struct{ v T }\n\nvar b box[http.Transport]"), "refused"},
		{"a slice of clients", guardSrc("var cs []http.Client"), "refused"},
		{"a value type assertion", guardSrc("func f(x interface{}) { _ = x.(http.Client) }"), "refused"},
		{"a conversion to a client pointer", guardSrc("type look struct {\n\tTransport     http.RoundTripper\n\tCheckRedirect func(req *http.Request, via []*http.Request) error\n\tJar           http.CookieJar\n\tTimeout       time.Duration\n}\n\nfunc f(rt http.RoundTripper) *http.Client {\n\tl := &look{}\n\tl.Transport = rt\n\treturn (*http.Client)(l)\n}"), "refused"},
		{"pointers only", guardSrc("type s struct{ c *http.Client }\n\nfunc f(c *http.Client) *http.Transport { return nil }"), "none"},
		{"a pointer type assertion", guardSrc("func f(c *http.Client) bool {\n\t_, ok := c.Transport.(*http.Transport)\n\treturn ok\n}"), "none"},

		// Nothing rewires a client or transport after construction, and a
		// transport's TLSClientConfig is never named outside its literal.
		{"opaque transport assignment", guardSrc("func f(rt http.RoundTripper) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = rt\n\treturn c\n}"), "refused"},
		{"nil transport assignment", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = nil\n\treturn c\n}"), "refused"},
		{"builder transport assignment", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = netroute.NewTransport()\n\treturn c\n}"), "allowed"},
		{"direct transport assignment", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = &http.Transport{}\n\treturn c\n}"), "allowed"},
		{"a struct field's client rewired", guardSrc("type srv struct{ c *http.Client }\n\nfunc f(rt http.RoundTripper) *srv {\n\ts := &srv{c: netroute.NewClient(time.Second)}\n\ts.c.Transport = rt\n\treturn s\n}"), "refused"},
		{"a helper's client rewired", guardSrc("func get() *http.Client { return netroute.NewClient(time.Second) }\n\nfunc f(rt http.RoundTripper) *http.Client {\n\tc := get()\n\tc.Transport = rt\n\treturn c\n}"), "refused"},
		{"proxy set later", guardSrc("func f() *http.Client {\n\ttr := &http.Transport{}\n\ttr.Proxy = http.ProxyFromEnvironment\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"TLS dialer set later", guardSrc("func f(d func(ctx context.Context, n, a string) (net.Conn, error)) *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.DialTLSContext = d\n\treturn tr\n}"), "refused"},
		{"builder then wiped", guardSrc("func f() *http.Client {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig = nil\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"written through TLSClientConfig", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"TLSClientConfig only compared", guardSrc("func f() bool {\n\ttr := netroute.NewTransport()\n\treturn tr.TLSClientConfig != nil\n}"), "refused"},
		{"TLSClientConfig read into a local", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := tr.TLSClientConfig\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"TLS config through the assertion", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport.(*http.Transport).TLSClientConfig = nil\n\treturn c\n}"), "refused"},
		{"the CA wiped through an asserted local", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tt := c.Transport.(*http.Transport)\n\tt.TLSClientConfig = nil\n\treturn c\n}"), "refused"},
		{"an embedded transport's CA wiped", guardSrc("type wrap struct{ *http.Transport }\n\nfunc f() wrap {\n\tw := wrap{netroute.NewTransport()}\n\tw.TLSClientConfig = nil\n\treturn w\n}"), "refused"},
		{"an embedded transport's proxy set", guardSrc("type wrap struct{ *http.Transport }\n\nfunc f() wrap {\n\tw := wrap{&http.Transport{}}\n\tw.Proxy = http.ProxyFromEnvironment\n\treturn w\n}"), "refused"},
		{"a defined transport type's proxy set", guardSrc("type myT http.Transport\n\nfunc f() *http.Transport {\n\ttr := &http.Transport{}\n\t(*myT)(tr).Proxy = http.ProxyFromEnvironment\n\treturn tr\n}"), "refused"},
		{"the transport's pool read", fileSrc("import (\n\t\"crypto/tls\"\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nfunc f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\treturn tr, &tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}\n}\n"), "refused"},
		{"a TLS config element written through the transport", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.NextProtos[0] = \"h2\"\n\treturn tr\n}"), "refused"},
		{"a TLS config field incremented through the transport", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.MinVersion++\n\treturn tr\n}"), "refused"},
		{"a range loop assigns the transport's TLS config", tlsSrc("func f(cfgs []*tls.Config) *http.Transport {\n\ttr := netroute.NewTransport()\n\tfor _, tr.TLSClientConfig = range cfgs {\n\t}\n\treturn tr\n}"), "refused"},
		{"the address of a transport's TLSClientConfig", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tpp := &tr.TLSClientConfig\n\t*pp = nil\n\treturn tr\n}"), "refused"},
		{"the address of a direct transport's Proxy", guardSrc("func f() *http.Transport {\n\ttr := &http.Transport{}\n\tpp := &tr.Proxy\n\t*pp = http.ProxyFromEnvironment\n\treturn tr\n}"), "refused"},
		{"the address of a client's Transport", guardSrc("func f(rt http.RoundTripper) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tpt := &c.Transport\n\t*pt = rt\n\treturn c\n}"), "refused"},
		{"a range loop assigns a client's Transport", guardSrc("func f(rts []http.RoundTripper) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tfor _, c.Transport = range rts {\n\t}\n\treturn c\n}"), "refused"},
		{"a generic helper clears a client's Transport", guardSrc("func clear[T any](p *T) {\n\tvar z T\n\t*p = z\n}\n\nfunc f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tclear(&c.Transport)\n\treturn c\n}"), "refused"},
		{"a builder's transport overwritten whole", guardSrc("func f(o *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\t*tr = *o\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a client copy copied again", guardSrc("func f(c *http.Client) *http.Client {\n\tc2 := *c\n\tc3 := c2\n\treturn &c3\n}"), "none"},
		{"a client copy discarded or kept in an interface", guardSrc("func f(c *http.Client) interface{} {\n\tc2 := *c\n\t_ = c2\n\tvar x interface{}\n\tx = c2\n\treturn x\n}"), "none"},
		{"a whole transport copied through a pointer Go cannot resolve", fileSrc("import (\n\t\"net/http\"\n\n\t\"example.com/lib\"\n)\n\nfunc f(o *http.Transport) { *lib.Shared() = *o }\n"), "refused"},
		{"a client overwritten whole through a field", guardSrc("type srv struct{ c *http.Client }\n\nfunc f(s *srv, o *http.Client) { *s.c = *o }"), "refused"},
		{"a client's transport read", guardSrc("func f(c *http.Client) http.RoundTripper { return c.Transport }"), "none"},
		{"a transport's proxy read", guardSrc("func f(tr *http.Transport) bool { return tr.Proxy != nil }"), "none"},
		{"a builder client's own timeout may change", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Timeout = 2 * time.Second\n\treturn c\n}"), "none"},
		{"a builder client's timeout through its address", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tp := &c.Timeout\n\t*p = 2 * time.Second\n\treturn c\n}"), "none"},
		{"a builder client's timeout incremented", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Timeout++\n\treturn c\n}"), "none"},
		{"a builder transport's idle limit", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.MaxIdleConns = 4\n\treturn tr\n}"), "none"},
		{"a request's own fields, through their address too", guardSrc("func f(r *http.Request) {\n\tr.Header = http.Header{}\n\tp := &r.Host\n\t*p = \"x\"\n\tr.ContentLength++\n}"), "none"},
		{"a range loop over plain locals", guardSrc("func f(xs []int) int {\n\tn := 0\n\tfor _, n = range xs {\n\t}\n\treturn n\n}"), "none"},

		// A field with an HTTP name on a type Go resolves elsewhere is no
		// rewire; one Go cannot resolve is judged as net/http's.
		{"non-HTTP field names", "package settings\n\nimport \"net/http\"\n\ntype Options struct {\n\tClient          *http.Client\n\tTransport       string\n\tProxy           string\n\tTLSClientConfig string\n}\n\nfunc configure(o *Options) *http.Client {\n\to.Transport = \"stdio\"\n\to.Proxy = \"inherit\"\n\to.TLSClientConfig = \"system\"\n\t_ = o.TLSClientConfig\n\treturn o.Client\n}\n", "none"},
		{"a same-package type's own Transport field", guardSrc("type opts struct{ Transport string }\n\nfunc f(o *opts) { o.Transport = \"stdio\" }"), "none"},
		{"a field address on an options type", guardSrc("type opts struct{ Transport string }\n\nfunc f(o *opts, set func(*string)) { set(&o.Transport) }"), "none"},
		{"a field Go cannot resolve is judged as net/http's", fileSrc("import \"example.com/opts\"\n\nfunc f(o *opts.Options) { o.Proxy = \"inherit\" }\n"), "refused"},
		{"a TLSClientConfig Go cannot resolve", fileSrc("import \"example.com/lib\"\n\nfunc f() bool { return lib.Shared().TLSClientConfig != nil }\n"), "refused"},
		{"an unresolved client's transport set to the builder", fileSrc("import (\n\t\"example.com/lib\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nfunc f() { lib.Client().Transport = netroute.NewTransport() }\n"), "allowed"},
		{"an unresolved package's own Proxy", fileSrc("import \"example.com/lib\"\n\nfunc f() { lib.Proxy = nil }\n"), "none"},

		// A transport's CA can only be shared by naming its TLSClientConfig,
		// so the aliasing that once hid it (a shared slice, a holder, a pool)
		// is refused where it starts, and a server's own TLS config, which
		// never touches a transport, is no finding at all.
		{"an aliased slice carries the transport's config", tlsSrc("func route() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tconfigs := []*tls.Config{{}}\n\talias := configs\n\talias[0] = tr.TLSClientConfig\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: configs[0].RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a holder carries the transport's config", tlsSrc("type holder struct{ cfg *tls.Config }\n\nfunc f() *http.Transport {\n\ttr := netroute.NewTransport()\n\th := holder{cfg: tr.TLSClientConfig}\n\th.cfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a closure returns the transport's pool", fileSrc("import (\n\t\"crypto/x509\"\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nfunc f() (*http.Transport, func() *x509.CertPool) {\n\ttr := netroute.NewTransport()\n\treturn tr, func() *x509.CertPool { return tr.TLSClientConfig.RootCAs }\n}\n"), "refused"},
		{"a server-only TLS config is no client rewire", fileSrc("import (\n\t\"crypto/tls\"\n\t\"net/http\"\n)\n\nfunc serve(saved *http.Client, certs []tls.Certificate) (*http.Client, *http.Server) {\n\tcfg := &tls.Config{}\n\tcfg.Certificates = certs\n\treturn saved, &http.Server{TLSConfig: cfg}\n}\n"), "none"},
		{"a server config through a type alias", tlsSrc("type serverTLS = tls.Config\n\nfunc serve(saved *http.Client, certs []tls.Certificate) (*http.Client, *http.Server) {\n\tcfg := &serverTLS{}\n\tcfg.Certificates = certs\n\treturn saved, &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a server config of a defined type, overwritten whole", tlsSrc("type serverTLS tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tcfg := &serverTLS{}\n\t*cfg = serverTLS{Certificates: certs}\n\treturn &http.Server{TLSConfig: (*tls.Config)(cfg)}\n}"), "none"},
		{"a server config from new", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := new(tls.Config)\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a package-level server config value", tlsSrc("var serverCfg tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tserverCfg.Certificates = certs\n\treturn &http.Server{TLSConfig: &serverCfg}\n}"), "none"},
		{"a server's TLSConfig written through the server", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tsrv := &http.Server{TLSConfig: &tls.Config{}}\n\tsrv.TLSConfig.Certificates = certs\n\treturn srv\n}"), "none"},
		{"a server pool through an explicit dereference", fileSrc("import (\n\t\"crypto/tls\"\n\t\"crypto/x509\"\n\t\"net/http\"\n)\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tbase := &tls.Config{ClientCAs: x509.NewCertPool()}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: (*base).ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}\n"), "none"},
		{"a server pool through an aliased slice", fileSrc("import (\n\t\"crypto/tls\"\n\t\"crypto/x509\"\n\t\"net/http\"\n)\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tconfigs := []*tls.Config{{ClientCAs: x509.NewCertPool()}}\n\talias := configs\n\talias[0].MinVersion = tls.VersionTLS12\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: configs[0].ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}\n"), "none"},
		{"a handed-in config written", tlsSrc("func harden(cfg *tls.Config) { cfg.MinVersion = tls.VersionTLS12 }"), "none"},
		{"a handed-in pool overwritten", fileSrc("import \"crypto/x509\"\n\nfunc clear(p *x509.CertPool) { *p = x509.CertPool{} }\n"), "none"},
		{"the session CA's own copy, edited for a raw TLS dial", tlsSrc("func dial(addr string) (*tls.Conn, error) {\n\tcfg := netroute.ExtraCAs()\n\tif cfg == nil {\n\t\tcfg = &tls.Config{}\n\t}\n\tcfg.ServerName = \"edge\"\n\treturn tls.Dial(\"tcp\", addr, cfg)\n}"), "none"},
	}
	for _, k := range cases {
		k := k
		t.Run(k.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "synthetic.go", k.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			refused, allowed := 0, 0
			for _, x := range inspectFile(fset, f) {
				if x.allowed {
					allowed++
					continue
				}
				refused++
				if !strings.Contains(x.why, "NODE_EXTRA_CA_CERTS") || !strings.Contains(x.why, "netroute") {
					t.Errorf("a refusal does not name the cause and the fix: %s", x.why)
				}
			}
			got := "none"
			if refused > 0 {
				got = "refused"
			} else if allowed > 0 {
				got = "allowed"
			}
			if got != k.want {
				t.Fatalf("guard %s %d construction(s), want %s", got, refused+allowed, k.want)
			}
		})
	}
}

// TestABareClientIsNotBlessedByAnUnrelatedFactoryCall is the evaluator's repro
// of the first guard: a plain &http.Client{} in a function that also calls
// netroute.NewClient must stay refused, because only the client's own
// Transport can prove it trusts the session's CA.
func TestABareClientIsNotBlessedByAnUnrelatedFactoryCall(t *testing.T) {
	const src = `package p
import "net/http"
func f() { _ = netroute.NewClient(1); _ = &http.Client{} }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range inspectFile(fset, f) {
		if !got.allowed {
			return
		}
	}
	t.Fatal("guard allowed a bare client because an unrelated netroute.NewClient call appears in the same function")
}

// TestAShadowedAliasIsNotTheBuilder is the evaluator's shadowing repro: a
// parameter named nr hides the netroute import, so nr.NewTransport() is a
// method on the local factory (returning a nil RoundTripper, which makes the
// client dial http.DefaultTransport) and not the CA-aware builder.
func TestAShadowedAliasIsNotTheBuilder(t *testing.T) {
	const src = `package route
import ("net/http"; nr "github.com/akleizor/ccmorph/netroute")
var safe = nr.NewClient(0)
type factory struct { tr http.RoundTripper }
func (f factory) NewTransport() http.RoundTripper { return f.tr }
func wrap(nr factory) *http.Client { return &http.Client{Transport: nr.NewTransport()} }
var broken = wrap(factory{})`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "route.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range inspectFile(fset, f) {
		if !x.allowed {
			return
		}
	}
	t.Fatal("the local factory method is not netroute.NewTransport")
}

// TestAnOptionsOwnFieldsAreNoClientRewire is the evaluator's false-positive
// repro: an options type may have its own Transport, Proxy or TLSClientConfig
// fields; Go says which type owns each, so none of these is refused.
func TestAnOptionsOwnFieldsAreNoClientRewire(t *testing.T) {
	const src = `package settings; import "net/http"; type Options struct { Client *http.Client; Transport string; Proxy string; TLSClientConfig string }; func configure(o *Options) *http.Client { o.Transport = "stdio"; o.Proxy = "inherit"; o.TLSClientConfig = "system"; return o.Client }`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "settings.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range inspectFile(fset, f) {
		if !x.allowed {
			t.Errorf("%s: unrelated field refused: %s", x.pos, x.why)
		}
	}
}

// TestALocalInANestedBlockDoesNotShadowTheImport is the evaluator's lexical
// repro: a local web inside a block hides the net/http import only until that
// block closes, so the &web.Client{} after it is the real client and must be
// refused for using http.DefaultTransport.
func TestALocalInANestedBlockDoesNotShadowTheImport(t *testing.T) {
	const src = `package p
import web "net/http"
func f() {
	{ web := struct{ Client int }{}; _ = web.Client }
	_ = &web.Client{}
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspectFile(fset, f)) == 0 {
		t.Fatal("guard missed the HTTP client after web went out of scope")
	}
}

// TestAWholeTLSWriteCannotShareTheTransportsRoots is the evaluator's pool
// repro (313c70b): a fresh config overwritten with the transport's RootCAs
// shares that pool, and the store through it clears the roots the transport
// trusts. Both places that name the transport's TLSClientConfig are refused,
// which is where the sharing starts; the stores into the fresh config are not
// judged, because nothing else can reach the transport's pool.
func TestAWholeTLSWriteCannotShareTheTransportsRoots(t *testing.T) {
	const src = `package p
import ("crypto/tls"; "crypto/x509"; "net/http"; "github.com/akleizor/ccmorph/netroute")
func route() *http.Transport {
	tr := netroute.NewTransport()
	if tr.TLSClientConfig != nil {
		cfg := &tls.Config{}
		*cfg = tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}
		*cfg.RootCAs = *x509.NewCertPool()
	}
	return tr
}`
	wantRefusals(t, "route.go", src, "route.go:5:5", "route.go:7:30")
}

// TestAnAliasedSliceCannotCarryTheTransportsConfig is the evaluator's slice
// repro (1cb750d): alias and configs share one backing array, so a transport's
// config stored through alias is read back through configs. The guard refuses
// the one place the transport's config is named; how the value travels after
// that is never followed, so no alias can hide it.
func TestAnAliasedSliceCannotCarryTheTransportsConfig(t *testing.T) {
	const src = `package p
import ("crypto/tls"; "net/http"; "github.com/akleizor/ccmorph/netroute")
func route() (*http.Transport, *tls.Config) {
 tr := netroute.NewTransport()
 configs := []*tls.Config{{}}
 alias := configs
 alias[0] = tr.TLSClientConfig
 cfg := &tls.Config{}
 *cfg = tls.Config{RootCAs: configs[0].RootCAs}
 return tr, cfg
}`
	wantRefusals(t, "route.go", src, "route.go:7:13")
}

// TestAPackageIsCheckedWhole: the production walk type-checks each package's
// files together, so a receiver whose type another file declares resolves.
// An options type's own Transport string in a.go is left alone when b.go sets
// it; a client field declared in a.go and rewired in b.go is still refused.
func TestAPackageIsCheckedWhole(t *testing.T) {
	fset := token.NewFileSet()
	a, err := parser.ParseFile(fset, "a.go", "package p\n\nimport \"net/http\"\n\ntype opts struct{ Transport string }\n\ntype srv struct{ c *http.Client }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	b, err := parser.ParseFile(fset, "b.go", "package p\n\nimport \"net/http\"\n\nfunc f(o *opts, s *srv, rt http.RoundTripper) {\n\to.Transport = \"stdio\"\n\ts.c.Transport = rt\n}\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, x := range inspectPackage(fset, []*ast.File{a, b}) {
		if !x.allowed {
			refused = append(refused, x.pos.String())
		}
	}
	if len(refused) != 1 || refused[0] != "b.go:7:2" {
		t.Fatalf("refused %v, want only the client rewire at b.go:7:2", refused)
	}
}

// TestOnlyNetroutesOwnBuildersAreExempt: the production walk skips exactly the
// bodies of NewTransport, NewClient and ExtraCAs in root/netroute, the
// constructor netroute/ca_test.go proves. Another function there, and the same
// three names in any other directory, are judged like the rest of the module.
func TestOnlyNetroutesOwnBuildersAreExempt(t *testing.T) {
	root := t.TempDir()
	builders := "package netroute\n\nimport (\n\t\"crypto/tls\"\n\t\"net/http\"\n)\n\nfunc ExtraCAs() *tls.Config { return nil }\n\nfunc NewTransport() *http.Transport {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\ttr.TLSClientConfig = ExtraCAs()\n\treturn tr\n}\n\nfunc NewClient() *http.Client { return &http.Client{Transport: NewTransport()} }\n"
	files := map[string]string{
		"netroute/ca.go":       builders,
		"netroute/other.go":    "package netroute\n\nimport \"net/http\"\n\nfunc Other() *http.Client { return &http.Client{} }\n",
		"x/netroute/ca.go":     builders,
		"y/route/uses.go":      "package route\n\nimport (\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar c = &http.Client{Transport: netroute.NewTransport()}\n",
		"y/route/uses_test.go": "package route\n\nimport \"net/http\"\n\nvar fixture = &http.Client{}\n",
	}
	for p, src := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := inspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	refusedIn := map[string]int{}
	allowedIn := map[string]int{}
	for _, x := range got {
		rel, _ := filepath.Rel(root, x.pos.Filename)
		rel = filepath.ToSlash(rel)
		if x.allowed {
			allowedIn[rel]++
		} else {
			refusedIn[rel]++
		}
	}
	if refusedIn["netroute/ca.go"] != 0 || allowedIn["netroute/ca.go"] != 0 {
		t.Errorf("the module's own builders were judged: %d refused, %d allowed", refusedIn["netroute/ca.go"], allowedIn["netroute/ca.go"])
	}
	if refusedIn["netroute/other.go"] == 0 {
		t.Errorf("a bare client in netroute/ beside the builders was not refused")
	}
	if refusedIn["x/netroute/ca.go"] == 0 {
		t.Errorf("a second package named netroute borrowed the builders' exemption")
	}
	if refusedIn["y/route/uses.go"] != 0 || allowedIn["y/route/uses.go"] != 1 {
		t.Errorf("the client on netroute.NewTransport: %d refused, %d allowed; want 0 and 1", refusedIn["y/route/uses.go"], allowedIn["y/route/uses.go"])
	}
	if refusedIn["y/route/uses_test.go"] != 0 {
		t.Errorf("a test fixture was judged")
	}
}

// TestTheGuardLoadsNetHTTPsTypes: every rule reads Go's own net/http objects.
// If the toolchain cannot supply them, or net/http renames one, a rule would go
// silently dead; fail here instead, so a weaker guard is never green.
func TestTheGuardLoadsNetHTTPsTypes(t *testing.T) {
	h, err := sharedImporter.httpObjects()
	if err != nil {
		t.Fatalf("the guard cannot load net/http's types: %v", err)
	}
	for name, v := range map[string]*types.Var{
		"Client.Transport":          h.clientTransport,
		"Transport.TLSClientConfig": h.tlsClientConfig,
		"Transport.Proxy":           h.proxy,
		"Transport.DialTLS":         h.dialTLS,
		"Transport.DialTLSContext":  h.dialTLSContext,
	} {
		if v == nil {
			t.Errorf("net/http has no field %s", name)
		}
	}
	if len(h.defaults) != 6 {
		t.Errorf("net/http declares %d of DefaultTransport, DefaultClient, Get, Head, Post and PostForm; want 6", len(h.defaults))
	}
	tlsPkg, err := sharedImporter.Import("crypto/tls")
	if err != nil || tlsPkg.Scope().Lookup("Config") == nil {
		t.Fatalf("the guard cannot load crypto/tls's Config, which netroute.ExtraCAs returns: %v", err)
	}
}

// TestTheGuardsNetrouteModelIsNetroutes: the guard types a value taken from
// netroute's builders by its own model of them. Each modelled builder must
// exist in netroute/ with the same result type, or the guard reasons about a
// netroute that is not this module's.
func TestTheGuardsNetrouteModelIsNetroutes(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	paths, err := filepath.Glob(filepath.Join("..", "netroute", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	conf := types.Config{Importer: sharedImporter, Error: func(error) {}}
	real, _ := conf.Check(netroutePath, fset, files, nil)
	model, err := sharedImporter.Import(netroutePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range model.Scope().Names() {
		m := model.Scope().Lookup(name).Type().(*types.Signature)
		obj := real.Scope().Lookup(name)
		if obj == nil {
			t.Fatalf("the guard models netroute.%s, which netroute/ does not declare", name)
		}
		r, ok := obj.Type().(*types.Signature)
		if !ok || r.Results().Len() != 1 || r.Results().At(0).Type().String() != m.Results().At(0).Type().String() {
			t.Fatalf("netroute.%s is %v; the guard models it returning %v", name, obj.Type(), m.Results().At(0).Type())
		}
	}
}

// TestTheGuardReadsNoTestFile: a test may build a client for a fixture; only
// production code can make a session fail.
func TestTheGuardReadsNoTestFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prod.go"), []byte("package p\n\nvar x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "fixture_test.go"), []byte("package p\n\nvar y = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files, err := productionFiles(root, fset)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || fset.Position(files[0].Pos()).Filename != filepath.Join(root, "prod.go") {
		t.Fatalf("the guard read %d file(s); want only prod.go", len(files))
	}
}

// wantRefusals parses src as name and requires exactly the refusals at want,
// in order, each naming the cause and the fix.
func wantRefusals(t *testing.T, name, src string, want ...string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, x := range inspectFile(fset, f) {
		if x.allowed {
			continue
		}
		refused = append(refused, x.pos.String())
		if !strings.Contains(x.why, "NODE_EXTRA_CA_CERTS") || !strings.Contains(x.why, "netroute") {
			t.Errorf("a refusal does not name the cause and the fix: %s", x.why)
		}
	}
	if strings.Join(refused, " ") != strings.Join(want, " ") {
		t.Fatalf("refused %v, want %v", refused, want)
	}
}

// finding is one client construction or rewire and whether a proof allowed it.
type finding struct {
	pos     token.Position
	allowed bool
	why     string
}

const (
	clientMessage           = "builds an *http.Client outside a CA-aware constructor: inside a Remote Control session this client honours the proxy the edge exports but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its TLS handshake fails before any request leaves; build it with netroute.NewClient, or set its Transport to netroute.NewTransport(), a local bound only to it, or a transport literal (#822, #829)"
	transportMessage        = "builds an http.Transport that honours a proxy but not the CA a Remote Control session signs every host with (NODE_EXTRA_CA_CERTS), so its TLS handshake fails inside the edge; take netroute.NewTransport, write TLSClientConfig: netroute.ExtraCAs() inline beside the Proxy with no DialTLS override, or leave Proxy nil for a direct route (#829)"
	typeUseMessage          = "names http.Client or http.Transport outside a literal or a pointer type, or holds one by value where it can start out zero (new, a variable declared without a value, a value or embedded field, parameter or result, a type alias or definition, a conversion, a type argument, a value type assertion): this guard cannot see the transport such a client dials with, and a zero http.Client honours the session's proxy but not its CA (NODE_EXTRA_CA_CERTS); hold a *http.Client from netroute.NewClient or a transport from netroute.NewTransport (#829)"
	defaultClientMessage    = "uses http.DefaultClient (directly or through http.Get, Head, Post or PostForm): it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so the call fails inside a Remote Control session; call Do on a client from netroute.NewClient (#829)"
	defaultTransportMessage = "uses http.DefaultTransport: it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its handshake fails inside a Remote Control session; take netroute.NewTransport, the one CA-aware clone of it (#822, #829)"
	dotImportMessage        = "dot-imports net/http or netroute, which hides from a reader the clients the file builds and the builder it calls; a client that honours the session's proxy but not NODE_EXTRA_CA_CERTS fails its handshake inside a Remote Control session; import the package by name and build clients with netroute.NewClient (#829)"
	tlsClientConfigMessage  = "names a transport's TLSClientConfig outside its literal: that config holds the CA a Remote Control session signs every host with (NODE_EXTRA_CA_CERTS, from netroute.ExtraCAs), and whatever reads it can edit, share or replace it, so the client's handshake fails inside the edge; take netroute.NewTransport, or write TLSClientConfig: netroute.ExtraCAs() in the transport literal, and never touch the field afterwards (#829)"
	mutationMessage         = "rewires a client or transport after construction (a client's Transport, a transport's Proxy, DialTLS or DialTLSContext, or the whole value; assigned, range-assigned or through an address taken) with a value this guard cannot prove trusts the session's CA (NODE_EXTRA_CA_CERTS); build the finished client with netroute.NewClient or netroute.NewTransport, or a transport literal with Proxy nil (#829)"
)

const (
	// guardHead is the import set every row of the bypass table may use; the
	// blank declarations keep each import used.
	guardHead = "package p\n\nimport (\n\t\"context\"\n\t\"net\"\n\t\"net/http\"\n\t\"time\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar (\n\t_ context.Context\n\t_ net.Conn\n\t_ = time.Second\n\t_ = netroute.ExtraCAs\n)\n\n"
	// netroutePath is the one package whose builders prove a transport.
	netroutePath = "github.com/akleizor/ccmorph/netroute"
)

// guardSrc is a row body under the shared import set.
func guardSrc(body string) string { return guardHead + body + "\n" }

// tlsSrc is a row body under the shared import set plus crypto/tls.
func tlsSrc(body string) string {
	return "package p\n\nimport (\n\t\"crypto/tls\"\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar (\n\t_ *tls.Config\n\t_ *http.Client\n\t_ = netroute.ExtraCAs\n)\n\n" + body + "\n"
}

// fileSrc is a whole file whose text follows the package clause, including its
// own imports.
func fileSrc(rest string) string { return "package p\n\n" + rest }

// guardImporter loads net/http and crypto/tls from the toolchain, so Go itself
// says which type a literal builds and which field a selector names, and gives
// this module's netroute its three builders (netrouteModel). Every other import
// fails, and go/types then gives it an empty package and carries on: its name
// still resolves to its path, its members resolve to nothing. One instance
// serves the whole test binary, so each package is read once.
type guardImporter struct {
	std      types.Importer
	netroute *types.Package
	http     *httpObjects
}

func (g *guardImporter) Import(path string) (*types.Package, error) {
	if path == netroutePath {
		return g.netrouteModel()
	}
	if path != "net/http" && path != "crypto/tls" {
		return nil, errors.New("the client guard loads only net/http, crypto/tls and netroute's builders")
	}
	if g.std == nil {
		g.std = importer.Default()
	}
	return g.std.Import(path)
}

// netrouteModel is netroute as the guard sees it from outside: NewTransport,
// NewClient and ExtraCAs with their real result types, nothing else.
// TestTheGuardsNetrouteModelIsNetroutes holds it to netroute/ca.go.
func (g *guardImporter) netrouteModel() (*types.Package, error) {
	if g.netroute != nil {
		return g.netroute, nil
	}
	httpPkg, err := g.Import("net/http")
	if err != nil {
		return nil, err
	}
	tlsPkg, err := g.Import("crypto/tls")
	if err != nil {
		return nil, err
	}
	pkg := types.NewPackage(netroutePath, "netroute")
	ptr := func(p *types.Package, name string) types.Type {
		return types.NewPointer(p.Scope().Lookup(name).Type())
	}
	declare := func(name string, params *types.Tuple, result types.Type) {
		results := types.NewTuple(types.NewVar(token.NoPos, pkg, "", result))
		pkg.Scope().Insert(types.NewFunc(token.NoPos, pkg, name, types.NewSignature(nil, params, results, false)))
	}
	declare("NewTransport", nil, ptr(httpPkg, "Transport"))
	declare("ExtraCAs", nil, ptr(tlsPkg, "Config"))
	declare("NewClient", types.NewTuple(types.NewVar(token.NoPos, pkg, "timeout", types.Typ[types.Int64])), ptr(httpPkg, "Client"))
	pkg.MarkComplete()
	g.netroute = pkg
	return pkg, nil
}

// httpObjects are the net/http objects the rules read, each Go's own: a
// selector or key is judged by the object it resolves to, never its spelling.
type httpObjects struct {
	client, transport types.Type
	clientTransport   *types.Var
	tlsClientConfig   *types.Var
	proxy             *types.Var
	dialTLS           *types.Var
	dialTLSContext    *types.Var
	defaults          map[types.Object]string
}

func (g *guardImporter) httpObjects() (*httpObjects, error) {
	if g.http != nil {
		return g.http, nil
	}
	pkg, err := g.Import("net/http")
	if err != nil {
		return nil, err
	}
	client, transport := pkg.Scope().Lookup("Client"), pkg.Scope().Lookup("Transport")
	if client == nil || transport == nil {
		return nil, errors.New("net/http declares no Client or Transport")
	}
	field := func(t types.Type, name string) *types.Var {
		s, ok := t.Underlying().(*types.Struct)
		if !ok {
			return nil
		}
		for i := 0; i < s.NumFields(); i++ {
			if s.Field(i).Name() == name {
				return s.Field(i)
			}
		}
		return nil
	}
	h := &httpObjects{
		client:          client.Type(),
		transport:       transport.Type(),
		clientTransport: field(client.Type(), "Transport"),
		tlsClientConfig: field(transport.Type(), "TLSClientConfig"),
		proxy:           field(transport.Type(), "Proxy"),
		dialTLS:         field(transport.Type(), "DialTLS"),
		dialTLSContext:  field(transport.Type(), "DialTLSContext"),
		defaults:        map[types.Object]string{},
	}
	if o := pkg.Scope().Lookup("DefaultTransport"); o != nil {
		h.defaults[o] = defaultTransportMessage
	}
	for _, name := range []string{"DefaultClient", "Get", "Head", "Post", "PostForm"} {
		if o := pkg.Scope().Lookup(name); o != nil {
			h.defaults[o] = defaultClientMessage
		}
	}
	g.http = h
	return h, nil
}

var sharedImporter = &guardImporter{}

// builderNames are netroute's three constructors.
var builderNames = map[string]bool{"NewTransport": true, "NewClient": true, "ExtraCAs": true}

// inspectFile reports every client construction, transport construction and
// rewire in f, refused unless its own transport proves it CA-aware or direct.
func inspectFile(fset *token.FileSet, f *ast.File) []finding {
	return inspectPackage(fset, []*ast.File{f})
}

// inspectPackage type-checks the files of one package together and inspects
// each: a receiver whose type another file of the package declares still
// resolves. Check's errors are expected (every import but net/http, crypto/tls
// and netroute is unresolved) and ignored; what resolves is Go's answer.
func inspectPackage(fset *token.FileSet, files []*ast.File) []finding {
	return inspectPackageExcept(fset, files, nil)
}

// inspectTree judges every production package under root. The bodies of
// root/netroute's NewTransport, NewClient and ExtraCAs are the constructor and
// are not judged; nothing else is skipped.
func inspectTree(root string) ([]finding, error) {
	fset := token.NewFileSet()
	files, err := productionFiles(root, fset)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, errors.New("the guard read no production file under " + root)
	}
	dir := filepath.Join(root, "netroute")
	var out []finding
	for _, pkg := range packagesOf(fset, files) {
		var skip []*ast.FuncDecl
		for _, f := range pkg {
			if f.Name.Name != "netroute" || filepath.Dir(fset.Position(f.Package).Filename) != dir {
				continue
			}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil && builderNames[fd.Name.Name] {
					skip = append(skip, fd)
				}
			}
		}
		out = append(out, inspectPackageExcept(fset, pkg, skip)...)
	}
	return out, nil
}

// checker is one package under Go's own name resolution: info says which object
// each identifier means, so whether web.Client is net/http's, whether nr is the
// netroute import or a parameter that hides it, and which variable a client
// dials with are answered by go/types, not by this guard matching spellings.
type checker struct {
	info   *types.Info
	h      *httpObjects
	parent map[ast.Node]ast.Node
	defs   map[types.Object]*ast.Ident
	uses   map[types.Object][]*ast.Ident
}

type rawFinding struct {
	pos     token.Pos
	allowed bool
	why     string
}

// inspectPackageExcept is inspectPackage with the findings inside skip dropped.
func inspectPackageExcept(fset *token.FileSet, files []*ast.File, skip []*ast.FuncDecl) []finding {
	if len(files) == 0 {
		return nil
	}
	h, err := sharedImporter.httpObjects()
	if err != nil {
		return []finding{{pos: fset.Position(files[0].Package), why: "the client guard cannot load net/http's types from the toolchain (" + err.Error() + "), so it cannot tell whether a client trusts the session's CA (NODE_EXTRA_CA_CERTS) or comes from netroute; fix the toolchain, never the rule (#829)"}}
	}
	info := &types.Info{
		Types:     map[ast.Expr]types.TypeAndValue{},
		Defs:      map[*ast.Ident]types.Object{},
		Uses:      map[*ast.Ident]types.Object{},
		Instances: map[*ast.Ident]types.Instance{},
	}
	conf := types.Config{Importer: sharedImporter, Error: func(error) {}}
	_, _ = conf.Check(files[0].Name.Name, fset, files, info)
	c := &checker{info: info, h: h, parent: map[ast.Node]ast.Node{}, defs: map[types.Object]*ast.Ident{}, uses: map[types.Object][]*ast.Ident{}}
	for _, f := range files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return false
			}
			if len(stack) > 0 {
				c.parent[n] = stack[len(stack)-1]
			}
			stack = append(stack, n)
			return true
		})
	}
	for id, obj := range info.Defs {
		if obj != nil {
			c.defs[obj] = id
		}
	}
	for id, obj := range info.Uses {
		c.uses[obj] = append(c.uses[obj], id)
	}
	var raw []rawFinding
	add := func(n ast.Node, allowed bool, why string) {
		raw = append(raw, rawFinding{pos: n.Pos(), allowed: allowed, why: why})
	}
	for _, f := range files {
		c.inspect(f, add)
	}
	// A client or transport type named anywhere but under a pointer star or as
	// a composite literal's type.
	for e, tv := range info.Types {
		if tv.IsType() && c.isHTTPValue(tv.Type) && !c.framed(e) {
			add(e, false, typeUseMessage)
		}
	}
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].pos != raw[j].pos {
			return raw[i].pos < raw[j].pos
		}
		return raw[i].why < raw[j].why
	})
	var out []finding
	seen := map[rawFinding]bool{}
	for _, r := range raw {
		if seen[r] || within(skip, r.pos) {
			continue
		}
		seen[r] = true
		out = append(out, finding{pos: fset.Position(r.pos), allowed: r.allowed, why: r.why})
	}
	return out
}

func within(decls []*ast.FuncDecl, pos token.Pos) bool {
	for _, d := range decls {
		if d.Pos() <= pos && pos < d.End() {
			return true
		}
	}
	return false
}

func (c *checker) inspect(f *ast.File, add func(ast.Node, bool, string)) {
	for _, spec := range dotImports(f) {
		add(spec, false, dotImportMessage)
	}
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.CompositeLit:
			t := deref(c.info.TypeOf(x))
			switch {
			case c.isClient(t):
				add(x, c.clientLitOK(x), clientMessage)
			case c.isTransport(t):
				add(x, c.transportLitOK(x), transportMessage)
			}
		case *ast.CallExpr:
			// A conversion to a client or transport makes one the guard never
			// saw built: (*http.Client)(p) of a look-alike struct is a client.
			if tv, ok := c.info.Types[x.Fun]; ok && tv.IsType() && c.isHTTPValue(deref(tv.Type)) {
				add(x, false, typeUseMessage)
			}
		case *ast.SelectorExpr:
			c.selector(x, add)
		case *ast.Ident:
			if obj := c.info.Uses[x]; obj != nil {
				if msg, ok := c.h.defaults[obj]; ok {
					add(x, false, msg)
				}
			}
			if v, ok := c.info.Defs[x].(*types.Var); ok && c.isHTTPValue(v.Type()) && !c.initialisedLocal(v, x) {
				add(x, false, typeUseMessage)
			}
			if inst, ok := c.info.Instances[x]; ok && inst.TypeArgs != nil {
				for i := 0; i < inst.TypeArgs.Len(); i++ {
					if c.isHTTPValue(inst.TypeArgs.At(i)) {
						add(x, false, typeUseMessage)
					}
				}
			}
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				var rhs ast.Expr
				if len(x.Lhs) == len(x.Rhs) {
					rhs = x.Rhs[i]
				}
				c.wholeWrite(lhs, rhs, add)
			}
		case *ast.RangeStmt:
			if x.Key != nil {
				c.wholeWrite(x.Key, nil, add)
			}
			if x.Value != nil {
				c.wholeWrite(x.Value, nil, add)
			}
		}
		return true
	})
}

// initialisedLocal: v is a function's local declared one-to-one with a value
// (probe := *client, c2 := probe). Every expression that yields a client or
// transport value is judged where it starts (a literal, a dereference of a
// pointer built elsewhere, or a field, result, element or assertion whose
// value type is refused), and a whole write to v later is refused by
// wholeWrite. Only a variable, field, parameter or result that can start out
// zero is refused: its nil Transport is http.DefaultTransport.
func (c *checker) initialisedLocal(v *types.Var, def *ast.Ident) bool {
	if v.IsField() || v.Pkg() == nil || v.Parent() == nil || v.Parent() == v.Pkg().Scope() {
		return false
	}
	switch p := c.parent[def].(type) {
	case *ast.AssignStmt:
		if p.Tok != token.DEFINE {
			return false
		}
		_, rhs := c.writeOf(def)
		return rhs != nil
	case *ast.ValueSpec:
		return len(p.Values) == len(p.Names)
	}
	return false
}

// selector judges a selector that names one of the guarded fields. A field Go
// resolves is judged by its object; one on a receiver Go cannot resolve (an
// import the guard does not load) is judged as net/http's by its name.
func (c *checker) selector(x *ast.SelectorExpr, add func(ast.Node, bool, string)) {
	obj := c.info.Uses[x.Sel]
	if obj == nil {
		if c.isPackage(x.X) {
			return
		}
		switch x.Sel.Name {
		case "TLSClientConfig":
			add(x, false, tlsClientConfigMessage)
		case "Proxy", "DialTLS", "DialTLSContext":
			if w, _ := c.writeOf(x); w {
				add(x, false, mutationMessage)
			}
		case "Transport":
			if w, rhs := c.writeOf(x); w {
				add(x, rhs != nil && c.transportValueOK(rhs), mutationMessage)
			}
		}
		return
	}
	v, ok := obj.(*types.Var)
	if !ok || !v.IsField() {
		return
	}
	switch v {
	case c.h.tlsClientConfig:
		add(x, false, tlsClientConfigMessage)
	case c.h.proxy, c.h.dialTLS, c.h.dialTLSContext:
		if w, _ := c.writeOf(x); w {
			add(x, false, mutationMessage)
		}
	case c.h.clientTransport:
		if w, rhs := c.writeOf(x); w {
			add(x, rhs != nil && c.transportValueOK(rhs), mutationMessage)
		}
	}
}

// wholeWrite refuses a write that replaces a whole client or transport value
// (*tr = *o, *c = http.Client{}, probe = http.Client{}), judged by the
// target's type or, when Go cannot resolve the target, by the value's. A
// variable's own declaration is judged by its Defs rule instead.
func (c *checker) wholeWrite(lhs, rhs ast.Expr, add func(ast.Node, bool, string)) {
	if id, ok := unparen(lhs).(*ast.Ident); ok && (id.Name == "_" || c.info.Defs[id] != nil) {
		return
	}
	target := c.info.TypeOf(lhs)
	if c.isHTTPValue(target) || (unresolved(target) && rhs != nil && c.isHTTPValue(c.info.TypeOf(rhs))) {
		add(lhs, false, mutationMessage)
	}
}

// unresolved: Go could not type the expression (an import the guard does not
// load), so the guard judges it by what is written into it.
func unresolved(t types.Type) bool {
	return t == nil || t == types.Typ[types.Invalid]
}

// writeOf says whether e is written to (assigned, range-assigned, incremented
// or has its address taken) and, for a plain one-to-one assignment, the value.
func (c *checker) writeOf(e ast.Node) (bool, ast.Expr) {
	n := e
	p := c.parent[n]
	for {
		pe, ok := p.(*ast.ParenExpr)
		if !ok {
			break
		}
		n, p = pe, c.parent[pe]
	}
	switch s := p.(type) {
	case *ast.AssignStmt:
		for i, l := range s.Lhs {
			if l != n {
				continue
			}
			if len(s.Lhs) == len(s.Rhs) && (s.Tok == token.ASSIGN || s.Tok == token.DEFINE) {
				return true, s.Rhs[i]
			}
			return true, nil
		}
	case *ast.IncDecStmt:
		return s.X == n, nil
	case *ast.RangeStmt:
		return s.Key == n || s.Value == n, nil
	case *ast.UnaryExpr:
		return s.Op == token.AND && s.X == n, nil
	}
	return false, nil
}

// clientLitOK: a client literal is proved by its own keyed Transport.
func (c *checker) clientLitOK(lit *ast.CompositeLit) bool {
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Transport" {
			return c.transportValueOK(kv.Value)
		}
	}
	return false
}

// transportValueOK: a call to netroute.NewTransport, a transport literal (judged
// on its own), or a local Go binds only to netroute.NewTransport.
func (c *checker) transportValueOK(v ast.Expr) bool {
	v = unparen(v)
	if c.isBuilderCall(v, "NewTransport") {
		return true
	}
	if u, ok := v.(*ast.UnaryExpr); ok && u.Op == token.AND {
		if lit, ok := unparen(u.X).(*ast.CompositeLit); ok && c.isTransport(c.info.TypeOf(lit)) {
			return true
		}
	}
	if id, ok := v.(*ast.Ident); ok {
		if lv, ok := c.info.Uses[id].(*types.Var); ok {
			return c.boundOnlyToNewTransport(lv)
		}
	}
	return false
}

// transportLitOK: direct (Proxy absent or the predeclared nil), or the CA from
// a netroute.ExtraCAs() call written in the literal, with no TLS dialer that
// would bypass TLSClientConfig.
func (c *checker) transportLitOK(lit *ast.CompositeLit) bool {
	var proxy, tlsCfg ast.Expr
	dialer := false
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return false
		}
		switch key.Name {
		case "Proxy":
			proxy = kv.Value
		case "TLSClientConfig":
			tlsCfg = kv.Value
		case "DialTLS", "DialTLSContext":
			dialer = true
		}
	}
	if proxy == nil || c.isNil(proxy) {
		return true
	}
	return tlsCfg != nil && c.isBuilderCall(tlsCfg, "ExtraCAs") && !dialer
}

// boundOnlyToNewTransport: v is a function's local, declared with a value, and
// every write Go records to it (in its function and any closure) is a one-to-one
// assignment of a netroute.NewTransport() call; its address is never taken.
func (c *checker) boundOnlyToNewTransport(v *types.Var) bool {
	if v.IsField() || v.Pkg() == nil || v.Parent() == nil || v.Parent() == v.Pkg().Scope() {
		return false
	}
	def := c.defs[v]
	if def == nil {
		return false
	}
	switch p := c.parent[def].(type) {
	case *ast.AssignStmt:
		w, rhs := c.writeOf(def)
		if !w || p.Tok != token.DEFINE || rhs == nil || !c.isBuilderCall(rhs, "NewTransport") {
			return false
		}
	case *ast.ValueSpec:
		if len(p.Values) != len(p.Names) {
			return false
		}
		for i, n := range p.Names {
			if n == def && !c.isBuilderCall(p.Values[i], "NewTransport") {
				return false
			}
		}
	default:
		return false
	}
	for _, id := range c.uses[v] {
		w, rhs := c.writeOf(id)
		if !w {
			continue
		}
		if rhs == nil || !c.isBuilderCall(rhs, "NewTransport") {
			return false
		}
	}
	return true
}

// isBuilderCall: e calls the package-level function name of this module's
// netroute, as Go resolves it (an alias counts, a local of the same name or a
// package of the same name does not).
func (c *checker) isBuilderCall(e ast.Expr, name string) bool {
	call, ok := unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	var id *ast.Ident
	switch fn := unparen(call.Fun).(type) {
	case *ast.Ident:
		id = fn
	case *ast.SelectorExpr:
		id = fn.Sel
	default:
		return false
	}
	obj, ok := c.info.Uses[id].(*types.Func)
	if !ok || obj.Pkg() == nil || obj.Pkg().Path() != netroutePath || obj.Name() != name {
		return false
	}
	return obj.Pkg().Scope().Lookup(name) == obj
}

func (c *checker) isNil(e ast.Expr) bool {
	id, ok := unparen(e).(*ast.Ident)
	return ok && c.info.Uses[id] == types.Universe.Lookup("nil")
}

func (c *checker) isPackage(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	_, pkg := c.info.Uses[id].(*types.PkgName)
	return pkg
}

// framed: a type expression under a pointer star or as a composite literal's
// type, the two places naming http.Client or http.Transport is allowed.
func (c *checker) framed(e ast.Expr) bool {
	var n ast.Node = e
	p := c.parent[n]
	for {
		pe, ok := p.(*ast.ParenExpr)
		if !ok {
			break
		}
		n, p = pe, c.parent[pe]
	}
	switch s := p.(type) {
	case *ast.StarExpr:
		return s.X == n
	case *ast.CompositeLit:
		return s.Type == n
	}
	return false
}

// isClient and isTransport compare underlying types, so an alias or a type
// defined from net/http's counts as the same.
func (c *checker) isClient(t types.Type) bool {
	return t != nil && types.Identical(t.Underlying(), c.h.client.Underlying())
}

func (c *checker) isTransport(t types.Type) bool {
	return t != nil && types.Identical(t.Underlying(), c.h.transport.Underlying())
}

// isHTTPValue: a client or transport held by value, not by pointer.
func (c *checker) isHTTPValue(t types.Type) bool {
	return c.isClient(t) || c.isTransport(t)
}

func deref(t types.Type) types.Type {
	if p, ok := t.(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func dotImports(f *ast.File) []*ast.ImportSpec {
	var out []*ast.ImportSpec
	for _, spec := range f.Imports {
		if spec.Name == nil || spec.Name.Name != "." {
			continue
		}
		path, err := strconv.Unquote(spec.Path.Value)
		if err == nil && (path == "net/http" || path == netroutePath) {
			out = append(out, spec)
		}
	}
	return out
}

func unparen(expr ast.Expr) ast.Expr {
	for {
		p, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = p.X
	}
}

// packagesOf groups files by directory and package clause, in a stable order,
// so each package is type-checked whole.
func packagesOf(fset *token.FileSet, files []*ast.File) [][]*ast.File {
	groups := map[string][]*ast.File{}
	var keys []string
	for _, f := range files {
		k := filepath.Dir(fset.Position(f.Package).Filename) + "\x00" + f.Name.Name
		if _, seen := groups[k]; !seen {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], f)
	}
	sort.Strings(keys)
	out := make([][]*ast.File, 0, len(keys))
	for _, k := range keys {
		out = append(out, groups[k])
	}
	return out
}

// productionFiles parses every non-test Go file under root, skipping hidden
// directories (this repo's machine-local state) and testdata.
func productionFiles(root string, fset *token.FileSet) ([]*ast.File, error) {
	var files []*ast.File
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	return files, err
}
