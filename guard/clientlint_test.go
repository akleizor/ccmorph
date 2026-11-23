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
// walk. A client is allowed only by proof, in its own innermost function or
// initializer: it is built through netroute.NewClient/NewTransport (or reads
// netroute.ExtraCAs), or its transport sets Proxy: nil, so the session's proxy
// can never sign it.
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

// TestTheWholeModuleGuardSeesEveryBypass pins what a name-based allowlist
// misses: a sibling initializer in an allowed declaration, a method borrowing
// the constructor's name, and a bare client no factory touches. Only the
// CA-aware constructions and the explicit direct transport are allowed.
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
	if len(refused) != 6 || allowed != 3 {
		t.Fatalf("guard refused %d and allowed %d construction(s), want 6 and 3: %v", len(refused), allowed, refused)
	}
	for _, x := range refused {
		if !strings.Contains(x.why, "NODE_EXTRA_CA_CERTS") || !strings.Contains(x.why, "netroute") {
			t.Errorf("a refusal does not name the cause and the fix: %s", x.why)
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
// refused unless its own innermost scope proves it CA-aware or direct.
func inspectFile(fset *token.FileSet, f *ast.File) []finding {
	regions := proofRegions(f)
	var out []finding
	add := func(n ast.Node, allowed bool, msg string) {
		out = append(out, finding{pos: fset.Position(n.Pos()), allowed: allowed, why: msg})
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if isHTTPClient(x.Type) {
				add(x, caAware(innermost(regions, x), f.Name.Name) || direct(x), clientMessage)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isHTTPClient(x.Args[0]) {
				add(x, caAware(innermost(regions, x), f.Name.Name), clientMessage)
			}
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "http" && x.Sel.Name == "DefaultTransport" {
				add(x, caAware(innermost(regions, x), f.Name.Name), defaultTransportMessage)
			}
		}
		return true
	})
	return out
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

// caAware is whether scope builds through netroute's CA-aware constructor (or
// reads the session's CA directly). Inside netroute the names are bare; every
// other package must spell them through the netroute package.
func caAware(scope ast.Node, pkg string) bool {
	if scope == nil {
		return false
	}
	found := false
	ast.Inspect(scope, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			if pkg == "netroute" && caName(f.Name) {
				found = true
			}
		case *ast.SelectorExpr:
			if id, ok := f.X.(*ast.Ident); ok && id.Name == "netroute" && caName(f.Sel.Name) {
				found = true
			}
		}
		return true
	})
	return found
}

func caName(name string) bool {
	return name == "ExtraCAs" || name == "NewTransport" || name == "NewClient"
}

// direct is a client whose transport is written in place with Proxy: nil (or no
// Proxy field at all): it never consults HTTPS_PROXY, so the session's proxy
// can never sign it and no extra CA is needed. A client with no Transport field
// uses http.DefaultTransport and is not direct.
func direct(lit *ast.CompositeLit) bool {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Transport" {
			continue
		}
		tlit, ok := compositeOf(kv.Value)
		if !ok || !isHTTPTransport(tlit.Type) {
			return false
		}
		proxy := ""
		for _, telt := range tlit.Elts {
			tkv, ok := telt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := tkv.Key.(*ast.Ident); ok && key.Name == "Proxy" {
				proxy = spelled(tkv.Value)
			}
		}
		return proxy == "" || proxy == "nil" // absent means nil, a direct dial
	}
	return false
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
