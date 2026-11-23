package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
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
// A transport is allowed the same way. The names each file gives net/http and
// this module's netroute are all read (an alias is still the same package), a
// rewire after construction is checked the same way, and a package named
// netroute outside netroute/ is refused, because only there is a bare
// NewTransport or ExtraCAs trusted (#829).
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
	for _, f := range files {
		for _, x := range inspectFile(fset, f) {
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
// the http.DefaultClient helpers, a standalone proxied transport, and a
// mutation after construction (Transport, Proxy or TLSClientConfig). Each row
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
	mutationMessage         = "rewires a client or transport after construction (its Transport, Proxy or TLSClientConfig) with a value this guard cannot prove trusts the session's CA (NODE_EXTRA_CA_CERTS); build the finished client with netroute.NewClient or netroute.NewTransport, or set Proxy: nil in the literal (#829)"
	defaultTransportMessage = "uses http.DefaultTransport with no TLSClientConfig: it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its handshake fails inside a Remote Control session; clone it through netroute.NewTransport instead (#822, #829)"
)

const (
	// guardHead is the import set every row of the bypass table may use.
	guardHead = "package p\n\nimport (\n\t\"net/http\"\n\t\"time\"\n\n\t\"github.com/akleizor/ccmorph/netroute\"\n)\n\n"
	// netroutePath is the one package whose bare NewTransport/ExtraCAs count.
	netroutePath = "github.com/akleizor/ccmorph/netroute"
)

// guardSrc is a row body under the shared import set.
func guardSrc(body string) string { return guardHead + body + "\n" }

// fileSrc is a whole file whose text follows the package clause, including its
// own imports.
func fileSrc(rest string) string { return "package p\n\n" + rest }

// names is how one file spells the packages the guard reasons about: every
// local name of net/http and of this module's netroute, whether the file is
// package netroute (its bare NewTransport/ExtraCAs count), and any dot import,
// which hides both from the guard.
type names struct {
	http, netroute map[string]bool
	inNetroute     bool
	dots           []*ast.ImportSpec
}

func fileNames(f *ast.File) names {
	n := names{http: map[string]bool{}, netroute: map[string]bool{}, inNetroute: f.Name.Name == "netroute"}
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		var into map[string]bool
		local := ""
		switch path {
		case "net/http":
			into, local = n.http, "http"
		case netroutePath:
			into, local = n.netroute, "netroute"
		default:
			continue
		}
		if spec.Name != nil {
			local = spec.Name.Name
		}
		switch local {
		case "_":
		case ".":
			n.dots = append(n.dots, spec)
		default:
			into[local] = true
		}
	}
	return n
}

// is is whether expr names <a package name in pkgs>.<sel>.
func is(expr ast.Expr, pkgs map[string]bool, sel string) bool {
	s, ok := expr.(*ast.SelectorExpr)
	if !ok || s.Sel.Name != sel {
		return false
	}
	id, ok := s.X.(*ast.Ident)
	return ok && pkgs[id.Name]
}

// inspectFile reports every client construction, transport construction and
// rewire in f, refused unless its own transport proves it CA-aware or direct.
// Containment in a function that happens to call a builder proves nothing: the
// proof is the transport the client will actually dial with (#829).
func inspectFile(fset *token.FileSet, f *ast.File) []finding {
	regions := proofRegions(f)
	n := fileNames(f)
	var out []finding
	add := func(node ast.Node, allowed bool, msg string) {
		out = append(out, finding{pos: fset.Position(node.Pos()), allowed: allowed, why: msg})
	}
	for _, spec := range n.dots {
		add(spec, false, dotImportMessage)
	}
	// framed are the selectors that are types, not names used as values: the
	// element of a *http.Client or the type of an &http.Client literal.
	framed := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(node ast.Node) bool {
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
	ast.Inspect(f, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.CompositeLit:
			scope := innermost(regions, x)
			switch {
			case is(x.Type, n.http, "Client"):
				add(x, clientTransportAllowed(x, scope, n), clientMessage)
			case is(x.Type, n.http, "Transport"):
				add(x, tlsClientConfigCA(x, scope, n) || directTransport(x), transportMessage)
			case x.Type != nil && mentionsHTTPType(x.Type, n):
				add(x, false, typeUseMessage)
			}
		case *ast.SelectorExpr:
			switch {
			case is(x, n.http, "DefaultTransport"):
				add(x, cloneGetsTheCA(x, innermost(regions, x), n), defaultTransportMessage)
			case (is(x, n.http, "Client") || is(x, n.http, "Transport")) && !framed[x]:
				add(x, false, typeUseMessage)
			case is(x, n.http, "DefaultClient") || is(x, n.http, "Get") || is(x, n.http, "Head") || is(x, n.http, "Post") || is(x, n.http, "PostForm"):
				add(x, false, defaultClientMessage)
			}
		case *ast.AssignStmt:
			scope := innermost(regions, x)
			for i, lhs := range x.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				var rhs ast.Expr
				if len(x.Lhs) == len(x.Rhs) {
					rhs = x.Rhs[i]
				}
				switch {
				case throughTLS(sel.X):
					add(sel, false, mutationMessage)
				case sel.Sel.Name == "Transport":
					add(sel, rhs != nil && transportAllowed(rhs, scope, n), mutationMessage)
				case sel.Sel.Name == "Proxy":
					add(sel, rhs != nil && spelled(rhs) == "nil", mutationMessage)
				case sel.Sel.Name == "TLSClientConfig":
					add(sel, rhs != nil && caConfig(rhs, scope, n), mutationMessage)
				}
			}
		}
		return true
	})
	return out
}

// throughTLS is whether expr reaches through a TLSClientConfig selector, so the
// assignment rewrites the config the builder set.
func throughTLS(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(x ast.Node) bool {
		if s, ok := x.(*ast.SelectorExpr); ok && s.Sel.Name == "TLSClientConfig" {
			found = true
		}
		return true
	})
	return found
}

// mentionsHTTPType is whether a literal's type names a client or transport, as
// a slice or map element or a type argument does: such an element is elided,
// so the client rules never see it.
func mentionsHTTPType(expr ast.Expr, n names) bool {
	found := false
	ast.Inspect(expr, func(x ast.Node) bool {
		if e, ok := x.(ast.Expr); ok && (is(e, n.http, "Client") || is(e, n.http, "Transport")) {
			found = true
		}
		return true
	})
	return found
}

// caConfig proves one TLS config expression: netroute.ExtraCAs, or a local
// bound only to it.
func caConfig(v ast.Expr, scope ast.Node, n names) bool {
	switch x := v.(type) {
	case *ast.CallExpr:
		return namedNetrouteCall(x, n, "ExtraCAs")
	case *ast.Ident:
		return boundFromExtraCAs(x.Name, scope, n)
	}
	return false
}

// clientTransportAllowed asks the construction's own Transport field. A client
// earns its place with the transport it names, never with an unrelated call in
// the same function.
func clientTransportAllowed(client *ast.CompositeLit, scope ast.Node, n names) bool {
	for _, elt := range client.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Transport" {
			continue
		}
		return transportAllowed(kv.Value, scope, n)
	}
	return false // no Transport: http.DefaultTransport, proxy honoured, CA ignored
}

// transportAllowed proves one transport expression: the CA-aware builder, a
// transport literal that carries the session's CA (or sets Proxy: nil and so
// never meets the session's proxy), or a local variable bound only to one of
// those in the same scope.
func transportAllowed(expr ast.Expr, scope ast.Node, n names) bool {
	switch v := expr.(type) {
	case *ast.CallExpr:
		return namedNetrouteCall(v, n, "NewTransport")
	case *ast.Ident:
		return boundTransport(v.Name, scope, n)
	case *ast.UnaryExpr, *ast.CompositeLit:
		lit, ok := compositeOf(expr)
		if !ok || !is(lit.Type, n.http, "Transport") {
			return false
		}
		return tlsClientConfigCA(lit, scope, n) || directTransport(lit)
	}
	return false
}

// tlsClientConfigCA proves the transport literal itself sets TLSClientConfig
// from the session's CA.
func tlsClientConfigCA(lit *ast.CompositeLit, scope ast.Node, n names) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "TLSClientConfig" {
			continue
		}
		return caConfig(kv.Value, scope, n)
	}
	return false
}

// onlyBoundBy is whether scope binds name at least once and every binding
// passes ok. A parameter, field, range variable, address taken or valueless
// declaration is a binding that passes nothing, so a builder's proof is lost as
// soon as the variable may hold anything else.
func onlyBoundBy(name string, scope ast.Node, ok func(ast.Expr) bool) bool {
	if scope == nil {
		return false
	}
	good, other := 0, 0
	judge := func(v ast.Expr) {
		if ok(v) {
			good++
		} else {
			other++
		}
	}
	ast.Inspect(scope, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, isID := lhs.(*ast.Ident); !isID || id.Name != name {
					continue
				}
				if len(x.Lhs) != len(x.Rhs) {
					other++
					continue
				}
				judge(x.Rhs[i])
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name != name {
					continue
				}
				if len(x.Values) != len(x.Names) {
					other++
					continue
				}
				judge(x.Values[i])
			}
		case *ast.Field:
			for _, id := range x.Names {
				if id.Name == name {
					other++
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{x.Key, x.Value} {
				if id, isID := e.(*ast.Ident); isID && id.Name == name {
					other++
				}
			}
		case *ast.UnaryExpr:
			if id, isID := x.X.(*ast.Ident); isID && x.Op == token.AND && id.Name == name {
				other++
			}
		}
		return true
	})
	return good > 0 && other == 0
}

// boundTransport is a local variable in this scope bound only to
// netroute.NewTransport, or bound only to a DefaultTransport clone whose
// TLSClientConfig the same scope then sets from ExtraCAs.
func boundTransport(name string, scope ast.Node, n names) bool {
	builder := func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		return ok && namedNetrouteCall(c, n, "NewTransport")
	}
	clone := func(e ast.Expr) bool { return isDefaultTransportClone(e, n) }
	return (onlyBoundBy(name, scope, builder) || (onlyBoundBy(name, scope, clone) && setsTLSToCA(name, scope, n))) && !tlsReset(name, scope, n)
}

// tlsReset is whether the scope assigns name.TLSClientConfig anything but the
// session's CA; a later wipe cancels what the builder gave the transport.
func tlsReset(name string, scope ast.Node, n names) bool {
	reset := false
	ast.Inspect(scope, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "TLSClientConfig" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != name {
			return true
		}
		if !caConfig(as.Rhs[0], scope, n) {
			reset = true
		}
		return true
	})
	return reset
}

// setsTLSToCA is whether the scope assigns name.TLSClientConfig from ExtraCAs.
func setsTLSToCA(name string, scope ast.Node, n names) bool {
	found := false
	ast.Inspect(scope, func(node ast.Node) bool {
		as, ok := node.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		sel, ok := as.Lhs[0].(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "TLSClientConfig" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != name {
			return true
		}
		found = caConfig(as.Rhs[0], scope, n)
		return true
	})
	return found
}

// boundFromExtraCAs is a local variable in this scope bound only to ExtraCAs.
func boundFromExtraCAs(name string, scope ast.Node, n names) bool {
	return onlyBoundBy(name, scope, func(e ast.Expr) bool {
		c, ok := e.(*ast.CallExpr)
		return ok && namedNetrouteCall(c, n, "ExtraCAs")
	})
}

// cloneGetsTheCA: a DefaultTransport use is allowed only when the clone is
// bound to a variable bound only to clones, whose TLSClientConfig this same
// scope then sets from the session's CA, which is netroute.NewTransport's own
// body.
func cloneGetsTheCA(use ast.Node, scope ast.Node, n names) bool {
	if scope == nil {
		return false
	}
	name := ""
	ast.Inspect(scope, func(x ast.Node) bool {
		as, ok := x.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, rhs := range as.Rhs {
			if containsNode(rhs, use) && isDefaultTransportClone(rhs, n) {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					name = id.Name
				}
			}
		}
		return true
	})
	clone := func(e ast.Expr) bool { return isDefaultTransportClone(e, n) }
	return name != "" && onlyBoundBy(name, scope, clone) && setsTLSToCA(name, scope, n) && !tlsReset(name, scope, n)
}

// containsNode is whether root's span holds n.
func containsNode(root ast.Node, n ast.Node) bool {
	return root.Pos() <= n.Pos() && n.End() <= root.End()
}

// isDefaultTransportClone is http.DefaultTransport.(*http.Transport).Clone().
func isDefaultTransportClone(expr ast.Expr, n names) bool {
	call, ok := expr.(*ast.CallExpr)
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
		typ = star.X // http.DefaultTransport.(*http.Transport).Clone()
	}
	return is(typ, n.http, "Transport") && is(ta.X, n.http, "DefaultTransport")
}

// namedNetrouteCall is a call to netroute's name, bare inside package netroute
// and package-qualified everywhere else.
func namedNetrouteCall(call *ast.CallExpr, n names, name string) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return n.inNetroute && f.Name == name
	case *ast.SelectorExpr:
		return is(f, n.netroute, name)
	}
	return false
}

// directTransport is a transport literal written in place with Proxy: nil (or no
// Proxy field at all): it never consults HTTPS_PROXY, so the session's proxy can
// never sign it and no extra CA is needed.
func directTransport(lit *ast.CompositeLit) bool {
	proxy := ""
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Proxy" {
			proxy = spelled(kv.Value)
		}
	}
	return proxy == "" || proxy == "nil" // absent means nil, a direct dial
}

// proofRegions are the places a proof can live: every function or func-literal
// (with its parameters, so a parameter of the same name is seen), and every
// package-level var value. A client's proof must sit in its own innermost
// region, so the sibling initializer of an allowed declaration cannot borrow
// it.
func proofRegions(f *ast.File) []ast.Node {
	var out []ast.Node
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out = append(out, d)
			}
		case *ast.GenDecl:
			if d.Tok != token.VAR {
				continue
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, v := range vs.Values {
					out = append(out, v)
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if lit, ok := n.(*ast.FuncLit); ok {
			out = append(out, lit)
		}
		return true
	})
	return out
}

// innermost is the smallest region containing n, or nil when none does.
func innermost(regions []ast.Node, n ast.Node) ast.Node {
	var best ast.Node
	for _, r := range regions {
		if r.Pos() > n.Pos() || n.End() > r.End() {
			continue
		}
		if best == nil || r.End()-r.Pos() < best.End()-best.Pos() {
			best = r
		}
	}
	return best
}

// compositeOf unwraps the & of an &http.Transport literal.
func compositeOf(expr ast.Expr) (*ast.CompositeLit, bool) {
	if u, ok := expr.(*ast.UnaryExpr); ok && u.Op == token.AND {
		expr = u.X
	}
	lit, ok := expr.(*ast.CompositeLit)
	return lit, ok
}

// spelled is the one identifier expr names, for the nil Proxy test.
func spelled(expr ast.Expr) string {
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
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
