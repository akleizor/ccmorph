package util

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GH-830 kinds, guarded at the layer where they are born: a test file.
//
// TestKeychainMigrateMovesThePlainKeyAndReadsItBack derived its fixture
// profile name from the base of a t.TempDir() path. Since Go 1.24 that base
// is the constant "001" — the per-run randomness moved to the parent — so the
// name was the same for every run, checkout and worktree on the machine, and
// two concurrent `go test ./util` runs sharing one login keychain raced in one
// account: the loser's security add-generic-password answered -25299, "The
// specified item already exists". The keychain is one shared namespace per
// Mac, so a fixture identity that never varies across runs is a bug no Linux
// CI can see. These guards refuse both halves of the kind:
//
//   - an identity derived from the base of a temp dir (its value is constant),
//   - a test function reaching the real keychain code with no PATH stub, so
//     the machine's one keychain is what it reaches.
//
// The shape is the one adapter/client_factory_test.go set: parse, inspect,
// refuse by node, and pin the detector against the old shape itself so the
// guard cannot rot.

// TestNoTestDerivesAnIdentityFromTheTempDirBase refuses every *_test.go that
// computes filepath.Base of a t.TempDir() path (directly, or through a name
// assigned from it): that base is "001" on every run, so anything built from
// it is a shared system-namespace identity, not per-run randomness.
func TestNoTestDerivesAnIdentityFromTheTempDirBase(t *testing.T) {
	fset, files := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range tempDirBaseFindings(fset, f) {
			t.Errorf("%s: this derives a value from the base of a t.TempDir() path. Since Go 1.24 that base is the constant \"001\" — the per-run randomness is the parent directory — so whatever is built from it is identical for every run, checkout and worktree: a keychain account or service, a fixed port, any global name then names one shared system-namespace identity, and concurrent runs collide in it (the keychain said add-generic-password: returned -25299, \"The specified item already exists\"). Derive the identity from the random parent instead — filepath.Base(filepath.Dir(t.TempDir())) — or stub the tool on PATH with state in t.TempDir(), so runs share nothing (#830)",
				fset.Position(pos))
		}
	}
}

// TestEveryKeychainTestStubsSecurityOnPath refuses a test function that
// reaches the keychain code — the Keychain*/keychainPut/keychainMove helpers
// or an exec of security itself — while setting no PATH stub, because what it
// then reaches is the one login keychain of the machine it runs on.
func TestEveryKeychainTestStubsSecurityOnPath(t *testing.T) {
	fset, files := parseRepoTestFiles(t)
	for _, f := range files {
		for _, fn := range unstubbedKeychainRefs(fset, f) {
			t.Errorf("%s: %s reaches the real login keychain from a test function that sets no PATH stub: security is one namespace per machine, so a run here races every other session (add-generic-password: returned -25299), and an identity built from a temp dir cannot separate them — its base is \"001\" on every run since Go 1.24. Write the stub script and scope it with t.Setenv, as the delete test in util/keychain_test.go does (#830)",
				fset.Position(fn.pos), fn.name)
		}
	}
}

// TestTheSharedIdentityGuardSeesEveryOldShape pins both detectors on the
// migrate test as it stood on main when #830 was filed, and on the compliant
// shapes, so a future edit cannot quietly widen what the guards accept.
func TestTheSharedIdentityGuardSeesEveryOldShape(t *testing.T) {
	const oldMigrate = `package p

import (
	"os/exec"
	"path/filepath"
	"strings"
)

func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if exec.Command("security", "default-keychain").Run() != nil {
		t.Skip("no login keychain")
	}
	dir := t.TempDir()
	name := "ccmtest" + strings.ReplaceAll(filepath.Base(dir), "-", "")
	t.Cleanup(func() { KeychainDelete(name) })
	if err := KeychainMigrate("state"); err != nil {
		panic(err)
	}
	if _, err := KeychainGet("keychain:" + name + ".api_key"); err != nil {
		panic(err)
	}
}
`
	const stubbed = `package p

import "path/filepath"

func TestStubbed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	if filepath.Base("/usr/libexec") != "libexec" {
		t.Skip()
	}
	KeychainDelete("x")
}
`
	fset := token.NewFileSet()
	old, err := parser.ParseFile(fset, "oldshape.go", oldMigrate, 0)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := parser.ParseFile(fset, "stubbed.go", stubbed, 0)
	if err != nil {
		t.Fatal(err)
	}

	if got := tempDirBaseFindings(fset, old); len(got) != 1 {
		t.Errorf("the old name-from-tempdir-base shape produced %d findings, want 1", len(got))
	}
	if got := unstubbedKeychainRefs(fset, old); len(got) == 0 {
		t.Error("the old real-keychain shape produced no stub finding")
	} else if got[0].name != "TestKeychainMigrateMovesThePlainKeyAndReadsItBack" {
		t.Errorf("finding named %q, want the migrate test", got[0].name)
	}
	if got := tempDirBaseFindings(fset, ok); len(got) != 0 {
		t.Errorf("the stubbed shape produced %d base findings, want 0", len(got))
	}
	if got := unstubbedKeychainRefs(fset, ok); len(got) != 0 {
		t.Errorf("the stubbed shape produced %d stub findings, want 0", len(got))
	}
}

// parseRepoTestFiles parses every *_test.go of the module, from the package
// directory the suite runs in (cwd for `go test ./pkg`), skipping hidden
// directories. It fails rather than reporting an empty scan, so a guard that
// stopped reading files cannot pass.
func parseRepoTestFiles(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	root := ".."
	if _, err := os.Stat("go.mod"); err == nil {
		root = "."
	}
	fset := token.NewFileSet()
	var files []*ast.File
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return fmt.Errorf("%s: %v", p, perr)
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("the guard read no test file")
	}
	return fset, files
}

// tempDirBaseFindings returns the position of every call of the shape
// <anything>.Base(x) where x is a t.TempDir() call or a name assigned from
// one, anywhere in the file.
func tempDirBaseFindings(fset *token.FileSet, f *ast.File) []token.Pos {
	temps := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range d.Rhs {
				if i < len(d.Lhs) && isTempDirCall(rhs) {
					if id, ok := d.Lhs[i].(*ast.Ident); ok {
						temps[id.Name] = true
					}
				}
			}
		case *ast.ValueSpec:
			for i, rhs := range d.Values {
				if i < len(d.Names) && isTempDirCall(rhs) {
					temps[d.Names[i].Name] = true
				}
			}
		}
		return true
	})
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.SelectorExpr:
			if fun.Sel.Name != "Base" {
				return true
			}
		case *ast.Ident:
			if fun.Name != "Base" {
				return true
			}
		default:
			return true
		}
		if isTempDirCall(call.Args[0]) {
			out = append(out, call.Pos())
			return true
		}
		if id, ok := call.Args[0].(*ast.Ident); ok && temps[id.Name] {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

type keychainRefSite struct {
	pos  token.Pos
	name string
}

// unstubbedKeychainRefs returns every function of the file that references
// the keychain helpers (an identifier starting with Keychain, keychainPut,
// keychainMove) or execs the security binary, without a t.Setenv of "PATH"
// anywhere in its body. A stub set by a helper the function calls still
// counts only if the call site itself re-scopes PATH — by design: the guard
// is the one thing that must not be fooled by indirection.
func unstubbedKeychainRefs(fset *token.FileSet, f *ast.File) []keychainRefSite {
	var out []keychainRefSite
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var reachedAt token.Pos
		var hasStub bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if strings.HasPrefix(x.Name, "Keychain") || x.Name == "keychainPut" || x.Name == "keychainMove" {
					if reachedAt == token.NoPos {
						reachedAt = x.Pos()
					}
				}
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "exec" && sel.Sel.Name == "Command" &&
						len(x.Args) > 0 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Value == `"security"` {
							if reachedAt == token.NoPos {
								reachedAt = x.Pos()
							}
						}
					}
					if sel.Sel.Name == "Setenv" && len(x.Args) > 0 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Value == `"PATH"` {
							hasStub = true
						}
					}
				}
			}
			return true
		})
		if reachedAt != token.NoPos && !hasStub {
			out = append(out, keychainRefSite{pos: reachedAt, name: fn.Name.Name})
		}
	}
	return out
}

func isTempDirCall(n ast.Expr) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "TempDir"
}
