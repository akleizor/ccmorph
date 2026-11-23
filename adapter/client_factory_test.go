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
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checked++
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.Name == "newProbeClient" {
					continue // the factory itself
				}
				flagHTTPClients(t, fset, d.Body)
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					if d.Tok == token.VAR && (hasVarName(vs, "probeClient") || hasVarName(vs, "loopback")) {
						continue // the factory seam, and loopback's plain-http client
					}
					for _, v := range vs.Values {
						flagHTTPClients(t, fset, v)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("the guard read no production file")
	}
}

func hasVarName(vs *ast.ValueSpec, name string) bool {
	for _, n := range vs.Names {
		if n.Name == name {
			return true
		}
	}
	return false
}

// flagHTTPClients fails on every &http.Client{ built under node.
func flagHTTPClients(t *testing.T, fset *token.FileSet, node ast.Node) {
	t.Helper()
	if node == nil {
		return
	}
	ast.Inspect(node, func(n ast.Node) bool {
		u, ok := n.(*ast.UnaryExpr)
		if !ok || u.Op != token.AND {
			return true
		}
		lit, ok := u.X.(*ast.CompositeLit)
		if !ok || !isHTTPClient(lit.Type) {
			return true
		}
		t.Errorf("%s builds an *http.Client outside the factory: this client will not trust the session's extra CA (NODE_EXTRA_CA_CERTS), so a probe inside a Remote Control session fails its TLS handshake before any request leaves; build it with the factory (newProbeClient, reached as probeClient) (#822)",
			fset.Position(u.Pos()))
		return true
	})
}

func isHTTPClient(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "http" && sel.Sel.Name == "Client"
}
