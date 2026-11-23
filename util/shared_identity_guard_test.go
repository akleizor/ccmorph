package util

import (
	"fmt"
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

// GH-830 kinds, guarded at the layer where they are born: a test file.
//
// TestKeychainMigrateMovesThePlainKeyAndReadsItBack derived its fixture
// profile name from the base of a t.TempDir() path. Since Go 1.24 that base
// is the constant "001" — the per-run randomness moved to the parent — so the
// name was the same for every run, checkout and worktree on the machine, and
// two concurrent `go test ./util` runs sharing one login keychain raced in one
// account: the loser's security add-generic-password answered -25299, "The
// specified item already exists". The keychain is one shared namespace per
// Mac, and so are ports and global names, so a fixture identity that never
// varies across runs is a bug no Linux CI can see. These guards refuse the
// shapes that name a shared system-namespace identity with no per-run
// randomness:
//
//   - an identity derived from the base of a temp dir (its value is constant),
//   - a literal port bound outside the pool the repo's own rule reserves
//     (#176, #188: fixed-port tests take a number from 1024-32767, which no
//     bind-to-zero draw hands out, or bind :0),
//   - a test function reaching the real keychain code while leaving PATH
//     unchanged.
//
// Each detector works per function, so an unrelated variable of the same
// spelling elsewhere cannot be mistaken for a temp dir, and none of them
// accepts a hole: a PATH "stub" that re-exports os.Getenv("PATH") changes
// nothing and is refused. The shape is the one adapter/client_factory_test.go
// set: parse, inspect, refuse by node, and pin the detector against the old
// shape itself so the guard cannot rot.

// TestNoTestDerivesAnIdentityFromTheTempDirBase refuses every *_test.go that
// computes filepath.Base of a t.TempDir() path (directly, or through a name
// assigned from it in the same function): that base is "001" on every run, so
// anything built from it is a shared system-namespace identity, not per-run
// randomness.
func TestNoTestDerivesAnIdentityFromTheTempDirBase(t *testing.T) {
	fset, files := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range tempDirBaseFindings(fset, f) {
			t.Errorf("%s: this derives a value from the base of a t.TempDir() path. Since Go 1.24 that base is the constant \"001\" — the per-run randomness is the parent directory — so whatever is built from it is identical for every run, checkout and worktree: a keychain account or service, a fixed port, any global name then names one shared system-namespace identity, and concurrent runs collide in it (the keychain said add-generic-password: returned -25299, \"The specified item already exists\"). Derive the identity from the random parent instead — filepath.Base(filepath.Dir(t.TempDir())) — or stub the tool on PATH with state in t.TempDir(), so runs share nothing (#830)",
				fset.Position(pos))
		}
	}
}

// TestNoTestDerivesAnIdentityFromAFixedPort refuses a test that binds a
// literal port outside the window the repo's own fixed-port rule reserves:
// below 1024 it is a fixed system service's number, from 32768 up it is the
// ephemeral pool every parallel gate process draws from, so between the test
// choosing it and binding it a parallel process can take it (#176, #188).
func TestNoTestDerivesAnIdentityFromAFixedPort(t *testing.T) {
	fset, files := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range fixedPortFindings(fset, f) {
			t.Errorf("%s: this binds a literal fixed port from a test. A port is one system namespace per machine: below 1024 the number belongs to a fixed service, from 32768 up it comes from the ephemeral range (32768-60999 on Linux, 49152-65535 on macOS) that every parallel gate worker's bind-to-zero draw hands out, so a full gate races a test that is green alone. Bind :0, or take a number from 1024-32767 probed free, as portOutsideTheEphemeralPool in adapter/adapter_test.go does (#176, #188, #830)",
				fset.Position(pos))
		}
	}
}

// TestEveryKeychainTestStubsSecurityOnPath refuses a function that reaches
// the keychain code — the Keychain*/keychainPut/keychainMove helpers or an
// exec of security itself — without scoping PATH to a stub script this file
// writes: re-exporting os.Getenv("PATH") is not a stub, it is the same PATH
// the real /usr/bin/security answers from.
func TestEveryKeychainTestStubsSecurityOnPath(t *testing.T) {
	fset, files := parseRepoTestFiles(t)
	for _, f := range files {
		for _, fn := range unstubbedKeychainRefs(fset, f) {
			t.Errorf("%s: %s reaches the real login keychain: this file writes no security stub script, or the function scopes no PATH beyond what it inherits (t.Setenv(\"PATH\", os.Getenv(\"PATH\")) changes nothing). security is one namespace per machine, so a run here races every other session (add-generic-password: returned -25299), and an identity built from a temp dir cannot separate them — its base is \"001\" on every run since Go 1.24. Write the stub script and prepend its dir with t.Setenv, as the delete test in util/keychain_test.go does (#830)",
				fset.Position(fn.pos), fn.name)
		}
	}
}

// TestTheSharedIdentityGuardSeesEveryOldShape pins every detector on the
// shapes it exists for — the migrate test as it stood on main when #830 was
// filed, the bypasses a first cut of this guard accepted (a PATH "stub" that
// re-exports the inherited env; a same-spelled directory in another function)
// — and on the compliant shapes, so a future edit cannot quietly widen what
// the guards accept.
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
	const compliant = `package p

import (
	"os"
	"os/exec"
	"path/filepath"
)

func TestStubbed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := os.WriteFile(filepath.Join(dir, "security"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Skip()
	}
	if filepath.Base("/usr/libexec") != "libexec" {
		t.Skip()
	}
	KeychainDelete("x")
	_ = exec.Command("true")
}

func TestLocalDir(t *testing.T) {
	dir := t.TempDir()
	_ = dir
}

func TestOrdinaryBase(t *testing.T) {
	dir := "/usr/bin"
	if filepath.Base(dir) != "bin" {
		t.Fatal(dir)
	}
}
`
	const passthrough = `package p

import (
	"os"
	"os/exec"
)

func TestNoStub(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	_ = exec.Command("security", "default-keychain").Run()
}
`
	const fixedPort = `package p

import "net"

func TestFixedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:49173")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
}
`
	const poolPort = `package p

import "net"

func TestPoolPort(t *testing.T) {
	for _, a := range []string{"127.0.0.1:0", "127.0.0.1:8080", "127.0.0.1:32767", "[::1]:4000"} {
		if l, err := net.Listen("tcp", a); err == nil {
			l.Close()
		}
	}
}
`
	fset := token.NewFileSet()
	srcs := map[string]string{}
	parsed := map[string]*ast.File{}
	for name, src := range map[string]string{"old": oldMigrate, "compliant": compliant,
		"passthrough": passthrough, "fixedPort": fixedPort, "poolPort": poolPort} {
		srcs[name] = src
		f, err := parser.ParseFile(fset, name+".go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		parsed[name] = f
	}
	if got := tempDirBaseFindings(fset, parsed["old"]); len(got) != 1 {
		t.Errorf("the old name-from-tempdir-base shape produced %d findings, want 1", len(got))
	}
	if got := unstubbedKeychainRefs(fset, parsed["old"]); len(got) == 0 {
		t.Error("the old real-keychain shape produced no stub finding")
	} else if got[0].name != "TestKeychainMigrateMovesThePlainKeyAndReadsItBack" {
		t.Errorf("finding named %q, want the migrate test", got[0].name)
	}
	if got := tempDirBaseFindings(fset, parsed["compliant"]); len(got) != 0 {
		t.Errorf("the compliant shape produced %d base findings, want 0 (a same-spelled dir in another function must not be tracked across functions)", len(got))
	}
	if got := unstubbedKeychainRefs(fset, parsed["compliant"]); len(got) != 0 {
		t.Errorf("the compliant shape produced %d stub findings, want 0", len(got))
	}
	if got := unstubbedKeychainRefs(fset, parsed["passthrough"]); len(got) != 1 {
		t.Errorf("t.Setenv(\"PATH\", os.Getenv(\"PATH\")) produced %d findings, want 1: it leaves PATH pointed at the real security", len(got))
	}
	if got := fixedPortFindings(fset, parsed["fixedPort"]); len(got) != 1 {
		t.Errorf("net.Listen 127.0.0.1:49173 produced %d findings, want 1 (that port is in the ephemeral pool)", len(got))
	}
	if got := fixedPortFindings(fset, parsed["poolPort"]); len(got) != 0 {
		t.Errorf("the reserved-window and :0 ports produced %d findings, want 0", len(got))
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
// <anything>.Base(x) where x is a t.TempDir() call (inline) or a name the
// same function assigned from one. Scope is one function on purpose: a
// variable of the same spelling bound elsewhere, e.g. dir := "/usr/bin", is
// not a temp dir and must stay unflagged.
func tempDirBaseFindings(fset *token.FileSet, f *ast.File) []token.Pos {
	var out []token.Pos
	inFunc := func(body ast.Node) {
		temps := map[string]bool{}
		ast.Inspect(body, func(n ast.Node) bool {
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
		ast.Inspect(body, func(n ast.Node) bool {
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
	}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			inFunc(fn.Body)
		}
	}
	return out
}

// fixedPortFindings returns the position of every bind of a literal port
// outside 1024-32767 (and not :0, the ephemeral draw every listener here
// uses). It reads the address only where the bind happens to be a literal;
// a port passed by number, e.g. from portOutsideTheEphemeralPool, is the
// fix this message asks for, not a finding.
func fixedPortFindings(fset *token.FileSet, f *ast.File) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		argIndex := -1
		switch {
		case pkg.Name == "net":
			switch sel.Sel.Name {
			case "Listen", "ListenPacket", "ListenUnix", "ListenUnixgram", "ListenTLS":
				argIndex = 1
			}
		case pkg.Name == "http":
			switch sel.Sel.Name {
			case "ListenAndServe", "ListenAndServeTLS":
				argIndex = 0
			}
		}
		if argIndex < 0 || len(call.Args) <= argIndex {
			return true
		}
		lit, ok := call.Args[argIndex].(*ast.BasicLit)
		if !ok {
			return true
		}
		addr, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		i := strings.LastIndex(addr, ":")
		if i < 0 || strings.LastIndex(addr, "]") > i {
			return true
		}
		port, err := strconv.Atoi(addr[i+1:])
		if err != nil || port == 0 {
			return true
		}
		if port < 1024 || port > 32767 {
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
// keychainMove) or execs the security binary, without a PATH stub: a
// t.Setenv("PATH", x) in the function whose x is not the inherited
// os.Getenv("PATH") re-exported, plus a security stub script this file
// writes (filepath.Join(..., "security")). A stub scoped by a helper still
// requires the call-site function itself to re-scope PATH — the guard is the
// one thing that must not be fooled by indirection.
func unstubbedKeychainRefs(fset *token.FileSet, f *ast.File) []keychainRefSite {
	var writesStub bool
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Join" {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "filepath" {
				for _, a := range call.Args {
					if lit, ok := a.(*ast.BasicLit); ok && lit.Value == `"security"` {
						writesStub = true
					}
				}
			}
		}
		return true
	})
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
					if id, ok := sel.X.(*ast.Ident); ok && id.Name == "t" && sel.Sel.Name == "Setenv" &&
						len(x.Args) > 0 {
						if lit, ok := x.Args[0].(*ast.BasicLit); ok && lit.Value == `"PATH"` &&
							len(x.Args) > 1 && !isGetenvPATH(x.Args[1]) {
							hasStub = true
						}
					}
				}
			}
			return true
		})
		if reachedAt != token.NoPos && !(hasStub && writesStub) {
			out = append(out, keychainRefSite{pos: reachedAt, name: fn.Name.Name})
		}
	}
	return out
}

// isGetenvPATH reports whether x is exactly os.Getenv("PATH"): passing that
// to t.Setenv is the no-op that still resolves the real security binary.
func isGetenvPATH(x ast.Expr) bool {
	call, ok := x.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != "os" || sel.Sel.Name != "Getenv" {
		return false
	}
	lit, ok := call.Args[0].(*ast.BasicLit)
	return ok && lit.Value == `"PATH"`
}

func isTempDirCall(n ast.Expr) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "TempDir"
}
