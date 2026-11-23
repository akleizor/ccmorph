package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryProductionHTTPClientTrustsTheSessionsExtraCA extends #822's guard to
// the whole module. A production *http.Client (or an http.DefaultTransport
// clone) that honours the proxy environment but not NODE_EXTRA_CA_CERTS fails
// its TLS handshake inside a Remote Control session before any request leaves;
// the adapter guard could not see shim or ctxwindow, one package outside its
// walk. A client is allowed only by its own Transport: netroute.NewTransport
// (directly or through a local bound to it), a transport literal carrying
// netroute.ExtraCAs, or a literal setting Proxy: nil, so the session's proxy can
// never sign it. An unrelated builder call in the same function proves nothing.
func TestEveryProductionHTTPClientTrustsTheSessionsExtraCA(t *testing.T) {
	fset := token.NewFileSet()
	files, err := productionFiles("..", fset)
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
	if refused == 0 && allowed == 0 {
		t.Fatalf("the guard found no http client construction under ..: it is not reading the module")
	}
}

// TestTheWholeModuleGuardSeesEveryBypass pins what a scope-based allowlist
// misses: a sibling initializer in an allowed declaration, a method borrowing
// the constructor's name, a bare client beside an unrelated builder call, and
// a local transport that is not the one the builder hands out. The proof must
// be the client's own transport.
func TestTheWholeModuleGuardSeesEveryBypass(t *testing.T) {
	const src = `package p

import "net/http"

var plain = &http.Client{}
var timeouted = &http.Client{Timeout: time.Second}
var factory, hidden = caClient(), &http.Client{}
var direct = &http.Client{Transport: &http.Transport{Proxy: nil}}
var proxied = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}

func caClient() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: netroute.ExtraCAs()}}
}

func built() *http.Client {
	return &http.Client{Transport: netroute.NewTransport()}
}

func borrowed() *http.Client {
	tr := netroute.NewTransport()
	return &http.Client{Transport: tr}
}

func unrelated() *http.Client {
	_ = netroute.NewClient(1)
	return &http.Client{}
}

func unknownTransport(tr *http.Transport) *http.Client {
	return &http.Client{Transport: tr}
}

func cloneThenNoCA() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{Transport: tr}
}

func cloneThenCA() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = netroute.ExtraCAs()
	return &http.Client{Transport: tr}
}

func builtThenWiped() *http.Client {
	tr := netroute.NewTransport()
	tr.TLSClientConfig = nil
	return &http.Client{Transport: tr}
}

type bypass struct{}

func (bypass) caClient() *http.Client { return &http.Client{} }

func clone() *http.Transport {
	return http.DefaultTransport.(*http.Transport).Clone()
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	refused, allowed := []finding{}, 0
	for _, x := range inspectFile(fset, f) {
		if x.allowed {
			allowed++
			continue
		}
		refused = append(refused, x)
	}
	if len(refused) != 11 || allowed != 6 {
		t.Fatalf("guard refused %d and allowed %d construction(s), want 11 and 6: %v", len(refused), allowed, refused)
	}
	for _, x := range refused {
		if !strings.Contains(x.why, "NODE_EXTRA_CA_CERTS") || !strings.Contains(x.why, "netroute") {
			t.Errorf("a refusal does not name the cause and the fix: %s", x.why)
		}
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
	defaultTransportMessage = "uses http.DefaultTransport with no TLSClientConfig: it honours the session's proxy but not the CA it signs every host with (NODE_EXTRA_CA_CERTS), so its handshake fails inside a Remote Control session; clone it through netroute.NewTransport instead (#822, #829)"
)

// inspectFile reports every client construction and DefaultTransport use in f,
// refused unless that construction's own Transport proves it CA-aware or direct.
// Containment in a function that happens to call a builder proves nothing: the
// proof is the transport the client will actually dial with (#829).
func inspectFile(fset *token.FileSet, f *ast.File) []finding {
	regions := proofRegions(f)
	pkg := f.Name.Name
	var out []finding
	add := func(n ast.Node, allowed bool, msg string) {
		out = append(out, finding{pos: fset.Position(n.Pos()), allowed: allowed, why: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if isHTTPClient(x.Type) {
				add(x, clientTransportAllowed(x, innermost(regions, x), pkg), clientMessage)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isHTTPClient(x.Args[0]) {
				add(x, false, clientMessage) // new(http.Client) carries no transport at all
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" && x.Sel.Name == "DefaultTransport" {
				add(x, cloneGetsTheCA(x, innermost(regions, x), pkg), defaultTransportMessage)
			}
		}
		return true
	})
	return out
}

// clientTransportAllowed asks the construction's own Transport field. A client
// earns its place with the transport it names, never with an unrelated call in
// the same function.
func clientTransportAllowed(client *ast.CompositeLit, scope ast.Node, pkg string) bool {
	for _, elt := range client.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Transport" {
			continue
		}
		return transportAllowed(kv.Value, scope, pkg)
	}
	return false // no Transport: http.DefaultTransport, proxy honoured, CA ignored
}

// transportAllowed proves one transport expression: the CA-aware builder, a
// transport literal that carries the session's CA (or sets Proxy: nil and so
// never meets the session's proxy), or a local variable bound to one of those
// in the same scope.
func transportAllowed(expr ast.Expr, scope ast.Node, pkg string) bool {
	switch v := expr.(type) {
	case *ast.CallExpr:
		return namedNetrouteCall(v, pkg, "NewTransport")
	case *ast.Ident:
		return boundTransport(v.Name, scope, pkg)
	case *ast.UnaryExpr, *ast.CompositeLit:
		lit, ok := compositeOf(expr)
		if !ok || !isHTTPTransport(lit.Type) {
			return false
		}
		return tlsClientConfigCA(lit, scope, pkg) || directTransport(lit)
	}
	return false
}

// tlsClientConfigCA proves the transport literal itself sets TLSClientConfig
// from the session's CA.
func tlsClientConfigCA(lit *ast.CompositeLit, scope ast.Node, pkg string) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "TLSClientConfig" {
			continue
		}
		switch v := kv.Value.(type) {
		case *ast.CallExpr:
			return namedNetrouteCall(v, pkg, "ExtraCAs")
		case *ast.Ident:
			return boundFromExtraCAs(v.Name, scope, pkg)
		}
		return false
	}
	return false
}

// boundTransport is a local variable in this scope assigned from
// netroute.NewTransport, or a DefaultTransport clone whose TLSClientConfig the
// same scope then sets from ExtraCAs.
func boundTransport(name string, scope ast.Node, pkg string) bool {
	if scope == nil {
		return false
	}
	fromBuilder, cloned := false, false
	ast.Inspect(scope, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != name || i >= len(x.Rhs) {
					continue
				}
				if isDefaultTransportClone(x.Rhs[i]) {
					cloned = true
				} else if call, ok := x.Rhs[i].(*ast.CallExpr); ok && namedNetrouteCall(call, pkg, "NewTransport") {
					fromBuilder = true
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name != name || i >= len(x.Values) {
					continue
				}
				if isDefaultTransportClone(x.Values[i]) {
					cloned = true
				} else if call, ok := x.Values[i].(*ast.CallExpr); ok && namedNetrouteCall(call, pkg, "NewTransport") {
					fromBuilder = true
				}
			}
		}
		return true
	})
	return (fromBuilder || (cloned && setsTLSToCA(name, scope, pkg))) && !tlsReset(name, scope, pkg)
}

// tlsReset is whether the scope assigns name.TLSClientConfig anything but the
// session's CA; a later wipe cancels what the builder gave the transport.
func tlsReset(name string, scope ast.Node, pkg string) bool {
	reset := false
	ast.Inspect(scope, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
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
		switch v := as.Rhs[0].(type) {
		case *ast.CallExpr:
			reset = !namedNetrouteCall(v, pkg, "ExtraCAs")
		case *ast.Ident:
			reset = !boundFromExtraCAs(v.Name, scope, pkg)
		default:
			reset = true
		}
		return true
	})
	return reset
}

// setsTLSToCA is whether the scope assigns name.TLSClientConfig from ExtraCAs.
func setsTLSToCA(name string, scope ast.Node, pkg string) bool {
	found := false
	ast.Inspect(scope, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
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
		switch v := as.Rhs[0].(type) {
		case *ast.CallExpr:
			found = namedNetrouteCall(v, pkg, "ExtraCAs")
		case *ast.Ident:
			found = boundFromExtraCAs(v.Name, scope, pkg)
		}
		return true
	})
	return found
}

// boundFromExtraCAs is a local variable in this scope assigned from ExtraCAs.
func boundFromExtraCAs(name string, scope ast.Node, pkg string) bool {
	if scope == nil {
		return false
	}
	found := false
	ast.Inspect(scope, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				if id, ok := lhs.(*ast.Ident); !ok || id.Name != name || i >= len(x.Rhs) {
					continue
				}
				if call, ok := x.Rhs[i].(*ast.CallExpr); ok && namedNetrouteCall(call, pkg, "ExtraCAs") {
					found = true
				}
			}
		case *ast.ValueSpec:
			for i, id := range x.Names {
				if id.Name != name || i >= len(x.Values) {
					continue
				}
				if call, ok := x.Values[i].(*ast.CallExpr); ok && namedNetrouteCall(call, pkg, "ExtraCAs") {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// cloneGetsTheCA: a DefaultTransport use is allowed only when the clone is
// bound to a variable whose TLSClientConfig this same scope then sets from the
// session's CA, which is netroute.NewTransport's own body.
func cloneGetsTheCA(n ast.Node, scope ast.Node, pkg string) bool {
	if scope == nil {
		return false
	}
	name := ""
	ast.Inspect(scope, func(x ast.Node) bool {
		as, ok := x.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			if containsNode(rhs, n) && i < len(as.Lhs) {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					name = id.Name
				}
			}
		}
		return true
	})
	return name != "" && setsTLSToCA(name, scope, pkg) && !tlsReset(name, scope, pkg)
}

// containsNode is whether root's span holds n.
func containsNode(root ast.Node, n ast.Node) bool {
	return root.Pos() <= n.Pos() && n.End() <= root.End()
}

// isDefaultTransportClone is http.DefaultTransport.(*http.Transport).Clone().
func isDefaultTransportClone(expr ast.Expr) bool {
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
	if !isHTTPTransport(typ) {
		return false
	}
	dt, ok := ta.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	base, ok := dt.X.(*ast.Ident)
	return ok && base.Name == "http" && dt.Sel.Name == "DefaultTransport"
}

// namedNetrouteCall is a call to netroute's name, bare inside netroute and
// package-qualified everywhere else.
func namedNetrouteCall(call *ast.CallExpr, pkg, name string) bool {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return pkg == "netroute" && f.Name == name
	case *ast.SelectorExpr:
		id, ok := f.X.(*ast.Ident)
		return ok && id.Name == "netroute" && f.Sel.Name == name
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

// proofRegions are the places a proof can live: every function and func-literal
// body, and every package-level var value. A client's proof must sit in its own
// innermost region, so the sibling initializer of an allowed declaration cannot
// borrow it.
func proofRegions(f *ast.File) []ast.Node {
	var out []ast.Node
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				out = append(out, d.Body)
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
			out = append(out, lit.Body)
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

func isHTTPClient(expr ast.Expr) bool {
	if expr == nil {
		return false
	}
	lit, ok := expr.(*ast.CompositeLit)
	if ok {
		expr = lit.Type
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "Client"
}

func isHTTPTransport(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "Transport"
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
