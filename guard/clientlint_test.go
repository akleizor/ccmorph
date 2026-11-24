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
// A client is allowed only by its own Transport: netroute.NewTransport, a
// transport literal carrying netroute.ExtraCAs, or a literal setting Proxy: nil.
// A transport is allowed the same way. Every name is Go's own: each package is
// type-checked whole (go/types, with net/http and crypto/tls from the
// toolchain), so an alias is the same package, a local that hides an import is
// no package, and a proof belongs to the variable Go binds, never to a
// same-named one in another block. A rewire after construction is judged by
// the type Go says owns the field, whether it is assigned, incremented,
// range-assigned or reached through an address taken; a CertPool overwritten in
// place and a fresh TLS config overwritten with another config's CA state are
// refused, and a package named netroute outside netroute/ is refused, because
// only there is a bare NewTransport or ExtraCAs trusted (#829).
func TestEveryProductionHTTPClientTrustsTheSessionsExtraCA(t *testing.T) {
	root := ".."
	fset := token.NewFileSet()
	files, err := productionFiles(root, fset)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the guard read no production file")
	}
	refused, allowed := 0, 0
	for _, pkg := range packagesOf(fset, files) {
		for _, x := range inspectPackage(fset, pkg) {
			if !x.allowed {
				refused++
				t.Errorf("%s: %s", x.pos, x.why)
				continue
			}
			allowed++
		}
	}
	for _, p := range strayNetroute(root, fset, files) {
		t.Errorf("%s: declares package netroute outside the module's netroute/: the guard trusts a bare NewTransport or ExtraCAs (the CA from NODE_EXTRA_CA_CERTS) only there; rename this package (#829)", p)
	}
	if refused == 0 && allowed == 0 {
		t.Fatalf("the guard found no http client construction under ..: it is not reading the module")
	}
}

// TestTheWholeModuleGuardSeesEveryBypass pins every way a file can build or
// rewire a client without the session's CA: the plain literal, a sibling
// initializer in an allowed declaration, a method borrowing the constructor's
// name, a transport it names that is not the builder's, an aliased or twice
// imported net/http, a dot import, a package that is not this module's
// netroute, a client or transport named outside a literal or a pointer type,
// the http.DefaultClient helpers, a standalone proxied transport, a mutation
// after construction (Transport, Proxy or TLSClientConfig, however it is
// written), a shared CertPool overwritten in place, and a fresh TLS config
// overwritten with another config's CA state. Each row
// is a whole file; "refused" needs at least one refused finding, "allowed" a
// finding and none refused, "none" no finding at all.
func TestTheWholeModuleGuardSeesEveryBypass(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		// The proof is the client's own transport, never its enclosing scope.
		{"bare client", guardSrc("var c = &http.Client{}"), "refused"},
		{"timeout-only client", guardSrc("var c = &http.Client{Timeout: time.Second}"), "refused"},
		{"sibling initializer", guardSrc("var a, b = caClient(), &http.Client{}"), "refused"},
		{"direct client", guardSrc("var c = &http.Client{Transport: &http.Transport{Proxy: nil}}"), "allowed"},
		{"proxied transport literal", guardSrc("var c = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}"), "refused"},
		{"transport literal with the CA", guardSrc("func f() *http.Client {\n\treturn &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: netroute.ExtraCAs()}}\n}"), "allowed"},
		{"builder inline", guardSrc("func f() *http.Client { return &http.Client{Transport: netroute.NewTransport()} }"), "allowed"},
		{"builder bound", guardSrc("func f() *http.Client {\n\ttr := netroute.NewTransport()\n\treturn &http.Client{Transport: tr}\n}"), "allowed"},
		{"unrelated builder call", guardSrc("func f() *http.Client {\n\t_ = netroute.NewClient(1)\n\treturn &http.Client{}\n}"), "refused"},
		{"bare client beside a builder", "package p\n\nimport \"net/http\"\n\nfunc f() { _ = netroute.NewClient(1); _ = &http.Client{} }\n", "refused"},
		{"transport parameter", guardSrc("func f(tr *http.Transport) *http.Client { return &http.Client{Transport: tr} }"), "refused"},
		{"clone without the CA", guardSrc("func f() *http.Client {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"clone then the CA", guardSrc("func f() *http.Client {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\ttr.TLSClientConfig = netroute.ExtraCAs()\n\treturn &http.Client{Transport: tr}\n}"), "allowed"},
		{"builder then wiped", guardSrc("func f() *http.Client {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig = nil\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"method borrowing the name", guardSrc("type bypass struct{}\n\nfunc (bypass) caClient() *http.Client { return &http.Client{} }"), "refused"},
		{"DefaultTransport returned bare", guardSrc("func clone() *http.Transport { return http.DefaultTransport.(*http.Transport).Clone() }"), "refused"},

		// A file's own names: an alias is the same package; a dot import hides
		// the names; only this module's netroute is trusted.
		{"aliased client", fileSrc(`import web "net/http"

func f() { _ = &web.Client{} }
`), "refused"},
		{"aliased client value", fileSrc(`import web "net/http"

var c = web.Client{}
`), "refused"},
		{"aliased new", fileSrc(`import web "net/http"

var c = new(web.Client)
`), "refused"},
		{"aliased clone", fileSrc(`import web "net/http"

func f() *web.Transport { return web.DefaultTransport.(*web.Transport).Clone() }
`), "refused"},
		{"aliased transport literal", fileSrc(`import web "net/http"

var t = &web.Transport{Proxy: web.ProxyFromEnvironment}
`), "refused"},
		{"net/http imported twice", `package p

import (
	"net/http"
	web "net/http"
)

var a *http.Client
var c = &web.Client{}
`, "refused"},
		{"dot import of net/http", fileSrc(`import . "net/http"

var c = &Client{}
`), "refused"},
		{"dot import of netroute", `package p

import (
	"net/http"

	. "github.com/akleizor/ccmorph/netroute"
)

var c = &http.Client{Transport: NewTransport()}
`, "refused"},
		{"a foreign netroute is no proof", `package p

import (
	"net/http"

	netroute "example.com/fake"
)

var c = &http.Client{Transport: netroute.NewTransport()}
`, "refused"},
		{"netroute never imported", `package p

import "net/http"

var c = &http.Client{Transport: netroute.NewTransport()}
`, "refused"},
		{"aliased client on the builder", `package p

import (
	web "net/http"

	"github.com/akleizor/ccmorph/netroute"
)

var c = &web.Client{Transport: netroute.NewTransport()}
`, "allowed"},
		{"aliased netroute builder", `package p

import (
	"net/http"

	nr "github.com/akleizor/ccmorph/netroute"
)

var c = &http.Client{Transport: nr.NewTransport()}
`, "allowed"},
		{"inside netroute", `package netroute

import "net/http"

func NewClient() *http.Client { return &http.Client{Transport: NewTransport()} }
`, "allowed"},

		// Every other way to name a client or transport.
		{"new client", guardSrc("var c = new(http.Client)"), "refused"},
		{"zero-value client", guardSrc("func f() {\n\tvar c http.Client\n\t_ = c\n}"), "refused"},
		{"value field", guardSrc("type s struct{ c http.Client }"), "refused"},
		{"embedded client", guardSrc("type s struct{ http.Client }"), "refused"},
		{"client type alias", guardSrc("type C = http.Client\n\nvar c = &C{}"), "refused"},
		{"defined client type", guardSrc("type C http.Client\n\nvar c = &C{}"), "refused"},
		{"transport type alias", guardSrc("type T = http.Transport\n\nvar t = &T{Proxy: http.ProxyFromEnvironment}"), "refused"},
		{"type argument", guardSrc("func mk[T any]() *T { return new(T) }\n\nvar c = mk[http.Client]()"), "refused"},
		{"elided slice element", guardSrc("var cs = []*http.Client{{}}"), "refused"},
		{"elided map element", guardSrc(`var ts = map[string]*http.Transport{"a": {Proxy: http.ProxyFromEnvironment}}`), "refused"},
		{"pointers only", guardSrc("type s struct{ c *http.Client }\n\nfunc f(c *http.Client) *http.Transport { return nil }"), "none"},
		{"DefaultClient", guardSrc("func f(r *http.Request) { _, _ = http.DefaultClient.Do(r) }"), "refused"},
		{"http.Get", guardSrc(`func f() { _, _ = http.Get("https://models.dev") }`), "refused"},
		{"http.Head", guardSrc(`func f() { _, _ = http.Head("https://models.dev") }`), "refused"},
		{"http.Post", guardSrc(`func f() { _, _ = http.Post("https://models.dev", "text/plain", nil) }`), "refused"},
		{"http.PostForm", guardSrc(`func f() { _, _ = http.PostForm("https://models.dev", nil) }`), "refused"},
		{"proxied transport handed out", guardSrc("func f(use func(http.RoundTripper)) { use(&http.Transport{Proxy: http.ProxyFromEnvironment}) }"), "refused"},
		{"direct transport handed out", guardSrc("func f(use func(http.RoundTripper)) { use(&http.Transport{}) }"), "allowed"},

		// A rewire after construction is checked like the literal it replaces.
		{"opaque transport assignment", guardSrc("func f(rt http.RoundTripper) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = rt\n\treturn c\n}"), "refused"},
		{"builder transport assignment", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport = netroute.NewTransport()\n\treturn c\n}"), "allowed"},
		{"proxy set later", guardSrc("func f() *http.Client {\n\ttr := &http.Transport{}\n\ttr.Proxy = http.ProxyFromEnvironment\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"assigned through TLSClientConfig", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"TLS config through the assertion", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Transport.(*http.Transport).TLSClientConfig = nil\n\treturn c\n}"), "refused"},
		{"ExtraCAs bound then set", guardSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\ttr.TLSClientConfig = cfg\n\treturn tr\n}"), "allowed"},
		{"builder rebound", guardSrc("func f(o *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\ttr = o\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"one branch only", guardSrc("func f(x bool) *http.Client {\n\tvar tr *http.Transport\n\tif x {\n\t\ttr = netroute.NewTransport()\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"parameter rebound in a branch", guardSrc("func f(tr *http.Transport, x bool) *http.Client {\n\tif x {\n\t\ttr = netroute.NewTransport()\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"address taken", guardSrc("func f(set func(**http.Transport)) *http.Client {\n\ttr := netroute.NewTransport()\n\tset(&tr)\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"config rebound", guardSrc("func f(o *tls.Config) *http.Client {\n\tcfg := netroute.ExtraCAs()\n\tcfg = o\n\treturn &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}}\n}"), "refused"},

		// A local name shadows the import: the qualifier is no package there.
		{"shadowed netroute alias", `package route

import (
	"net/http"
	nr "github.com/akleizor/ccmorph/netroute"
)

var safe = nr.NewClient(0)

type factory struct{ tr http.RoundTripper }

func (f factory) NewTransport() http.RoundTripper { return f.tr }

func wrap(nr factory) *http.Client { return &http.Client{Transport: nr.NewTransport()} }

var broken = wrap(factory{})
`, "refused"},
		{"shadowed net/http alias", `package p

import web "net/http"

type Client struct{}

func f() {
	web := struct{ Client int }{}
	_ = web.Client
	var c Client
	_ = c
}
`, "none"},

		// A field with an HTTP name on a non-HTTP receiver is no rewiring.
		{"non-HTTP field names", `package settings

import "net/http"

type Options struct {
	Client          *http.Client
	Transport       string
	Proxy           string
	TLSClientConfig string
}

func configure(o *Options) *http.Client {
	o.Transport = "stdio"
	o.Proxy = "inherit"
	o.TLSClientConfig = "system"
	return o.Client
}
`, "none"},

		// Scope is lexical: a local hides the package only inside its block.
		{"nested block does not shadow", `package p

import web "net/http"

func f() {
	{ web := struct{ Client int }{}; _ = web.Client }
	_ = &web.Client{}
}
`, "refused"},
		{"a local shadows inside its block", `package p

import web "net/http"

func f() {
	web := struct{ Client int }{}
	_ = web.Client
}
`, "none"},
		{"an initializer sees the import", `package p

import web "net/http"

func f() {
	web := web.Client{}
	_ = web
}
`, "refused"},
		{"if init does not leak", `package p

import web "net/http"

func f() {
	if web := 1; web > 0 {
		_ = web
	}
	_ = &web.Client{}
}
`, "refused"},
		{"if init shadows inside its body", `package p

import (
	"net/http"

	"github.com/akleizor/ccmorph/netroute"
)

type factory struct{}

func (factory) NewTransport() http.RoundTripper { return nil }

func f(x bool) *http.Client {
	if netroute := (factory{}); x {
		return &http.Client{Transport: netroute.NewTransport()}
	}
	return nil
}
`, "refused"},

		// Go's own resolution decides which variable a client dials with: a
		// same-named local in another block is another variable, a package
		// variable carries no proof, and a rewire is judged by the type Go says
		// owns the field, whatever the receiver is called.
		{"a package variable behind a block-local builder", guardSrc("var tr http.RoundTripper\n\nfunc f() *http.Client {\n\t{\n\t\ttr := netroute.NewTransport()\n\t\t_ = tr\n\t}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"an outer clone borrows an inner clone's CA", guardSrc("func f() *http.Transport {\n\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\tif true {\n\t\ttr := http.DefaultTransport.(*http.Transport).Clone()\n\t\ttr.TLSClientConfig = netroute.ExtraCAs()\n\t\t_ = tr\n\t}\n\treturn tr\n}"), "refused"},
		{"a struct field's client rewired", guardSrc("type srv struct{ c *http.Client }\n\nfunc f(rt http.RoundTripper) *srv {\n\ts := &srv{c: netroute.NewClient(time.Second)}\n\ts.c.Transport = rt\n\treturn s\n}"), "refused"},
		{"the CA wiped through an asserted local", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tt := c.Transport.(*http.Transport)\n\tt.TLSClientConfig = nil\n\treturn c\n}"), "refused"},
		{"a helper's client rewired", guardSrc("func get() *http.Client { return netroute.NewClient(time.Second) }\n\nfunc f(rt http.RoundTripper) *http.Client {\n\tc := get()\n\tc.Transport = rt\n\treturn c\n}"), "refused"},
		{"an embedded transport's CA wiped", guardSrc("type wrap struct{ *http.Transport }\n\nfunc f() wrap {\n\tw := wrap{netroute.NewTransport()}\n\tw.TLSClientConfig = nil\n\treturn w\n}"), "refused"},
		{"a builder's transport overwritten whole", guardSrc("func f(o *http.Transport) *http.Client {\n\ttr := netroute.NewTransport()\n\t*tr = *o\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a client overwritten whole through a field", guardSrc("type srv struct{ c *http.Client }\n\nfunc f(s *srv, o *http.Client) { *s.c = *o }"), "refused"},
		{"the CA config edited through its local", guardSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\tcfg.RootCAs = nil\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}\n}"), "refused"},
		{"a server-only TLS config is no client rewire", fileSrc("import (\n\t\"crypto/tls\"\n\t\"net/http\"\n)\n\nfunc serve(saved *http.Client, certs []tls.Certificate) (*http.Client, *http.Server) {\n\tcfg := &tls.Config{}\n\tcfg.Certificates = certs\n\treturn saved, &http.Server{TLSConfig: cfg}\n}\n"), "none"},
		// A crypto/tls Config is judged by the type Go resolves, never by its
		// spelling: one built here (a server's) is no client rewire, any other
		// stays refused, and a whole overwrite is refused through any alias.
		{"a server config through a type alias", tlsSrc("type serverTLS = tls.Config\n\nfunc serve(saved *http.Client, certs []tls.Certificate) (*http.Client, *http.Server) {\n\tcfg := &serverTLS{}\n\tcfg.Certificates = certs\n\treturn saved, &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a server config of a type defined from tls.Config", tlsSrc("type serverTLS tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tcfg := &serverTLS{}\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: (*tls.Config)(cfg)}\n}"), "none"},
		{"a server config under an aliased crypto/tls", fileSrc("import (\n\tt \"crypto/tls\"\n\t\"net/http\"\n)\n\nfunc serve(certs []t.Certificate) *http.Server {\n\tcfg := &t.Config{}\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: cfg}\n}\n"), "none"},
		{"a server config from new", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := new(tls.Config)\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a server config value", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := tls.Config{}\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: &cfg}\n}"), "none"},
		{"a zero-value server config", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tvar cfg tls.Config\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: &cfg}\n}"), "none"},
		{"a package-level server config value", tlsSrc("var serverCfg tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tserverCfg.Certificates = certs\n\treturn &http.Server{TLSConfig: &serverCfg}\n}"), "none"},
		{"an aliased config read back and edited", tlsSrc("type clientTLS = tls.Config\n\nfunc f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tvar cfg *clientTLS = tr.TLSClientConfig\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a transport's config overwritten whole through an alias", tlsSrc("type clientTLS = tls.Config\n\nfunc f() *http.Client {\n\ttr := netroute.NewTransport()\n\tvar p *clientTLS = tr.TLSClientConfig\n\t*p = clientTLS{InsecureSkipVerify: true}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a transport's config overwritten whole through a defined type", tlsSrc("type clientTLS tls.Config\n\nfunc f() *http.Client {\n\ttr := netroute.NewTransport()\n\tp := (*clientTLS)(tr.TLSClientConfig)\n\t*p = clientTLS{InsecureSkipVerify: true}\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a copy of the CA config edited and handed to a transport", tlsSrc("func f() *http.Transport {\n\tcfg := *netroute.ExtraCAs()\n\tcfg.InsecureSkipVerify = true\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: &cfg}\n}"), "refused"},
		{"a fresh aliased config rebound to the transport's", tlsSrc("type serverTLS = tls.Config\n\nfunc f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := &serverTLS{}\n\tcfg = tr.TLSClientConfig\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a fresh config whose address is taken", tlsSrc("type serverTLS = tls.Config\n\nfunc f(set func(**tls.Config)) *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := &serverTLS{}\n\tset(&cfg)\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a config held in a struct field", tlsSrc("type holder struct{ cfg *tls.Config }\n\nfunc f() *http.Client {\n\ttr := netroute.NewTransport()\n\th := holder{}\n\th.cfg = tr.TLSClientConfig\n\th.cfg.InsecureSkipVerify = true\n\treturn &http.Client{Transport: tr}\n}"), "refused"},
		{"a parameter config written", tlsSrc("func harden(cfg *tls.Config) { cfg.InsecureSkipVerify = true }"), "refused"},
		{"a parameter named new makes no fresh config", tlsSrc("func f(new func(*tls.Config) *tls.Config) *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := new(tr.TLSClientConfig)\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"the transport's config written through a dereference", tlsSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tp := tr.TLSClientConfig\n\t(*p).InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a fresh server config of a defined type overwritten whole", tlsSrc("type serverTLS tls.Config\n\nfunc serve(saved *http.Client, certs []tls.Certificate) (*http.Client, *http.Server) {\n\tcfg := &serverTLS{}\n\t*cfg = serverTLS{Certificates: certs}\n\treturn saved, &http.Server{TLSConfig: (*tls.Config)(cfg)}\n}"), "none"},
		{"a fresh server config of an alias overwritten whole", tlsSrc("type serverTLS = tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tcfg := &serverTLS{}\n\t*cfg = serverTLS{Certificates: certs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config overwritten whole, then a field set", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := new(tls.Config)\n\t*cfg = tls.Config{MinVersion: tls.VersionTLS12}\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh config overwritten with a copy of the transport's carries its pool", tlsSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{}\n\t*cfg = *tr.TLSClientConfig\n\tcfg.InsecureSkipVerify = true\n\treturn tr, cfg\n}"), "refused"},
		{"the CA config overwritten whole", tlsSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\t*cfg = tls.Config{InsecureSkipVerify: true}\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}\n}"), "refused"},
		{"a parameter config overwritten whole", tlsSrc("func harden(cfg *tls.Config) { *cfg = tls.Config{InsecureSkipVerify: true} }"), "refused"},
		{"a fresh config rebound to the transport's, then overwritten whole", tlsSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{}\n\tcfg = tr.TLSClientConfig\n\t*cfg = tls.Config{InsecureSkipVerify: true}\n\treturn tr\n}"), "refused"},
		{"a builder transport overwritten whole stays a client rewire", tlsSrc("func f(o *http.Transport) *http.Transport {\n\ttr := netroute.NewTransport()\n\t*tr = *o\n\treturn tr\n}"), "refused"},

		// A fresh config overwritten whole may carry no other config's CA state,
		// and a CertPool is never overwritten in place unless built here: a
		// store through a pool a transport shares undoes the CA it trusts.
		{"a fresh config overwritten with the transport's pool, then the pool cleared", poolSrc("func route() *http.Transport {\n\ttr := netroute.NewTransport()\n\tif tr.TLSClientConfig != nil {\n\t\tcfg := &tls.Config{}\n\t\t*cfg = tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}\n\t\t*cfg.RootCAs = *x509.NewCertPool()\n\t}\n\treturn tr\n}"), "refused"},
		{"a fresh config overwritten with the transport's pool", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with the session CA's pool", poolSrc("func f() *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: netroute.ExtraCAs().RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a fresh config overwritten with a pool read into a local", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tpool := tr.TLSClientConfig.RootCAs\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: pool}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh server config overwritten with a handed-in client-CA pool", poolSrc("func f(pool *x509.CertPool) *tls.Config {\n\tcfg := new(tls.Config)\n\t*cfg = tls.Config{ClientCAs: pool}\n\treturn cfg\n}"), "none"},
		{"a fresh server config overwritten with its server's own pool", poolSrc("type server struct {\n\tclientCAs *x509.CertPool\n\tcerts     []tls.Certificate\n}\n\nfunc (s *server) config() *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: s.certs, ClientCAs: s.clientCAs, ClientAuth: tls.RequireAndVerifyClientCert}\n\treturn cfg\n}"), "none"},
		{"a fresh server config overwritten with a system pool", poolSrc("func serve(certs []tls.Certificate) (*http.Server, error) {\n\tpool, err := x509.SystemCertPool()\n\tif err != nil {\n\t\treturn nil, err\n\t}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: pool}\n\treturn &http.Server{TLSConfig: cfg}, nil\n}"), "none"},
		{"a fresh config overwritten with a pool a helper takes from the transport", poolSrc("func poolOf(tr *http.Transport) *x509.CertPool { return nil }\n\nfunc f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: poolOf(tr)}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with a pool local copied from the transport's", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tp1 := tr.TLSClientConfig.RootCAs\n\tp2 := p1\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: p2}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with a pool local whose address is taken", poolSrc("func f(set func(**x509.CertPool)) *tls.Config {\n\tpool := x509.NewCertPool()\n\tset(&pool)\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: pool}\n\treturn cfg\n}"), "refused"},
		{"a fresh config overwritten with a config value staged in a local", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tbase := tls.Config{}\n\tbase.RootCAs = tr.TLSClientConfig.RootCAs\n\tcfg := &tls.Config{}\n\t*cfg = base\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with a pool a closure reads from the transport", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tget := func() *x509.CertPool { return tr.TLSClientConfig.RootCAs }\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: get()}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with a pool kept in a map local", poolSrc("func f(k string) (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tm := map[string]*x509.CertPool{}\n\tm[k] = tr.TLSClientConfig.RootCAs\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: m[k]}\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten with a handed-in transport's pool", poolSrc("func f(tr *http.Transport) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a fresh config overwritten with the pool of another fresh config", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tbase := &tls.Config{ClientCAs: x509.NewCertPool()}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: base.ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh config overwritten with a copy of a handed-in config", poolSrc("func f(base *tls.Config) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = *base\n\treturn cfg\n}"), "refused"},
		{"a fresh config overwritten through a conversion of the transport's", poolSrc("type serverTLS tls.Config\n\nfunc f() (*http.Transport, *serverTLS) {\n\ttr := netroute.NewTransport()\n\tcfg := &serverTLS{}\n\t*cfg = serverTLS(*tr.TLSClientConfig)\n\treturn tr, cfg\n}"), "refused"},
		{"a fresh config overwritten from a tuple", poolSrc("func f(load func() (tls.Config, error)) *tls.Config {\n\tcfg := &tls.Config{}\n\tvar err error\n\t*cfg, err = load()\n\t_ = err\n\treturn cfg\n}"), "refused"},
		{"a fresh server config overwritten with its own new pool", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tpool := x509.NewCertPool()\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config overwritten with a pool built inline", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: x509.NewCertPool()}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config overwritten with a nil pool", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: nil, Certificates: certs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config overwritten through a conversion", poolSrc("type serverTLS tls.Config\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\tcfg := &serverTLS{}\n\t*cfg = serverTLS(tls.Config{Certificates: certs})\n\treturn &http.Server{TLSConfig: (*tls.Config)(cfg)}\n}"), "none"},
		{"the transport's pool overwritten in place", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\t*tr.TLSClientConfig.RootCAs = x509.CertPool{}\n\treturn tr\n}"), "refused"},
		{"the transport's pool overwritten through a local", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tpool := tr.TLSClientConfig.RootCAs\n\t*pool = x509.CertPool{}\n\treturn tr\n}"), "refused"},
		{"a fresh literal sharing the transport's pool, then the pool cleared", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{RootCAs: tr.TLSClientConfig.RootCAs}\n\t*cfg.RootCAs = x509.CertPool{}\n\treturn tr\n}"), "refused"},
		{"the session CA's pool overwritten before a transport takes it", poolSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\t*cfg.RootCAs = x509.CertPool{}\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}\n}"), "refused"},
		{"a handed-in pool overwritten", poolSrc("func clear(p *x509.CertPool) { *p = x509.CertPool{} }"), "refused"},
		{"a fresh pool overwritten with the transport's", poolSrc("func f() (*http.Transport, *x509.CertPool) {\n\ttr := netroute.NewTransport()\n\tpool := x509.NewCertPool()\n\t*pool = *tr.TLSClientConfig.RootCAs\n\treturn tr, pool\n}"), "refused"},
		{"a fresh pool overwritten with an empty one", poolSrc("func f() *x509.CertPool {\n\tpool := x509.NewCertPool()\n\t*pool = x509.CertPool{}\n\treturn pool\n}"), "none"},

		// Every other way to write: an element, an increment, a range
		// assignment, and a field's address taken (whatever is stored through
		// it later is a value the guard cannot see).
		{"an element of the transport's TLS config written", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.NextProtos[0] = \"h2\"\n\treturn tr\n}"), "refused"},
		{"a TLS config field incremented through the transport", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\ttr.TLSClientConfig.MinVersion++\n\treturn tr\n}"), "refused"},
		{"a range loop assigns the transport's TLS config", poolSrc("func f(cfgs []*tls.Config) *http.Transport {\n\ttr := netroute.NewTransport()\n\tfor _, tr.TLSClientConfig = range cfgs {\n\t}\n\treturn tr\n}"), "refused"},
		{"the address of a transport's TLSClientConfig", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tpp := &tr.TLSClientConfig\n\t*pp = nil\n\treturn tr\n}"), "refused"},
		{"the address of a direct transport's Proxy", poolSrc("func f() *http.Transport {\n\ttr := &http.Transport{}\n\tpp := &tr.Proxy\n\t*pp = http.ProxyFromEnvironment\n\treturn tr\n}"), "refused"},
		{"the address of a client's Transport", poolSrc("func f(rt http.RoundTripper) *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tpt := &c.Transport\n\t*pt = rt\n\treturn c\n}"), "refused"},
		{"the address of the session CA's pool", poolSrc("func f() *http.Transport {\n\tcfg := netroute.ExtraCAs()\n\tpp := &cfg.RootCAs\n\t*pp = nil\n\treturn &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}\n}"), "refused"},
		{"a field address on a fresh server config", poolSrc("func serve(load func(*[]tls.Certificate)) *http.Server {\n\tcfg := &tls.Config{}\n\tload(&cfg.Certificates)\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a field address on an options type", guardSrc("type opts struct{ Transport string }\n\nfunc f(o *opts, set func(*string)) { set(&o.Transport) }"), "none"},
		{"a builder client's timeout through its address", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tp := &c.Timeout\n\t*p = 2 * time.Second\n\treturn c\n}"), "none"},
		{"a builder client's timeout incremented", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Timeout++\n\treturn c\n}"), "none"},
		{"a range loop over plain locals", guardSrc("func f(xs []int) int {\n\tn := 0\n\tfor _, n = range xs {\n\t}\n\treturn n\n}"), "none"},

		// A dereference, a field and an element are judged by the local they
		// start from: (*base).ClientCAs is base.ClientCAs, for a server's own
		// config and for a transport's alike.
		{"a fresh server config's pool read through an explicit dereference", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tbase := &tls.Config{ClientCAs: x509.NewCertPool()}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: (*base).ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config copied whole through a dereference", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tbase := &tls.Config{ClientCAs: x509.NewCertPool()}\n\tcfg := &tls.Config{}\n\t*cfg = *base\n\tcfg.Certificates = certs\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config's pool read through a local holder", poolSrc("type holder struct{ cfg *tls.Config }\n\nfunc serve(certs []tls.Certificate) *http.Server {\n\th := holder{cfg: &tls.Config{ClientCAs: x509.NewCertPool()}}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: (*h.cfg).ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh server config's pool read through a local slice", poolSrc("func serve(certs []tls.Certificate) *http.Server {\n\tbases := []*tls.Config{{ClientCAs: x509.NewCertPool()}}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{Certificates: certs, ClientCAs: bases[0].ClientCAs}\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a server receiver's own pool beside its own TLS config", poolSrc("type server struct {\n\ttlsCfg    *tls.Config\n\tclientCAs *x509.CertPool\n}\n\nfunc (s *server) config() *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{ClientCAs: s.clientCAs}\n\treturn cfg\n}"), "none"},
		{"the transport's pool read through an explicit dereference", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: (*tr.TLSClientConfig).RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a local read from the transport, dereferenced", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tbase := tr.TLSClientConfig\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: (*base).RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a handed-in config's pool", poolSrc("func f(base *tls.Config) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: base.RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a handed-in config's pool through a dereference", poolSrc("func f(base *tls.Config) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: (*base).RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a handed-in config value copied whole", poolSrc("func f(base tls.Config) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = base\n\treturn cfg\n}"), "refused"},
		{"a handed-in holder's config pool", poolSrc("type holder struct{ cfg *tls.Config }\n\nfunc f(h holder) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: h.cfg.RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a local holder whose config is later the transport's", poolSrc("type holder struct{ cfg *tls.Config }\n\nfunc f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\th := holder{cfg: &tls.Config{}}\n\th.cfg = tr.TLSClientConfig\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: h.cfg.RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a local slice holding the transport's config", poolSrc("func f() (*http.Transport, *tls.Config) {\n\ttr := netroute.NewTransport()\n\tcs := []*tls.Config{tr.TLSClientConfig}\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: cs[0].RootCAs}\n\treturn tr, cfg\n}"), "refused"},
		{"a holder a helper returns", poolSrc("type holder struct{ cfg *tls.Config }\n\nfunc load() holder { return holder{} }\n\nfunc f() *tls.Config {\n\th := load()\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: h.cfg.RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a config asserted out of an interface", poolSrc("func f(x interface{}) *tls.Config {\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: x.(*tls.Config).RootCAs}\n\treturn cfg\n}"), "refused"},
		{"a range over handed-in configs", poolSrc("func f(bases []*tls.Config) *tls.Config {\n\tcfg := &tls.Config{}\n\tfor _, b := range bases {\n\t\t*cfg = tls.Config{RootCAs: b.RootCAs}\n\t}\n\treturn cfg\n}"), "refused"},
		{"the address of a transport's pool", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tpp := &tr.TLSClientConfig.RootCAs\n\t*pp = nil\n\treturn tr\n}"), "refused"},
		{"a pool of a type defined from CertPool overwritten", poolSrc("type myPool x509.CertPool\n\nfunc f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tp := (*myPool)(tr.TLSClientConfig.RootCAs)\n\t*p = myPool{}\n\treturn tr\n}"), "refused"},
		{"a fresh pool rebound to the transport's, then overwritten", poolSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tpool := x509.NewCertPool()\n\tpool = tr.TLSClientConfig.RootCAs\n\t*pool = x509.CertPool{}\n\treturn tr\n}"), "refused"},
		{"an mTLS server's own pool", poolSrc("func serve(caPEM []byte, certs []tls.Certificate) *http.Server {\n\tpool := x509.NewCertPool()\n\tpool.AppendCertsFromPEM(caPEM)\n\tcfg := &tls.Config{Certificates: certs, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}\n\tcfg.MinVersion = tls.VersionTLS12\n\treturn &http.Server{TLSConfig: cfg}\n}"), "none"},
		{"a fresh pool copied from a handed-in pool", poolSrc("func f(other *x509.CertPool) *x509.CertPool {\n\tpool := x509.NewCertPool()\n\t*pool = *other\n\treturn pool\n}"), "none"},
		{"a fresh config's field incremented", poolSrc("func f() *tls.Config {\n\tcfg := &tls.Config{}\n\tcfg.MinVersion++\n\treturn cfg\n}"), "none"},
		{"a range loop into a fresh config's field", poolSrc("func f(names []string) *tls.Config {\n\tcfg := &tls.Config{}\n\tfor _, cfg.ServerName = range names {\n\t}\n\treturn cfg\n}"), "none"},
		{"a request's own fields, through their address too", guardSrc("func f(r *http.Request) {\n\tr.Header = http.Header{}\n\tp := &r.Host\n\t*p = \"x\"\n\tr.ContentLength++\n}"), "none"},
		{"pool parameters bound in a cycle", poolSrc("func f(a, b *x509.CertPool) *tls.Config {\n\ta = b\n\tb = a\n\tcfg := &tls.Config{}\n\t*cfg = tls.Config{RootCAs: a}\n\treturn cfg\n}"), "none"},
		{"a server's TLSConfig written through the server", tlsSrc("func serve(certs []tls.Certificate) *http.Server {\n\tsrv := &http.Server{TLSConfig: &tls.Config{}}\n\tsrv.TLSConfig.Certificates = certs\n\treturn srv\n}"), "refused"},
		{"the CA config read back and edited", guardSrc("func f() *http.Transport {\n\ttr := netroute.NewTransport()\n\tcfg := tr.TLSClientConfig\n\tcfg.InsecureSkipVerify = true\n\treturn tr\n}"), "refused"},
		{"a parameter named nil is no direct route", fileSrc("import (\n\t\"net/http\"\n\t\"net/url\"\n)\n\nfunc f(nil func(*http.Request) (*url.URL, error)) *http.Transport { return &http.Transport{Proxy: nil} }\n"), "refused"},
		{"a local named like the builder inside netroute", "package netroute\n\nimport \"net/http\"\n\nfunc f() *http.Client {\n\tNewTransport := func() *http.Transport { return &http.Transport{} }\n\treturn &http.Client{Transport: NewTransport()}\n}\n", "refused"},
		{"a field Go cannot resolve is judged as net/http's", fileSrc("import \"example.com/opts\"\n\nfunc f(o *opts.Options) { o.Proxy = \"inherit\" }\n"), "refused"},
		{"a closure dials its parent's builder-bound local", guardSrc("func f() func() *http.Client {\n\ttr := netroute.NewTransport()\n\treturn func() *http.Client { return &http.Client{Transport: tr} }\n}"), "allowed"},
		{"a builder client's own timeout may change", guardSrc("func f() *http.Client {\n\tc := netroute.NewClient(time.Second)\n\tc.Timeout = 2 * time.Second\n\treturn c\n}"), "none"},
		{"a same-package type's own Transport field", guardSrc("type opts struct{ Transport string }\n\nfunc f(o *opts) { o.Transport = \"stdio\" }"), "none"},
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
// repro:
// an options type may have its own Transport, Proxy or TLSClientConfig fields;
// only a receiver this file shows to be an http client or transport is
// rewiring, so none of these assignments is refused.
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
// repro: a fresh config overwritten whole with the transport's RootCAs shares
// that pool, so the store through it that follows clears the roots the
// transport trusts, NODE_EXTRA_CA_CERTS's included. Both writes are refused.
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
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "route.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var refused []string
	for _, x := range inspectFile(fset, f) {
		if !x.allowed {
			refused = append(refused, x.pos.String())
		}
	}
	if len(refused) != 2 || refused[0] != "route.go:7:3" || refused[1] != "route.go:8:3" {
		t.Fatalf("refused %v, want the shared-pool overwrite at route.go:7:3 and the pool store at route.go:8:3", refused)
	}
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

// TestTheGuardLoadsNetHTTPsTypes: the rewire rules read which type owns a field
// from net/http's and crypto/tls's own types, and which value is a CertPool from
// crypto/x509's. If the toolchain cannot supply them, the guard would fall back
// to judging fields by name alone; fail here instead, so a weaker guard is
// never silently green. The pool a tls.Config's RootCAs points to must be the
// CertPool crypto/x509 loads, or no pool store would ever match.
func TestTheGuardLoadsNetHTTPsTypes(t *testing.T) {
	for _, c := range []struct{ path, name string }{{"net/http", "Client"}, {"net/http", "Transport"}, {"crypto/tls", "Config"}, {"crypto/x509", "CertPool"}} {
		pkg, err := sharedImporter.Import(c.path)
		if err != nil {
			t.Fatalf("the guard cannot load %s: %v", c.path, err)
		}
		if pkg.Scope().Lookup(c.name) == nil {
			t.Fatalf("%s has no %s: the guard's types are not the toolchain's", c.path, c.name)
		}
	}
	tlsPkg, _ := sharedImporter.Import("crypto/tls")
	config := tlsPkg.Scope().Lookup("Config").Type().Underlying().(*types.Struct)
	for i := 0; i < config.NumFields(); i++ {
		if f := config.Field(i); f.Name() == "RootCAs" {
			if !poolState(f.Type()) || !isCertPool(elem(f.Type())) {
				t.Fatalf("tls.Config.RootCAs is %v, which the guard does not see as crypto/x509's CertPool", f.Type())
			}
			return
		}
	}
	t.Fatal("tls.Config has no RootCAs field")
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

// TestASecondPackageNamedNetrouteIsRefused: the guard trusts a bare
// NewTransport or ExtraCAs inside package netroute, so a second package with
// that name would borrow the proof. Only the module's netroute/ may use it.
func TestASecondPackageNamedNetrouteIsRefused(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{"netroute/a.go", "x/netroute/b.go"} {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("package netroute\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fset := token.NewFileSet()
	files, err := productionFiles(root, fset)
	if err != nil {
		t.Fatal(err)
	}
	got := strayNetroute(root, fset, files)
	want := filepath.Join(root, "x", "netroute", "b.go")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("stray packages named netroute = %v, want [%s]", got, want)
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

// finding is one client construction and whether a proof allowed it.
type finding struct {
	pos     token.Position
	allowed bool
	why     string
}

const (
	clientMessage           = "builds an *http.Client outside a CA-aware constructor: inside a Remote Control session this client honours the proxy the edge exports but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its TLS handshake fails before any request leaves; build it with netroute.NewClient or netroute.NewTransport, or prove a direct route with an explicit &http.Transport{Proxy: nil} (#822, #829)"
	transportMessage        = "builds an http.Transport that honours a proxy but not the CA a Remote Control session signs every host with (NODE_EXTRA_CA_CERTS), so its TLS handshake fails inside the edge; set TLSClientConfig: netroute.ExtraCAs() beside the Proxy, take netroute.NewTransport, or leave Proxy nil for a direct route (#829)"
	typeUseMessage          = "names http.Client or http.Transport outside a literal or a pointer type (new, a zero value, a value or embedded field, a type alias or definition, a type argument, a slice or map literal): this guard cannot see the transport such a client dials with, and a zero http.Client honours the session's proxy but not its CA (NODE_EXTRA_CA_CERTS); hold a *http.Client from netroute.NewClient or a transport from netroute.NewTransport (#829)"
	defaultClientMessage    = "uses http.DefaultClient (directly or through http.Get, Head, Post or PostForm): it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so the call fails inside a Remote Control session; call Do on a client from netroute.NewClient (#829)"
	dotImportMessage        = "dot-imports net/http or netroute, which hides from this guard the clients the file builds and the builder it calls; a client that honours the session's proxy but not NODE_EXTRA_CA_CERTS fails its handshake inside a Remote Control session; import the package by name and build clients with netroute.NewClient (#829)"
	mutationMessage         = "rewires a client or transport after construction (its Transport, Proxy or TLSClientConfig, a field or element of its TLS config, or the whole value; assigned, incremented, range-assigned or reached through an address taken) with a value this guard cannot prove trusts the session's CA (NODE_EXTRA_CA_CERTS); build the finished client with netroute.NewClient or netroute.NewTransport, or set Proxy: nil in the literal (#829)"
	defaultTransportMessage = "uses http.DefaultTransport with no TLSClientConfig: it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its handshake fails inside a Remote Control session; clone it through netroute.NewTransport instead (#822, #829)"
	tlsWriteMessage         = "writes into a crypto/tls Config this function did not build: it may be a transport's TLSClientConfig or the session's CA config (NODE_EXTRA_CA_CERTS, as netroute.ExtraCAs returns it) read back or handed in, so the write can undo the CA a client trusts; build the config here as a literal or new(tls.Config), however the type is spelled, or hold it as a tls.Config value, and set its fields there; never edit a transport's TLSClientConfig (#829)"
	sharedStateMessage      = "overwrites a crypto/tls Config it built with a value that carries another config's CA state (a TLS config or a CertPool not built in that value or held in a local bound only to x509.NewCertPool): the copy shares its RootCAs pool with a transport that may trust the session's CA (NODE_EXTRA_CA_CERTS, as netroute.ExtraCAs returns it), so a store through the copy undoes that trust; build the value in the assignment as a literal, with pools from x509.NewCertPool (#829)"
	poolWriteMessage        = "overwrites a crypto/x509 CertPool in place through a pointer this function did not get from x509.NewCertPool, or with a pool it did not build: the pool may be the RootCAs a transport trusts, the session's CA (NODE_EXTRA_CA_CERTS, as netroute.ExtraCAs returns it), and replacing it undoes that trust; build a new pool with x509.NewCertPool and add to it, never overwrite one in place (#829)"
)

const (
	// guardHead is the import set every row of the bypass table may use.
	guardHead = "package p\n\nimport (\n\t\"net/http\"\n\t\"time\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\n"
	// netroutePath is the one package whose bare NewTransport/ExtraCAs count.
	netroutePath = "github.com/akleizor/ccmorph/netroute"
)

// guardSrc is a row body under the shared import set.
func guardSrc(body string) string { return guardHead + body + "\n" }

// tlsSrc is a row body under the shared import set plus crypto/tls.
func tlsSrc(body string) string {
	return "package p\n\nimport (\n\t\"crypto/tls\"\n\t\"net/http\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\n" + body + "\n"
}

// poolSrc is a row body under the shared import set plus crypto/tls and
// crypto/x509; the blank declarations keep each import used.
func poolSrc(body string) string {
	return "package p\n\nimport (\n\t\"crypto/tls\"\n\t\"crypto/x509\"\n\t\"net/http\"\n\t\"time\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\nvar (\n\t_ *tls.Config\n\t_ *x509.CertPool\n\t_ *http.Client\n\t_ = time.Second\n\t_ = netroute.ExtraCAs\n)\n\n" + body + "\n"
}

// fileSrc is a whole file whose text follows the package clause, including its
// own imports.
func fileSrc(rest string) string { return "package p\n\n" + rest }

// guardImporter loads net/http, crypto/tls and crypto/x509 from the toolchain,
// so Go itself says which type a literal builds, which type owns the field an
// assignment writes and which value is a CertPool, and gives this module's
// netroute its three builders' result types (netrouteModel), so a transport or
// config taken from one is typed too. Every other import fails, and go/types
// then gives it an empty package and carries on: its name still resolves to
// its path, its members resolve to nothing. One instance serves the whole test
// binary, so each package is read once.
type guardImporter struct {
	std      types.Importer
	netroute *types.Package
}

func (g *guardImporter) Import(path string) (*types.Package, error) {
	if path == netroutePath {
		return g.netrouteModel()
	}
	if path != "net/http" && path != "crypto/tls" && path != "crypto/x509" {
		return nil, errors.New("the client guard loads only net/http, crypto/tls, crypto/x509 and netroute's builders")
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

var sharedImporter = &guardImporter{}

// resolved is one file under Go's own name resolution. info says which object
// each identifier means, so whether web.Client is net/http's, whether nr is the
// netroute import or a parameter that hides it, and which tr a client dials
// with are answered by go/types, not by this guard matching spellings (#829).
type resolved struct {
	file       *ast.File
	info       *types.Info
	inNetroute bool
}

// inspectFile reports every client construction, transport construction and
// rewire in f, refused unless its own transport proves it CA-aware or direct.
// Containment in a function that happens to call a builder proves nothing: the
// proof is the transport the client will actually dial with (#829).
func inspectFile(fset *token.FileSet, f *ast.File) []finding {
	return inspectPackage(fset, []*ast.File{f})
}

// inspectPackage type-checks the files of one package together and inspects
// each: a receiver whose type another file of the package declares still
// resolves. Check's errors are expected (every import but net/http, crypto/tls
// and crypto/x509 is unresolved) and ignored; what resolves is Go's answer.
func inspectPackage(fset *token.FileSet, files []*ast.File) []finding {
	if len(files) == 0 {
		return nil
	}
	info := &types.Info{
		Types:      map[ast.Expr]types.TypeAndValue{},
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: sharedImporter, Error: func(error) {}}
	_, _ = conf.Check(files[0].Name.Name, fset, files, info)
	var out []finding
	for _, f := range files {
		r := &resolved{file: f, info: info, inNetroute: f.Name.Name == "netroute"}
		out = append(out, r.inspect(fset)...)
	}
	return out
}

func (r *resolved) inspect(fset *token.FileSet) []finding {
	var out []finding
	add := func(node ast.Node, allowed bool, msg string) {
		out = append(out, finding{pos: fset.Position(node.Pos()), allowed: allowed, why: msg})
	}
	for _, spec := range dotImports(r.file) {
		add(spec, false, dotImportMessage)
	}
	// framed are the selectors that are types, not names used as values: the
	// element of a *http.Client or the type of an &http.Client literal.
	framed := map[*ast.SelectorExpr]bool{}
	ast.Inspect(r.file, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.StarExpr:
			if sel, ok := x.X.(*ast.SelectorExpr); ok {
				framed[sel] = true
			}
		case *ast.CompositeLit:
			if sel, ok := x.Type.(*ast.SelectorExpr); ok {
				framed[sel] = true
			}
		}
		return true
	})
	ast.Inspect(r.file, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.CompositeLit:
			switch {
			case r.httpSel(x.Type, "Client"):
				add(x, r.clientTransportAllowed(x), clientMessage)
			case r.httpSel(x.Type, "Transport"):
				add(x, r.tlsClientConfigCA(x) || r.directTransport(x), transportMessage)
			case x.Type != nil && r.mentionsHTTPType(x.Type):
				add(x, false, typeUseMessage)
			}
		case *ast.SelectorExpr:
			switch {
			case r.httpSel(x, "DefaultTransport"):
				add(x, r.cloneGetsTheCA(x), defaultTransportMessage)
			case (r.httpSel(x, "Client") || r.httpSel(x, "Transport")) && !framed[x]:
				add(x, false, typeUseMessage)
			case r.httpSel(x, "DefaultClient") || r.httpSel(x, "Get") || r.httpSel(x, "Head") || r.httpSel(x, "Post") || r.httpSel(x, "PostForm"):
				add(x, false, defaultClientMessage)
			}
		case *ast.AssignStmt:
			r.rewires(x, add)
		case *ast.IncDecStmt:
			r.judgeWrite(x.X, nil, add)
		case *ast.RangeStmt:
			if x.Tok == token.ASSIGN {
				if x.Key != nil {
					r.judgeWrite(x.Key, nil, add)
				}
				if x.Value != nil {
					r.judgeWrite(x.Value, nil, add)
				}
			}
		case *ast.UnaryExpr:
			// A field's address is a write the guard cannot follow: whatever is
			// stored through it later is judged here, as an unknown value.
			if x.Op == token.AND {
				switch unparen(x.X).(type) {
				case *ast.SelectorExpr, *ast.IndexExpr:
					r.judgeWrite(x.X, nil, add)
				}
			}
		}
		return true
	})
	return out
}

// rewires judges each assignment by judgeWrite, pairing each target with its
// value when the counts match.
func (r *resolved) rewires(as *ast.AssignStmt, add func(ast.Node, bool, string)) {
	for i, lhs := range as.Lhs {
		var rhs ast.Expr
		if len(as.Lhs) == len(as.Rhs) {
			rhs = as.Rhs[i]
		}
		r.judgeWrite(lhs, rhs, add)
	}
}

// judgeWrite judges one write that may rewire a client or transport after
// construction: a client's Transport, a transport's Proxy or TLSClientConfig,
// anything written through a TLSClientConfig or into a crypto/tls Config this
// function did not build (a field, an element or the whole value), a CertPool
// overwritten in place, and a whole client or transport overwritten through a
// pointer, each by the type Go resolves. The write is an assignment, an
// increment, a range assignment or a field's address taken; rhs is nil when no
// value can be paired with it, and then nothing proves the value. Go's own
// selection says which type owns the field, so an options struct's Transport
// string is no client. A field Go cannot resolve (its receiver's type comes from
// an import this guard does not load) is judged as net/http's: nothing proves
// it is not (#829).
func (r *resolved) judgeWrite(lhs, rhs ast.Expr, add func(ast.Node, bool, string)) {
	switch x := unparen(lhs).(type) {
	case *ast.StarExpr:
		r.judgeStore(x, rhs, add)
	case *ast.IndexExpr:
		r.judgeElement(x, add)
	case *ast.SelectorExpr:
		r.judgeField(x, rhs, add)
	}
}

// judgeStore judges a store through a pointer (*p = v), which replaces the
// whole value p points to: a transport may share that value.
func (r *resolved) judgeStore(star *ast.StarExpr, rhs ast.Expr, add func(ast.Node, bool, string)) {
	t := r.info.TypeOf(star)
	switch {
	case r.throughTLS(star.X):
		add(star, false, mutationMessage)
	case isCertPool(t):
		if !r.freshPool(star.X) || rhs == nil || r.carriesCAState(rhs) {
			add(star, false, poolWriteMessage)
		}
	case isTLSConfig(t):
		if !r.freshTLSConfig(star.X) {
			add(star, false, tlsWriteMessage)
		} else if rhs == nil || r.carriesCAState(rhs) {
			add(star, false, sharedStateMessage)
		}
	case r.holdsHTTPOrTLS(star):
		add(star, false, mutationMessage)
	}
}

// judgeElement judges a write into an element (s[i] = v) of a slice or map a
// TLS config holds: through a transport's TLSClientConfig, or in a config this
// function did not build, it edits a config a transport may dial with.
func (r *resolved) judgeElement(ix *ast.IndexExpr, add func(ast.Node, bool, string)) {
	if r.throughTLS(ix.X) {
		add(ix, false, mutationMessage)
		return
	}
	if sel, ok := unparen(ix.X).(*ast.SelectorExpr); ok && r.fieldOwner(sel) == "crypto/tls" && !r.freshTLSConfig(sel.X) {
		add(ix, false, tlsWriteMessage)
	}
}

// judgeField judges a write into a field (x.F = v).
func (r *resolved) judgeField(sel *ast.SelectorExpr, rhs ast.Expr, add func(ast.Node, bool, string)) {
	if r.isPackageName(sel.X) {
		return // a package's own variable; DefaultTransport and DefaultClient are refused where named
	}
	owner := r.fieldOwner(sel)
	if r.throughTLS(sel.X) {
		add(sel, false, mutationMessage)
		return
	}
	if owner == "crypto/tls" {
		if !r.freshTLSConfig(sel.X) {
			add(sel, false, tlsWriteMessage)
		}
		return // a config this function built itself (a server's): no outbound proof rests on it
	}
	if owner != "" && owner != "net/http" {
		return // another type's own field
	}
	switch sel.Sel.Name {
	case "Transport":
		add(sel, rhs != nil && r.transportAllowed(rhs), mutationMessage)
	case "Proxy":
		add(sel, rhs != nil && r.isNil(rhs), mutationMessage)
	case "TLSClientConfig":
		add(sel, rhs != nil && r.caConfig(rhs), mutationMessage)
	}
}

// pkgSel is whether expr is <a name Go resolves to the package at path>.<sel>.
// An alias is the same package; a local of the same name is not one at all.
func (r *resolved) pkgSel(expr ast.Expr, path, sel string) bool {
	s, ok := expr.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	if !ok {
		return false
	}
	pn, ok := r.info.Uses[id].(*types.PkgName)
	return ok && pn.Imported().Path() == path
}

func (r *resolved) httpSel(expr ast.Expr, sel string) bool {
	return r.pkgSel(expr, "net/http", sel)
}

// isPackageName is whether expr is an identifier Go resolves to an import.
func (r *resolved) isPackageName(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	_, isPkg := r.info.Uses[id].(*types.PkgName)
	return isPkg
}

// netrouteCall is a call to netroute's name: qualified by the module's netroute
// import, or bare inside package netroute when the name is its package-level
// function (unresolved means a sibling file declares it, in a lone-file check).
func (r *resolved) netrouteCall(expr ast.Expr, name string) bool {
	call, ok := unparen(expr).(*ast.CallExpr)
	if !ok {
		return false
	}
	switch fn := unparen(call.Fun).(type) {
	case *ast.Ident:
		if !r.inNetroute || fn.Name != name {
			return false
		}
		obj := r.info.Uses[fn]
		if obj == nil {
			return true
		}
		_, isFunc := obj.(*types.Func)
		return isFunc && obj.Pkg() != nil && obj.Parent() == obj.Pkg().Scope()
	case *ast.SelectorExpr:
		return r.pkgSel(fn, netroutePath, name)
	}
	return false
}

// isClone is http.DefaultTransport.(*http.Transport).Clone().
func (r *resolved) isClone(expr ast.Expr) bool {
	call, ok := unparen(expr).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Clone" {
		return false
	}
	ta, ok := sel.X.(*ast.TypeAssertExpr)
	if !ok {
		return false
	}
	typ := ta.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	return r.httpSel(typ, "Transport") && r.httpSel(ta.X, "DefaultTransport")
}

func (r *resolved) isNewTransport(e ast.Expr) bool { return r.netrouteCall(e, "NewTransport") }

func (r *resolved) isExtraCAs(e ast.Expr) bool { return r.netrouteCall(e, "ExtraCAs") }

// isNil is the predeclared nil, not a parameter or local that borrows its name.
func (r *resolved) isNil(expr ast.Expr) bool {
	id, ok := unparen(expr).(*ast.Ident)
	if !ok {
		return false
	}
	_, isNil := r.info.Uses[id].(*types.Nil)
	return isNil
}

// objOf is the object Go binds id to, declared or used.
func (r *resolved) objOf(id *ast.Ident) types.Object {
	if o := r.info.Defs[id]; o != nil {
		return o
	}
	return r.info.Uses[id]
}

// localVar is the function-local variable expr names, or nil: a package-level
// variable can be set from any file of its package, so it carries no proof.
func (r *resolved) localVar(expr ast.Expr) *types.Var {
	id, ok := unparen(expr).(*ast.Ident)
	if !ok {
		return nil
	}
	v, ok := r.objOf(id).(*types.Var)
	if !ok || v.IsField() || v.Pkg() == nil || v.Parent() == nil || v.Parent() == v.Pkg().Scope() {
		return nil
	}
	return v
}

// names is whether expr is an identifier Go binds to v.
func (r *resolved) names(expr ast.Expr, v *types.Var) bool {
	id, ok := unparen(expr).(*ast.Ident)
	return ok && r.objOf(id) == v
}

// onlyBoundBy is whether v is bound at least once and every binding passes ok.
// The bindings are v's own, by object: a same-named local in another block is
// another variable. A parameter, range variable, valueless declaration, a
// tuple assignment, its address taken, or a store through it (*v = ...) is a
// binding that passes nothing, so a proof is lost as soon as v may hold
// anything else.
func (r *resolved) onlyBoundBy(v *types.Var, ok func(ast.Expr) bool) bool {
	return r.onlyBound(v, ok, true)
}

// onlyBound is onlyBoundBy with the choice of whether a store through v
// (*v = ...) counts as a binding. It does for a proof (a transport or the CA
// config overwritten is no longer what the builder made); it does not for
// freshness, since the store replaces the contents of the config v points
// to, which is still the one this function built.
func (r *resolved) onlyBound(v *types.Var, ok func(ast.Expr) bool, storesRebind bool) bool {
	if v == nil {
		return false
	}
	good, other := 0, 0
	ast.Inspect(r.file, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if star, isStar := unparen(lhs).(*ast.StarExpr); isStar && r.names(star.X, v) {
					if storesRebind {
						other++
					}
					continue
				}
				if !r.names(lhs, v) {
					continue
				}
				if len(x.Lhs) != len(x.Rhs) || !ok(x.Rhs[i]) {
					other++
					continue
				}
				good++
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if r.objOf(id) != v {
					continue
				}
				if len(x.Values) != len(x.Names) || !ok(x.Values[i]) {
					other++
					continue
				}
				good++
			}
		case *ast.Field:
			for _, id := range x.Names {
				if r.objOf(id) == v {
					other++
				}
			}
		case *ast.RangeStmt:
			if (x.Key != nil && r.names(x.Key, v)) || (x.Value != nil && r.names(x.Value, v)) {
				other++
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND && r.names(x.X, v) {
				other++
			}
		}
		return true
	})
	return good > 0 && other == 0
}

// tlsWrites calls each with the value of every assignment to v.TLSClientConfig;
// nil when the assignment is a tuple whose value cannot be paired.
func (r *resolved) tlsWrites(v *types.Var, each func(ast.Expr)) {
	ast.Inspect(r.file, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, isSel := unparen(lhs).(*ast.SelectorExpr)
			if !isSel || sel.Sel.Name != "TLSClientConfig" || !r.names(sel.X, v) {
				continue
			}
			if len(as.Lhs) == len(as.Rhs) {
				each(as.Rhs[i])
			} else {
				each(nil)
			}
		}
		return true
	})
}

// setsTLSToCA is whether v.TLSClientConfig is assigned the session's CA.
func (r *resolved) setsTLSToCA(v *types.Var) bool {
	found := false
	r.tlsWrites(v, func(e ast.Expr) {
		if e != nil && r.caConfig(e) {
			found = true
		}
	})
	return found
}

// tlsReset is whether v.TLSClientConfig is assigned anything but the session's
// CA: a later wipe cancels what the builder gave the transport.
func (r *resolved) tlsReset(v *types.Var) bool {
	reset := false
	r.tlsWrites(v, func(e ast.Expr) {
		if e == nil || !r.caConfig(e) {
			reset = true
		}
	})
	return reset
}

// writesInto is whether any assignment stores into something reached from v
// (v.RootCAs = nil, v.X.Y = ..., *v.RootCAs = ..., v.NextProtos[0] = ...): a
// config bound to ExtraCAs and then edited no longer is the session's CA.
func (r *resolved) writesInto(v *types.Var) bool {
	found := false
	ast.Inspect(r.file, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			if _, isIdent := unparen(lhs).(*ast.Ident); !isIdent && r.names(rootOf(lhs), v) {
				found = true
			}
		}
		return true
	})
	return found
}

// rootOf is the expression a selector, index, assertion or dereference chain
// starts from: cfg in cfg.RootCAs, c in c.Transport.(*http.Transport).Proxy.
func rootOf(expr ast.Expr) ast.Expr {
	for {
		switch x := expr.(type) {
		case *ast.SelectorExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.TypeAssertExpr:
			expr = x.X
		case *ast.StarExpr:
			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		default:
			return expr
		}
	}
}

// caConfig proves one TLS config expression: netroute.ExtraCAs, or a local
// bound only to it and never edited.
func (r *resolved) caConfig(v ast.Expr) bool {
	if r.isExtraCAs(v) {
		return true
	}
	local := r.localVar(v)
	return local != nil && r.onlyBoundBy(local, r.isExtraCAs) && !r.writesInto(local)
}

// clientTransportAllowed asks the construction's own Transport field. A client
// earns its place with the transport it names, never with an unrelated call in
// the same function.
func (r *resolved) clientTransportAllowed(client *ast.CompositeLit) bool {
	for _, elt := range client.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Transport" {
			continue
		}
		return r.transportAllowed(kv.Value)
	}
	return false // no Transport: http.DefaultTransport, proxy honoured, CA ignored
}

// transportAllowed proves one transport expression: the CA-aware builder, a
// transport literal that carries the session's CA (or never meets the proxy),
// or a local variable Go binds only to one of those.
func (r *resolved) transportAllowed(expr ast.Expr) bool {
	switch v := unparen(expr).(type) {
	case *ast.CallExpr:
		return r.isNewTransport(v)
	case *ast.Ident:
		return r.boundTransport(r.localVar(v))
	case *ast.UnaryExpr, *ast.CompositeLit:
		lit, ok := compositeOf(v)
		if !ok || !r.httpSel(lit.Type, "Transport") {
			return false
		}
		return r.tlsClientConfigCA(lit) || r.directTransport(lit)
	}
	return false
}

// boundTransport is a local bound only to netroute.NewTransport, or bound only
// to a DefaultTransport clone whose TLSClientConfig is then set from ExtraCAs,
// and whose TLSClientConfig is never set to anything else.
func (r *resolved) boundTransport(v *types.Var) bool {
	if v == nil {
		return false
	}
	built := r.onlyBoundBy(v, r.isNewTransport) || (r.onlyBoundBy(v, r.isClone) && r.setsTLSToCA(v))
	return built && !r.tlsReset(v)
}

// tlsClientConfigCA proves the transport literal itself sets TLSClientConfig
// from the session's CA.
func (r *resolved) tlsClientConfigCA(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "TLSClientConfig" {
			continue
		}
		return r.caConfig(kv.Value)
	}
	return false
}

// directTransport is a transport literal with Proxy: nil (the predeclared nil)
// or no Proxy field at all: it never consults HTTPS_PROXY, so the session's
// proxy can never sign it and no extra CA is needed.
func (r *resolved) directTransport(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return false // positional fields: the guard cannot tell which is Proxy
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Proxy" {
			return r.isNil(kv.Value)
		}
	}
	return true // absent means nil, a direct dial
}

// cloneGetsTheCA: a DefaultTransport use is allowed only when its clone is
// bound to a local Go binds only to clones, whose TLSClientConfig is then set
// from the session's CA and never reset: netroute.NewTransport's own body.
func (r *resolved) cloneGetsTheCA(use ast.Node) bool {
	var v *types.Var
	ast.Inspect(r.file, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) != len(x.Rhs) {
				return true
			}
			for i, rhs := range x.Rhs {
				if containsNode(rhs, use) && r.isClone(rhs) {
					v = r.localVar(x.Lhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(x.Names) != len(x.Values) {
				return true
			}
			for i, val := range x.Values {
				if containsNode(val, use) && r.isClone(val) {
					v = r.localVar(x.Names[i])
				}
			}
		}
		return true
	})
	return v != nil && r.onlyBoundBy(v, r.isClone) && r.setsTLSToCA(v) && !r.tlsReset(v)
}

// fieldOwner is the import path of the package whose type declares the field
// sel writes, as Go resolves it; "" when Go cannot resolve the receiver.
func (r *resolved) fieldOwner(sel *ast.SelectorExpr) string {
	s := r.info.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal || s.Obj().Pkg() == nil {
		return ""
	}
	return s.Obj().Pkg().Path()
}

// throughTLS is whether expr reaches through a TLSClientConfig that is
// net/http's (or that Go cannot resolve), so the assignment edits the config
// the builder set.
func (r *resolved) throughTLS(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if s, ok := node.(*ast.SelectorExpr); ok && s.Sel.Name == "TLSClientConfig" {
			if o := r.fieldOwner(s); o == "" || o == "net/http" {
				found = true
			}
		}
		return true
	})
	return found
}

// freshTLSConfig is whether a write into expr lands in a crypto/tls Config no
// transport's proof can share, by the type Go resolves, never by its spelling:
// a variable whose type is the config itself (a value: the write lands in its
// own storage, and its address never passes caConfig), or a local Go binds
// only to configs built here, a literal or new(...) of crypto/tls's Config
// however it is spelled (tls.Config, an aliased import, a type alias, a type
// defined from it), such as a server's own config. Anything else (ExtraCAs's
// result, a parameter, a field, a transport's TLSClientConfig read back, a
// Server's TLSConfig) stays refused.
func (r *resolved) freshTLSConfig(expr ast.Expr) bool {
	if id, ok := unparen(expr).(*ast.Ident); ok {
		if v, ok := r.objOf(id).(*types.Var); ok && !v.IsField() && isTLSConfig(v.Type()) {
			return true
		}
	}
	v := r.localVar(rootOf(expr))
	return v != nil && r.onlyBound(v, r.builtTLSConfig, false)
}

// builtTLSConfig is whether e makes a new crypto/tls Config: a composite
// literal (or its address) or a call to the builtin new, whose type Go
// resolves to the config's.
func (r *resolved) builtTLSConfig(e ast.Expr) bool {
	if lit, ok := compositeOf(e); ok {
		return isTLSConfig(r.info.TypeOf(lit))
	}
	call, ok := unparen(e).(*ast.CallExpr)
	return ok && r.builtinNew(call, isTLSConfig)
}

// builtPool is whether e makes a new crypto/x509 CertPool: x509.NewCertPool(),
// a CertPool literal (or its address) or the builtin new of one.
func (r *resolved) builtPool(e ast.Expr) bool {
	if lit, ok := compositeOf(e); ok {
		return isCertPool(r.info.TypeOf(lit))
	}
	call, ok := unparen(e).(*ast.CallExpr)
	if !ok {
		return false
	}
	if r.pkgSel(unparen(call.Fun), "crypto/x509", "NewCertPool") {
		return len(call.Args) == 0
	}
	return r.builtinNew(call, isCertPool)
}

// builtinNew is whether call is the builtin new of a type is accepts; a
// parameter or local named new is no allocation.
func (r *resolved) builtinNew(call *ast.CallExpr, is func(types.Type) bool) bool {
	if len(call.Args) != 1 {
		return false
	}
	fn, ok := unparen(call.Fun).(*ast.Ident)
	if !ok {
		return false
	}
	if b, isBuiltin := r.info.Uses[fn].(*types.Builtin); !isBuiltin || b.Name() != "new" {
		return false
	}
	ptr, ok := r.info.TypeOf(call).(*types.Pointer)
	return ok && is(ptr.Elem())
}

// freshPool is whether expr is a local Go binds only to pools built here: a
// store through it replaces no pool a transport trusts.
func (r *resolved) freshPool(expr ast.Expr) bool {
	v := r.localVar(expr)
	return v != nil && r.onlyBound(v, r.builtPool, false)
}

// carriesCAState is whether e carries another config's CA state into the
// config or pool it overwrites: e mentions an http client or transport
// (whatever it yields may be its TLS config), a value that is or holds a
// crypto/tls Config it does not build itself (a literal or new), a config
// handed in as a parameter, or a local that is bound to, written with or
// addressed for such a value anywhere in the file. A dereference, a field and
// an element are traced to the local they start from, so (*base).ClientCAs is
// judged as base.ClientCAs. Such a copy shares that config's pool, a
// transport's TLSClientConfig or the session's CA, and a store through it
// undoes the CA the transport trusts (#829). A pool of no such origin (a
// server's own, a parameter, a field) carries nothing: a store through it is
// refused where it is written.
func (r *resolved) carriesCAState(e ast.Expr) bool {
	return r.carries(e, map[*types.Var]bool{})
}

func (r *resolved) carries(e ast.Expr, seen map[*types.Var]bool) bool {
	found := false
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		if found {
			return false
		}
		switch x := node.(type) {
		case *ast.KeyValueExpr:
			if id, ok := x.Key.(*ast.Ident); ok {
				if v, isVar := r.info.Uses[id].(*types.Var); isVar && v.IsField() {
					ast.Inspect(x.Value, visit) // a struct literal's key names a field, it holds nothing
					return false
				}
			}
			return true
		case *ast.ParenExpr:
			return true
		case ast.Expr:
			t := r.valueType(x)
			if t == nil || r.isNil(x) {
				return true
			}
			if httpState(t) {
				found = true
				return false
			}
			if v := r.localVar(x); v != nil {
				// A handed-in config (or a slice or map of them) is one this
				// function did not build; any other local is traced to what
				// binds it.
				found = (r.isParam(v) && holdsConfig(v.Type())) || r.localCarries(v, seen)
				return false
			}
			if !reachesState(t, map[types.Type]bool{}) {
				return true // a pool or any other value: what it is read from is judged in turn
			}
			if _, ok := compositeOf(x); ok {
				return true // built here: its elements are judged in turn
			}
			if call, ok := x.(*ast.CallExpr); ok {
				if tv, known := r.info.Types[call.Fun]; known && tv.IsType() {
					return true // a conversion carries what its operand carries
				}
			}
			if _, ok := x.(*ast.StarExpr); ok {
				return true // a dereference carries what its operand carries: (*base).X is base.X
			}
			if root := r.projectionRoot(x); root != nil {
				// A field or element of a local (h.cfg, cs[0], (*base).cfg) holds
				// what that local was bound to or written with; one of a
				// parameter was handed in, so nothing proves it is not a
				// transport's.
				found = r.isParam(root) || r.localCarries(root, seen)
				return false
			}
			if r.builtTLSConfig(x) {
				return false
			}
			found = true // another config, or a value that may hold one
			return false
		}
		return true
	}
	ast.Inspect(e, visit)
	return found
}

// localCarries is whether local v holds CA state: anywhere in the file it is
// bound to, or written with (v.F = x, v[i] = x, *v = x), a value that carries
// it, or the address of v or of something in it is taken (then anything may
// be stored there). A parameter is bound by no expression in its function.
func (r *resolved) localCarries(v *types.Var, seen map[*types.Var]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	found := false
	value := func(values []ast.Expr, i, n int) {
		switch {
		case len(values) == n:
			found = found || r.carries(values[i], seen)
		case len(values) == 1:
			found = found || r.carries(values[0], seen)
		}
	}
	ast.Inspect(r.file, func(node ast.Node) bool {
		if found {
			return false
		}
		switch x := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if r.names(rootOf(lhs), v) {
					value(x.Rhs, i, len(x.Lhs))
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if r.objOf(id) == v {
					value(x.Values, i, len(x.Names))
				}
			}
		case *ast.RangeStmt:
			if (x.Key != nil && r.names(rootOf(x.Key), v)) || (x.Value != nil && r.names(rootOf(x.Value), v)) {
				found = r.carries(x.X, seen)
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND && r.names(rootOf(x.X), v) {
				found = true
			}
		}
		return true
	})
	return found
}

// projectionRoot is the function-local variable a chain of field selections,
// indexes, dereferences and parentheses starts from (h in h.cfg, cs in cs[0],
// base in (*base).cfg); nil when the chain starts anywhere else (a call, a
// type assertion, a package's variable, a method value).
func (r *resolved) projectionRoot(e ast.Expr) *types.Var {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.SelectorExpr:
			if s := r.info.Selections[x]; s == nil || s.Kind() != types.FieldVal {
				return nil
			}
			e = x.X
		case *ast.Ident:
			return r.localVar(x)
		default:
			return nil
		}
	}
}

// isParam is whether v is a parameter or receiver of a function in the file:
// its value was handed in, so nothing in the function says what it holds.
func (r *resolved) isParam(v *types.Var) bool {
	found := false
	in := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, f := range fields.List {
			for _, id := range f.Names {
				if r.objOf(id) == v {
					found = true
				}
			}
		}
	}
	ast.Inspect(r.file, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.FuncDecl:
			in(x.Recv)
		case *ast.FuncType:
			in(x.Params)
		}
		return !found
	})
	return found
}

// holdsConfig is whether t is a crypto/tls Config or a pointer, slice, array,
// map or channel of them.
func holdsConfig(t types.Type) bool {
	for i := 0; i < 16 && t != nil; i++ {
		if isTLSConfig(t) {
			return true
		}
		switch u := t.Underlying().(type) {
		case *types.Pointer:
			t = u.Elem()
		case *types.Slice:
			t = u.Elem()
		case *types.Array:
			t = u.Elem()
		case *types.Map:
			t = u.Elem()
		case *types.Chan:
			t = u.Elem()
		default:
			return false
		}
	}
	return false
}

// reachesState is whether a value of type t is, or holds through pointers,
// containers or struct fields, a crypto/tls Config or a net/http Client or
// Transport. A CertPool holds none of them, nor does a function or an
// interface value as far as its type says.
func reachesState(t types.Type, seen map[types.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true
	if isTLSConfig(t) || sameUnderlying(t, "net/http", "Client") || sameUnderlying(t, "net/http", "Transport") {
		return true
	}
	switch u := t.Underlying().(type) {
	case *types.Pointer:
		return reachesState(u.Elem(), seen)
	case *types.Slice:
		return reachesState(u.Elem(), seen)
	case *types.Array:
		return reachesState(u.Elem(), seen)
	case *types.Map:
		return reachesState(u.Key(), seen) || reachesState(u.Elem(), seen)
	case *types.Chan:
		return reachesState(u.Elem(), seen)
	case *types.Struct:
		for i := 0; i < u.NumFields(); i++ {
			if reachesState(u.Field(i).Type(), seen) {
				return true
			}
		}
	}
	return false
}

// valueType is the type Go gives e as a value; nil for a type, a package name
// or a field's own name.
func (r *resolved) valueType(e ast.Expr) types.Type {
	if tv, ok := r.info.Types[e]; ok {
		if tv.IsValue() {
			return tv.Type
		}
		return nil
	}
	if id, ok := e.(*ast.Ident); ok {
		if v, isVar := r.objOf(id).(*types.Var); isVar && !v.IsField() {
			return v.Type()
		}
	}
	return nil
}

// elem is t, or what t points to.
func elem(t types.Type) types.Type {
	if p, ok := t.Underlying().(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

// poolState is whether t is a crypto/x509 CertPool or a pointer to one.
func poolState(t types.Type) bool { return isCertPool(elem(t)) }

// httpState is whether t is net/http's Client or Transport or a pointer to one.
func httpState(t types.Type) bool {
	e := elem(t)
	return sameUnderlying(e, "net/http", "Client") || sameUnderlying(e, "net/http", "Transport")
}

// isCertPool is whether Go resolves t to crypto/x509's CertPool, an alias of it
// or a type defined from it: its fields are all unexported, so no other type
// shares its underlying struct.
func isCertPool(t types.Type) bool { return sameUnderlying(t, "crypto/x509", "CertPool") }

// isTLSConfig is whether Go resolves t to crypto/tls's Config, an alias of it
// or a type defined from it: all share its underlying struct, whose unexported
// fields no other type can declare.
func isTLSConfig(t types.Type) bool { return sameUnderlying(t, "crypto/tls", "Config") }

// sameUnderlying is whether t's underlying type is that of the named type
// path.name, as the guard's importer loads it; false when either is unknown.
func sameUnderlying(t types.Type, path, name string) bool {
	if t == nil {
		return false
	}
	pkg, err := sharedImporter.Import(path)
	if err != nil {
		return false
	}
	obj := pkg.Scope().Lookup(name)
	if obj == nil {
		return false
	}
	return types.Identical(t.Underlying(), obj.Type().Underlying())
}

// holdsHTTPOrTLS is whether Go types expr as net/http's Client or Transport or
// crypto/tls's Config, through an alias or a type defined from one: a store
// through such a pointer replaces the whole value.
func (r *resolved) holdsHTTPOrTLS(expr ast.Expr) bool {
	t := r.info.TypeOf(expr)
	return sameUnderlying(t, "net/http", "Client") || sameUnderlying(t, "net/http", "Transport") || isTLSConfig(t)
}

// mentionsHTTPType is whether a literal's type names a client or transport, as
// a slice or map element or a type argument does: such an element is elided,
// so the client rules never see it.
func (r *resolved) mentionsHTTPType(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(x ast.Node) bool {
		if e, ok := x.(ast.Expr); ok && (r.httpSel(e, "Client") || r.httpSel(e, "Transport")) {
			found = true
		}
		return true
	})
	return found
}

// dotImports are the file's dot imports of net/http or this module's netroute:
// they hide the names the guard reasons about, so each is refused.
func dotImports(f *ast.File) []*ast.ImportSpec {
	var out []*ast.ImportSpec
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || (path != "net/http" && path != netroutePath) {
			continue
		}
		if spec.Name != nil && spec.Name.Name == "." {
			out = append(out, spec)
		}
	}
	return out
}

// containsNode is whether root's span holds n.
func containsNode(root ast.Node, n ast.Node) bool {
	return root.Pos() <= n.Pos() && n.End() <= root.End()
}

// compositeOf unwraps the & of an &http.Transport literal.
func compositeOf(expr ast.Expr) (*ast.CompositeLit, bool) {
	expr = unparen(expr)
	if u, ok := expr.(*ast.UnaryExpr); ok && u.Op == token.AND {
		expr = unparen(u.X)
	}
	lit, ok := expr.(*ast.CompositeLit)
	return lit, ok
}

// unparen strips the parentheses around expr.
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

// strayNetroute is every file under root that declares package netroute outside
// root/netroute: the guard trusts a bare NewTransport/ExtraCAs inside package
// netroute, so a second package with that name would borrow the proof.
func strayNetroute(root string, fset *token.FileSet, files []*ast.File) []string {
	want := filepath.Join(root, "netroute")
	var out []string
	for _, f := range files {
		if f.Name.Name != "netroute" {
			continue
		}
		p := fset.Position(f.Package).Filename
		if filepath.Dir(p) != want {
			out = append(out, p)
		}
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
