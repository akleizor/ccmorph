package adapter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestOnlyTheProbeClientFactoryBuildsAnHTTPClient guards the kind #822 fixed.
// A hand-built &http.Client{} silently drops the session's extra CA
// (NODE_EXTRA_CA_CERTS), so inside a Remote Control session a classification
// probe fails its TLS handshake before any request leaves and a launch that
// must classify a new model cannot start. Every probe client is therefore
// built by the newProbeClient factory, which reads netroute.ExtraCAs() at call
// time; loopback is the one other client, and it keeps its own, because it
// speaks plain http.
func TestOnlyTheProbeClientFactoryBuildsAnHTTPClient(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("the guard read no production file")
	}
	// Allow exactly two constructions, by node: the factory's own and
	// loopback's plain-http one. No declaration is skipped by the name it
	// carries, so an extra initializer cannot hide in an allowed variable's
	// declaration: `var probeClient, other = newProbeClient, &http.Client{}`
	// still trips the guard.
	allowed := allowedHTTPClients(files)
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if u, ok := httpClientLit(n); ok && !allowed[u] {
				t.Errorf("%s builds an *http.Client outside the factory: this client will not trust the session's extra CA (NODE_EXTRA_CA_CERTS), so a probe inside a Remote Control session fails its TLS handshake before any request leaves; build it with the factory (newProbeClient, reached as probeClient) (#822)",
					fset.Position(u.Pos()))
			}
			return true
		})
	}
}

// TestTheClientGuardSeesEveryInitializer pins the bypass this guard first
// missed: an extra initializer that shares a declaration with an allowed name
// must still be refused, and only the factory's and loopback's own literals
// are allowed. The old name-based skip read
// `var probeClient, other = newProbeClient, &http.Client{}` as the factory
// seam and let the second client through.
func TestTheClientGuardSeesEveryInitializer(t *testing.T) {
	const src = `package p

var probeClient, unsafeProductionClient = newProbeClient, &http.Client{}
var loopback = &http.Client{}

func newProbeClient() *http.Client { return &http.Client{} }
`
	f, err := parser.ParseFile(token.NewFileSet(), "synthetic.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := allowedHTTPClients([]*ast.File{f})
	allowedCount, refusedCount := 0, 0
	ast.Inspect(f, func(n ast.Node) bool {
		if u, ok := httpClientLit(n); ok {
			if allowed[u] {
				allowedCount++
			} else {
				refusedCount++
			}
		}
		return true
	})
	if allowedCount != 2 || refusedCount != 1 {
		t.Fatalf("guard allowed %d and refused %d client literal(s), want 2 and 1", allowedCount, refusedCount)
	}
}

// allowedHTTPClients is the guard's allowlist, keyed by node so that an
// initializer beside an allowed name in the same declaration is still seen.
func allowedHTTPClients(files []*ast.File) map[ast.Node]bool {
	allowed := map[ast.Node]bool{}
	mark := func(node ast.Node) {
		ast.Inspect(node, func(n ast.Node) bool {
			if u, ok := httpClientLit(n); ok {
				allowed[u] = true
			}
			return true
		})
	}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "newProbeClient" {
					mark(d.Body) // the one factory
				}
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || len(vs.Names) != len(vs.Values) {
						continue
					}
					for i, n := range vs.Names {
						if n.Name == "loopback" {
							mark(vs.Values[i]) // loopback speaks plain http
						}
					}
				}
			}
		}
	}
	return allowed
}

// httpClientLit is the &http.Client{} a node builds, when it builds one.
func httpClientLit(n ast.Node) (*ast.UnaryExpr, bool) {
	u, ok := n.(*ast.UnaryExpr)
	if !ok || u.Op != token.AND {
		return nil, false
	}
	lit, ok := u.X.(*ast.CompositeLit)
	if !ok || !isHTTPClient(lit.Type) {
		return nil, false
	}
	return u, true
}

func isHTTPClient(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "Client"
}
