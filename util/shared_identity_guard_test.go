package util

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
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
// Mac, and so are ports and file or socket names, so a fixture identity that
// never varies across runs is a bug no Linux CI can see. These guards refuse,
// per test function, the shapes that name a shared system-namespace identity
// with no per-run randomness:
//
//   - an identity derived from the base of a temp dir (its value is constant),
//   - a constant port at a bind site (take :0, or draw a number from the
//     1024-32767 window no bind-to-zero draw hands out, as #176/#188 say),
//   - a file or socket created at a constant path,
//   - the keychain reached while the function's fake security is not the one
//     stubSecurity(t) installs.
//
// The keychain rule trusts exactly one stub: the plain func stubSecurity in
// util/keychain_test.go, which puts this test binary on PATH as security
// (GH-830). No static evidence — a written script, a t.Setenv, a Join — can
// prove a stub is what runs, so the guard accepts none as proof; the runtime
// tests in keychain_test.go pin that helper. A second declaration of the name
// is itself a finding, and with no such declaration (a probe module, a
// sibling checkout) every function that reaches the keychain is refused.
//
// Scope is one declaration, and a reach is covered only by a stub statement
// in a body enclosing it and before it; a same-module helper that writes PATH
// is followed one level. Deeper indirection is not modelled. The shape is the
// one adapter/client_factory_test.go set: parse, inspect, refuse by node, and
// pin every detector on the old shapes, the bypasses and the compliant shapes
// so the guard cannot rot.

const trustedStubFile = "util/keychain_test.go"

// identityFix is the half of every message that names the cause and the fix,
// so a reader of any finding learns both without opening this file.
const identityFix = " Since Go 1.24 the base of a t.TempDir() path is the constant \"001\" — the per-run randomness is its parent — so whatever is built from it is the same identity for every run, checkout and worktree, and concurrent runs collide in it: the keychain answered add-generic-password: returned -25299, \"The specified item already exists\". Fix: derive the identity from the random parent (filepath.Base(filepath.Dir(t.TempDir()))); keep files and sockets under t.TempDir(); bind :0 or draw a port the way portOutsideTheEphemeralPool in adapter/adapter_test.go does; reach the keychain only through stubSecurity(t) in util/keychain_test.go (#830)."

const (
	tempDirBaseMsg = "this derives a value from the base of a t.TempDir() path." + identityFix
	fixedPortMsg   = "this binds a constant port from a test." + identityFix
	fixedPathMsg   = "this creates a file or socket at a constant path from a test." + identityFix
	unstubbedMsg   = "this reaches the real login keychain: this file declares no security stub, or the function does not call stubSecurity(t) before the reach, or it writes PATH after the stub." + identityFix
	strayStubMsg   = "this declares stubSecurity somewhere other than the one plain func in util/keychain_test.go; the guard trusts only that one, because a second declaration could stand in for the stub without installing it." + identityFix
)

// TestNoTestDerivesAnIdentityFromTheTempDirBase refuses every *_test.go that
// computes filepath.Base of a t.TempDir() path (directly, or through a name
// assigned from it, or an alias): that base is "001" on every run, so anything
// built from it is a shared system-namespace identity.
func TestNoTestDerivesAnIdentityFromTheTempDirBase(t *testing.T) {
	fset, files, _ := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range tempDirBaseFindings(f) {
			t.Errorf("%s: %s", fset.Position(pos), tempDirBaseMsg)
		}
	}
}

// TestNoTestDerivesAnIdentityFromAFixedPort refuses a test that binds a
// constant port. A port is one system namespace per machine, so a test that
// pins one races every concurrent run and every parallel gate worker.
func TestNoTestDerivesAnIdentityFromAFixedPort(t *testing.T) {
	fset, files, _ := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range fixedPortFindings(f) {
			t.Errorf("%s: %s", fset.Position(pos), fixedPortMsg)
		}
	}
}

// TestNoTestDerivesAnIdentityFromAFixedPath refuses a test that creates a file
// or socket at a constant path.
func TestNoTestDerivesAnIdentityFromAFixedPath(t *testing.T) {
	fset, files, _ := parseRepoTestFiles(t)
	for _, f := range files {
		for _, pos := range fixedPathFindings(f) {
			t.Errorf("%s: %s", fset.Position(pos), fixedPathMsg)
		}
	}
}

// TestEveryKeychainTestStubsSecurityOnPath refuses a function that reaches the
// keychain code without calling stubSecurity(t) before the reach, refuses any
// second declaration of stubSecurity, and is red with no stub declaration at
// all.
func TestEveryKeychainTestStubsSecurityOnPath(t *testing.T) {
	fset, files, root := parseRepoTestFiles(t)
	trusted := trustedStub(fset, files, root)
	writers := pathWriterFuncs(files)
	for _, pos := range stubDeclFindings(fset, files, root) {
		t.Errorf("%s: %s", fset.Position(pos), strayStubMsg)
	}
	for _, f := range files {
		for _, site := range unstubbedKeychainRefs(f, trusted, writers) {
			t.Errorf("%s: %s: %s", fset.Position(site.pos), site.name, unstubbedMsg)
		}
	}
}

// TestTheSharedIdentityGuardMessagesNameTheCauseAndFix pins the words every
// finding carries, so a refusal teaches the reader the cause and the fix.
func TestTheSharedIdentityGuardMessagesNameTheCauseAndFix(t *testing.T) {
	needs := []string{
		"Go 1.24",
		`"001"`,
		"filepath.Base(filepath.Dir(t.TempDir()))",
		"bind :0 or draw a port the way portOutsideTheEphemeralPool in adapter/adapter_test.go does",
		"files and sockets under t.TempDir()",
		"stubSecurity(t)",
		"-25299",
		"#830",
	}
	for _, msg := range []string{tempDirBaseMsg, fixedPortMsg, fixedPathMsg, unstubbedMsg, strayStubMsg} {
		for _, need := range needs {
			if !strings.Contains(msg, need) {
				t.Errorf("a guard message does not name %q: %s", need, msg)
			}
		}
	}
}

// TestTheSharedIdentityGuardSeesEveryOldShape pins every detector on the old
// shapes, the bypasses earlier cuts missed, and the compliant shapes, so a
// future edit cannot quietly widen what the guards accept. Lines a detector
// must report end in "// want", or "// base" / "// stub" / "// first-reach"
// where the same fixture feeds two detectors.
func TestTheSharedIdentityGuardSeesEveryOldShape(t *testing.T) {
	checkDetector(t, "old_test.go", oldMigrateFixture, "// base", tempDirBaseFindings)
	checkKeychain(t, "old_test.go", oldMigrateFixture, "// stub", true)

	checkDetector(t, "badbase_test.go", badTempBaseFixture, "// want", tempDirBaseFindings)
	checkDetector(t, "goodbase_test.go", goodTempBaseFixture, "// want", tempDirBaseFindings)

	checkDetector(t, "badport_test.go", badPortFixture, "// want", fixedPortFindings)
	checkDetector(t, "goodport_test.go", goodPortFixture, "// want", fixedPortFindings)

	checkDetector(t, "badpath_test.go", badPathFixture, "// want", fixedPathFindings)
	checkDetector(t, "goodpath_test.go", goodPathFixture, "// want", fixedPathFindings)

	checkKeychain(t, "badkeychain_test.go", badKeychainFixture, "// want", true)
	checkKeychain(t, "goodkeychain_test.go", goodKeychainFixture, "// want", true)
	checkKeychain(t, "goodkeychain_test.go", goodKeychainFixture, "// first-reach", false)

	const plainStub = `package p

import "testing"

func stubSecurity(t *testing.T) string { return "" }
`
	const methodStub = plainStub + `
func (h helper) stubSecurity(t *testing.T) string { return "" }
`
	checkStubDecls(t, "/r/probe_test.go", plainStub, 1, false)
	checkStubDecls(t, "/r/cli/x_test.go", plainStub+`var stubSecurity = func(t *testing.T) string { return "" }`+"\n", 2, false)
	checkStubDecls(t, "/r/util/keychain_test.go", plainStub, 0, true)
	checkStubDecls(t, "/r/util/keychain_test.go", methodStub, 0, true)
	checkStubDecls(t, "/r/util/keychain_test.go", `package p

var stubSecurity = func(t *testing.T) string { return "" }
`, 1, false)
}

// parseRepoTestFiles parses every *_test.go of the module, from the package
// directory the suite runs in (cwd for `go test ./pkg`), skipping hidden
// directories. It fails rather than reporting an empty scan, so a guard that
// stopped reading files cannot pass.
func parseRepoTestFiles(t *testing.T) (*token.FileSet, []*ast.File, string) {
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
	return fset, files, root
}

// --- the // want harness ---------------------------------------------------

func markedLines(src, marker string) []int {
	var out []int
	for i, line := range strings.Split(src, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), marker) {
			out = append(out, i+1)
		}
	}
	sort.Ints(out)
	return out
}

func uniqueLines(lines []int) []int {
	sort.Ints(lines)
	out := lines[:0]
	for i, l := range lines {
		if i == 0 || l != lines[i-1] {
			out = append(out, l)
		}
	}
	return out
}

func checkDetector(t *testing.T, name, src, marker string, detect func(*ast.File) []token.Pos) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var got []int
	for _, p := range detect(f) {
		got = append(got, fset.Position(p).Line)
	}
	got = uniqueLines(got)
	want := markedLines(src, marker)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: flagged lines %v, want %v (lines ending %q)", name, got, want, marker)
	}
}

func checkKeychain(t *testing.T, name, src, marker string, trusted bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var got []int
	for _, site := range unstubbedKeychainRefs(f, trusted, pathWriterFuncs([]*ast.File{f})) {
		got = append(got, fset.Position(site.pos).Line)
	}
	got = uniqueLines(got)
	want := markedLines(src, marker)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s (trusted=%v): flagged lines %v, want %v (lines ending %q)", name, trusted, got, want, marker)
	}
}

func checkStubDecls(t *testing.T, name, src string, wantFindings int, wantTrusted bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	found := len(stubDeclFindings(fset, []*ast.File{f}, "/r"))
	trusted := trustedStub(fset, []*ast.File{f}, "/r")
	if found != wantFindings || trusted != wantTrusted {
		t.Errorf("%s: %d stub declarations, trusted=%v; want %d, %v", name, found, trusted, wantFindings, wantTrusted)
	}
}

// --- constants -------------------------------------------------------------

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func constString(e ast.Expr) (string, bool) { return constStringDepth(e, 0) }

func constStringDepth(e ast.Expr, depth int) (string, bool) {
	if depth > 16 {
		return "", false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constStringDepth(x.X, depth+1)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		l, ok := constStringDepth(x.X, depth+1)
		if !ok {
			return "", false
		}
		r, ok := constStringDepth(x.Y, depth+1)
		if !ok {
			return "", false
		}
		return l + r, true
	case *ast.Ident:
		return constIdentString(x, depth+1)
	}
	return "", false
}

func constIdentString(id *ast.Ident, depth int) (string, bool) {
	if id.Obj == nil || id.Obj.Kind != ast.Con {
		return "", false
	}
	vs, ok := id.Obj.Decl.(*ast.ValueSpec)
	if !ok {
		return "", false
	}
	for i, n := range vs.Names {
		if n.Name == id.Name && i < len(vs.Values) {
			return constStringDepth(vs.Values[i], depth)
		}
	}
	return "", false
}

func constInt(e ast.Expr) (int64, bool) { return constIntDepth(e, 0) }

func constIntDepth(e ast.Expr, depth int) (int64, bool) {
	if depth > 16 {
		return 0, false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseInt(x.Value, 0, 64)
		return n, err == nil
	case *ast.ParenExpr:
		return constIntDepth(x.X, depth+1)
	case *ast.Ident:
		if x.Obj == nil || x.Obj.Kind != ast.Con {
			return 0, false
		}
		if vs, ok := x.Obj.Decl.(*ast.ValueSpec); ok {
			for i, n := range vs.Names {
				if n.Name == x.Name && i < len(vs.Values) {
					return constIntDepth(vs.Values[i], depth+1)
				}
			}
		}
	}
	return 0, false
}

func isType(e ast.Expr, pkg, name string) bool {
	sel, ok := unparen(e).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

func isOSFunc(e ast.Expr, name string) bool {
	call, ok := unparen(e).(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "os" && sel.Sel.Name == name
}

// --- temp-dir base ---------------------------------------------------------

// tempDirBaseFindings returns the position of every call of the shape
// <anything>.Base(x) where x is a t.TempDir() call (inline) or a variable the
// same file assigned from one (directly, through var, or through an alias).
// Variables are followed by *ast.Object, so a same-spelled dir bound to
// "/usr/bin" elsewhere, or shadowed in an inner block, is never flagged.
func tempDirBaseFindings(f *ast.File) []token.Pos {
	temps := map[*ast.Object]bool{}
	mark := func(lhs, rhs []ast.Expr) {
		for i, r := range rhs {
			if i >= len(lhs) {
				break
			}
			id, ok := lhs[i].(*ast.Ident)
			if !ok || id.Obj == nil {
				continue
			}
			if isTempDirCall(r) || isMarkedTemp(r, temps) {
				temps[id.Obj] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			mark(x.Lhs, x.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, len(x.Names))
			for i, nm := range x.Names {
				lhs[i] = nm
			}
			mark(lhs, x.Values)
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
		if isTempDirCall(call.Args[0]) || isMarkedTemp(call.Args[0], temps) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

func isMarkedTemp(e ast.Expr, temps map[*ast.Object]bool) bool {
	id, ok := unparen(e).(*ast.Ident)
	return ok && id.Obj != nil && temps[id.Obj]
}

func isTempDirCall(n ast.Expr) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) != 0 {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "TempDir"
}

// --- fixed ports -----------------------------------------------------------

// fixedPortFindings returns the position of every bind of a constant nonzero
// port: the number names one system namespace, and a literal has no per-run
// randomness, so the only port a test may pin is :0, and a number it needs
// must come from a draw in the 1024-32767 window (#176, #188).
func fixedPortFindings(f *ast.File) []token.Pos {
	var out []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			sel, ok := x.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			pkgName := ""
			if pkg != nil {
				pkgName = pkg.Name
			}
			switch {
			case pkgName == "net" && (sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket"):
				if len(x.Args) < 2 {
					return true
				}
				if netw, ok := constString(x.Args[0]); ok && isUnixNetwork(netw) {
					return true
				}
				if fixedPortAddr(x.Args[1]) {
					out = append(out, x.Pos())
				}
			case pkgName == "http" && (sel.Sel.Name == "ListenAndServe" || sel.Sel.Name == "ListenAndServeTLS"):
				if len(x.Args) >= 1 && fixedPortAddr(x.Args[0]) {
					out = append(out, x.Pos())
				}
			case pkgName != "net" && pkgName != "http" && (sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket") && len(x.Args) == 3:
				// a net.ListenConfig's Listen(ctx, network, address)
				if fixedPortAddr(x.Args[2]) {
					out = append(out, x.Pos())
				}
			case pkgName == "net" && (sel.Sel.Name == "ListenTCP" || sel.Sel.Name == "ListenUDP"):
				if len(x.Args) >= 2 && fixedAddrPort(x.Args[1]) {
					out = append(out, x.Pos())
				}
			}
		case *ast.CompositeLit:
			if !isType(x.Type, "http", "Server") {
				return true
			}
			for _, el := range x.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Addr" && fixedPortAddr(kv.Value) {
					out = append(out, x.Pos())
					break
				}
			}
		}
		return true
	})
	return out
}

func isUnixNetwork(network string) bool {
	switch network {
	case "unix", "unixgram", "unixpacket":
		return true
	}
	return false
}

// fixedPortAddr reports a constant address whose port is a fixed, nonzero
// number or a named service; a drawn port ("127.0.0.1:"+strconv.Itoa(p)) is
// not constant and stays allowed.
func fixedPortAddr(e ast.Expr) bool {
	e = unparen(e)
	if call, ok := e.(*ast.CallExpr); ok {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "JoinHostPort" || len(call.Args) != 2 {
			return false
		}
		p, ok := constString(call.Args[1])
		return ok && portIsFixed(p)
	}
	addr, ok := constString(e)
	if !ok {
		return false
	}
	_, port, err := netSplitHostPort(addr)
	if err != nil {
		return false
	}
	return portIsFixed(port)
}

func netSplitHostPort(addr string) (host, port string, err error) {
	// net.SplitHostPort without importing net into the guard's own build:
	// the guard must compile in a scratch module, so the two lines it needs
	// live here.
	i := strings.LastIndex(addr, ":")
	if i < 0 || strings.LastIndex(addr, "]") > i {
		return "", "", fmt.Errorf("missing port in address %s", addr)
	}
	return addr[:i], addr[i+1:], nil
}

func portIsFixed(port string) bool {
	if port == "" {
		return false
	}
	if n, err := strconv.Atoi(port); err == nil {
		return n != 0
	}
	return true // a named port such as http
}

// fixedAddrPort reports a net.TCPAddr/UDPAddr literal with a constant,
// nonzero Port.
func fixedAddrPort(e ast.Expr) bool {
	e = unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || (!isType(lit.Type, "net", "TCPAddr") && !isType(lit.Type, "net", "UDPAddr")) {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Port" {
			p, ok := constInt(kv.Value)
			return ok && p != 0
		}
	}
	return false
}

// --- fixed paths -----------------------------------------------------------

// fixedPathFindings returns the position of every creation of a file or socket
// at a constant path: the name is one system namespace, and concurrent runs on
// one machine collide in it.
func fixedPathFindings(f *ast.File) []token.Pos {
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
		pkg, _ := sel.X.(*ast.Ident)
		pkgName := ""
		if pkg != nil {
			pkgName = pkg.Name
		}
		arg := -1
		if pkgName == "os" || pkgName == "ioutil" {
			switch sel.Sel.Name {
			case "Create", "WriteFile", "Mkdir", "MkdirAll", "OpenFile":
				arg = 0
			case "Symlink", "Link", "Rename":
				arg = 1
			}
		}
		if pkgName == "net" {
			switch sel.Sel.Name {
			case "Listen", "ListenPacket":
				if len(call.Args) >= 2 {
					if netw, ok := constString(call.Args[0]); ok && isUnixNetwork(netw) {
						arg = 1
					}
				}
			case "ListenUnix", "ListenUnixgram":
				if len(call.Args) >= 2 && fixedUnixName(call.Args[1]) {
					out = append(out, call.Pos())
				}
			}
		}
		if arg >= 0 && arg < len(call.Args) && fixedPath(call.Args[arg]) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// fixedPath reports a constant path, a Join of constants and os.TempDir(), or
// os.TempDir()+constant. Paths under t.TempDir() or os.MkdirTemp/CreateTemp,
// /dev/null and testdata/ are the compliant answers and are never reported.
func fixedPath(e ast.Expr) bool {
	e = unparen(e)
	if call, ok := e.(*ast.CallExpr); ok {
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, _ := sel.X.(*ast.Ident)
		pkgName := ""
		if pkg != nil {
			pkgName = pkg.Name
		}
		if pkgName == "os" && sel.Sel.Name == "TempDir" && len(call.Args) == 0 {
			return true
		}
		if (pkgName == "filepath" || pkgName == "path") && sel.Sel.Name == "Join" && len(call.Args) > 0 {
			if first, ok := constString(call.Args[0]); ok && (first == "testdata" || strings.HasPrefix(first, "testdata/")) {
				return false
			}
			for _, a := range call.Args {
				if _, ok := constString(a); ok {
					continue
				}
				if isOSFunc(a, "TempDir") {
					continue
				}
				return false
			}
			return true
		}
		return false
	}
	if bin, ok := e.(*ast.BinaryExpr); ok && bin.Op == token.ADD && isOSFunc(bin.X, "TempDir") {
		_, ok := constString(bin.Y)
		return ok
	}
	s, ok := constString(e)
	if !ok {
		return false
	}
	return s != "/dev/null" && s != "testdata" && !strings.HasPrefix(s, "testdata/")
}

func fixedUnixName(e ast.Expr) bool {
	e = unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || !isType(lit.Type, "net", "UnixAddr") {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Name" {
			s, ok := constString(kv.Value)
			return ok && s != ""
		}
	}
	return false
}

// --- the keychain reach ----------------------------------------------------

type keychainRefSite struct {
	pos  token.Pos
	name string
}

// unstubbedKeychainRefs returns every function or package-level declaration of
// the file that reaches the keychain code — an identifier starting with
// Keychain (KeychainService excepted), keychainPut, keychainMove, or an exec
// of security — without a stubSecurity(t) statement in an enclosing body
// before the reach, with a trusted stub declared, and with no PATH write
// anywhere in the declaration. An absolute path to security is never covered:
// no PATH stub can intercept it.
func unstubbedKeychainRefs(f *ast.File, trusted bool, pathWriters map[string]bool) []keychainRefSite {
	var out []keychainRefSite
	for _, decl := range f.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			if x.Body == nil {
				continue
			}
			if pos, ok := firstUncoveredReach(x.Body, trusted, pathWriters); ok {
				out = append(out, keychainRefSite{pos: pos, name: x.Name.Name})
			}
		case *ast.GenDecl:
			if x.Tok != token.VAR && x.Tok != token.CONST {
				continue
			}
			for _, sp := range x.Specs {
				vs, ok := sp.(*ast.ValueSpec)
				if !ok {
					continue
				}
				var pos token.Pos
				for _, v := range vs.Values {
					ast.Inspect(v, func(n ast.Node) bool {
						if pos == token.NoPos {
							if p, _, ok := keychainReach(n); ok {
								pos = p
							}
						}
						return pos == token.NoPos
					})
				}
				if pos != token.NoPos {
					out = append(out, keychainRefSite{pos: pos, name: "a package-level declaration"})
				}
			}
		}
	}
	return out
}

func firstUncoveredReach(body *ast.BlockStmt, trusted bool, pathWriters map[string]bool) (token.Pos, bool) {
	var bodies []*ast.BlockStmt
	ast.Inspect(body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			bodies = append(bodies, fl.Body)
		}
		return true
	})
	bodies = append(bodies, body)
	pathWrite := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && pathWriteCall(call, pathWriters) {
			pathWrite = true
		}
		return true
	})
	var first token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		if p, absolute, ok := keychainReach(n); ok && first == token.NoPos {
			if absolute || !trusted || pathWrite || !stubCovers(bodies, p) {
				first = p
			}
		}
		return true
	})
	return first, first != token.NoPos
}

func stubCovers(bodies []*ast.BlockStmt, p token.Pos) bool {
	for _, b := range bodies {
		if p < b.Pos() || p >= b.End() {
			continue
		}
		for _, s := range b.List {
			if s.Pos() >= p {
				break
			}
			if isStubStatement(s) {
				return true
			}
		}
	}
	return false
}

func isStubStatement(s ast.Stmt) bool {
	var e ast.Expr
	switch x := s.(type) {
	case *ast.ExprStmt:
		e = x.X
	case *ast.AssignStmt:
		if len(x.Rhs) == 1 {
			e = x.Rhs[0]
		}
	case *ast.DeclStmt:
		gd, ok := x.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
			return false
		}
		vs, ok := gd.Specs[0].(*ast.ValueSpec)
		if !ok || len(vs.Values) != 1 {
			return false
		}
		e = vs.Values[0]
	}
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok || id.Name != "stubSecurity" {
		return false
	}
	return id.Obj == nil || id.Obj.Kind == ast.Fun
}

func keychainReach(n ast.Node) (token.Pos, bool, bool) {
	switch x := n.(type) {
	case *ast.Ident:
		if strings.HasPrefix(x.Name, "Keychain") && x.Name != "KeychainService" {
			return x.Pos(), false, true
		}
		if x.Name == "keychainPut" || x.Name == "keychainMove" {
			return x.Pos(), false, true
		}
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return 0, false, false
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != "exec" {
			return 0, false, false
		}
		arg := -1
		switch sel.Sel.Name {
		case "Command":
			arg = 0
		case "CommandContext":
			arg = 1
		default:
			return 0, false, false
		}
		if len(x.Args) <= arg {
			return 0, false, false
		}
		name, ok := constString(x.Args[arg])
		if !ok {
			return 0, false, false
		}
		if name == "security" {
			return x.Pos(), false, true
		}
		if strings.Contains(name, "/") && path.Base(name) == "security" {
			return x.Pos(), true, true
		}
	}
	return 0, false, false
}

func pathWriteCall(call *ast.CallExpr, pathWriters map[string]bool) bool {
	if id, ok := call.Fun.(*ast.Ident); ok && pathWriters[id.Name] {
		return true
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Clearenv":
		return true
	case "Setenv", "Unsetenv":
		if len(call.Args) == 0 {
			return false
		}
		k, ok := constString(call.Args[0])
		return ok && k == "PATH"
	}
	return false
}

// pathWriterFuncs names the plain same-module helpers that write PATH, so a
// function that calls one after its stub is refused; one level is followed.
func pathWriterFuncs(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || fn.Name.Name == "stubSecurity" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && pathWriteCall(call, nil) {
					out[fn.Name.Name] = true
				}
				return true
			})
		}
	}
	return out
}

// stubDeclFindings reports every declaration of stubSecurity except the one
// plain func in util/keychain_test.go. A method is no declaration of the name
// (an unqualified stubSecurity(t) cannot call it), so it is not reported; it
// grants no trust either.
func stubDeclFindings(fset *token.FileSet, files []*ast.File, root string) []token.Pos {
	var out []token.Pos
	for _, f := range files {
		rel := relTo(root, fset.Position(f.Pos()).Filename)
		for _, d := range f.Decls {
			switch x := d.(type) {
			case *ast.FuncDecl:
				if x.Name.Name == "stubSecurity" && x.Recv == nil && rel != trustedStubFile {
					out = append(out, x.Pos())
				}
			case *ast.GenDecl:
				for _, sp := range x.Specs {
					vs, ok := sp.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, n := range vs.Names {
						if n.Name == "stubSecurity" {
							out = append(out, n.Pos())
						}
					}
				}
			}
		}
	}
	return out
}

// trustedStub reports whether util/keychain_test.go declares the plain func
// the guard accepts as the one security stub.
func trustedStub(fset *token.FileSet, files []*ast.File, root string) bool {
	for _, f := range files {
		if relTo(root, fset.Position(f.Pos()).Filename) != trustedStubFile {
			continue
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "stubSecurity" && fn.Body != nil {
				return true
			}
		}
	}
	return false
}

func relTo(root, name string) string {
	rel, err := filepath.Rel(root, name)
	if err != nil {
		rel = name
	}
	return filepath.ToSlash(rel)
}

// --- fixtures --------------------------------------------------------------

// oldMigrateFixture is the migrate test as it stood on main when #830 was
// filed (4f02ed0:util/keychain_test.go).
const oldMigrateFixture = `package p

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/akleizor/ccmorph/profile"
)

func TestKeychainMigrateMovesThePlainKeyAndReadsItBack(t *testing.T) {
	if runtime.GOOS != "darwin" || exec.Command("security", "default-keychain").Run() != nil { // stub
		t.Skip("no login keychain")
	}
	dir := t.TempDir()
	name := "ccmtest" + strings.ReplaceAll(filepath.Base(dir), "-", "") // base
	t.Cleanup(func() { KeychainDelete(name) })
	conf := filepath.Join(dir, name+".conf")
	secret := ` + "`sk-\"quoted\"\\back`" + `
	os.WriteFile(conf, []byte("type=apikey\napi_key="+secret+"\n"), 0o600)
	if err := KeychainMigrate(dir); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(conf)
	if !strings.Contains(string(data), "api_key=keychain:"+name+".api_key") || strings.Contains(string(data), "sk-") {
		t.Fatal(string(data))
	}
	if got, err := profile.KeychainGet("keychain:" + name + ".api_key"); err != nil || got != secret {
		t.Fatalf("got %q %v", got, err)
	}
}
`

const badTempBaseFixture = `package p

import (
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func Inline(t *testing.T) {
	_ = filepath.Base(t.TempDir()) // want
}

func ThroughAssign(t *testing.T) {
	dir := t.TempDir()
	name := "ccmtest" + strings.ReplaceAll(filepath.Base(dir), "-", "") // want
	_ = name
}

func ThroughVar(t *testing.T) {
	var d = t.TempDir()
	_ = path.Base(d) // want
}

func ThroughAlias(t *testing.T) {
	dir := t.TempDir()
	alias := dir
	_ = filepath.Base(alias) // want
}

func ThroughLaterAssign(t *testing.T) {
	var dir string
	dir = t.TempDir()
	_ = filepath.Base(dir) // want
}

func InSubtest(t *testing.T) {
	t.Run("x", func(t *testing.T) {
		_ = filepath.Base(t.TempDir()) // want
	})
}

func Bench(b *testing.B) {
	_ = filepath.Base(b.TempDir()) // want
}
`

const goodTempBaseFixture = `package p

import (
	"path/filepath"
	"testing"
)

func ParentOfTemp(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Base(filepath.Dir(dir))
	_ = filepath.Base(filepath.Dir(t.TempDir()))
	_ = filepath.Base(filepath.Join(dir, "x.conf"))
}

func Ordinary(t *testing.T) {
	dir := "/usr/bin"
	_ = filepath.Base(dir)
}

func ShadowInClosure(t *testing.T) {
	dir := t.TempDir()
	t.Run("a", func(t *testing.T) { d := t.TempDir(); _ = d })
	t.Run("b", func(t *testing.T) { d := "/usr/bin"; _ = filepath.Base(d) })
	_ = dir
}

func ShadowInBlock(t *testing.T) {
	dir := t.TempDir()
	{
		dir := "/usr/bin"
		_ = filepath.Base(dir)
	}
	_ = dir
}
`

const badPortFixture = `package p

import (
	"context"
	"net"
	"net/http"
)

const packageAddr = "127.0.0.1:7000"

func LiteralLoopback(t *testing.T) {
	net.Listen("tcp", "127.0.0.1:49173") // want
}

func LiteralLowest(t *testing.T) {
	net.Listen("tcp", ":1024") // want
}

func LiteralHighest(t *testing.T) {
	net.Listen("tcp", ":32767") // want
}

func LiteralOne(t *testing.T) {
	net.Listen("tcp", ":1") // want
}

func LiteralV6(t *testing.T) {
	net.Listen("tcp", "[::1]:4000") // want
}

func NamedPort(t *testing.T) {
	net.Listen("tcp", ":http") // want
}

func PacketPort(t *testing.T) {
	net.ListenPacket("udp", ":5353") // want
}

func HTTP(t *testing.T) {
	http.ListenAndServe(":8080", nil) // want
}

func HTTPS(t *testing.T) {
	http.ListenAndServeTLS(":8443", "cert", "key", nil) // want
}

func ListenConfig(t *testing.T) {
	lc := net.ListenConfig{}
	lc.Listen(context.Background(), "tcp", "127.0.0.1:9000") // want
}

func PackageConst(t *testing.T) {
	net.Listen("tcp", packageAddr) // want
}

func LocalConst(t *testing.T) {
	const addr = ":7001"
	net.Listen("tcp", addr) // want
}

func Concatenated(t *testing.T) {
	net.Listen("tcp", "127.0.0.1:"+"6000") // want
}

func Joined(t *testing.T) {
	net.Listen("tcp", net.JoinHostPort("127.0.0.1", "5000")) // want
}

func TCPAddrLiteral(t *testing.T) {
	net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4001}) // want
}

func UDPAddrLiteral(t *testing.T) {
	net.ListenUDP("udp", &net.UDPAddr{Port: 4002}) // want
}

func ServerAddr(t *testing.T) {
	_ = http.Server{Addr: "127.0.0.1:4003"} // want
}
`

const goodPortFixture = `package p

import (
	"context"
	"net"
	"net/http"
	"strconv"
)

func ZeroLoopback(t *testing.T) {
	net.Listen("tcp", "127.0.0.1:0")
}

func ZeroOnly(t *testing.T) {
	net.Listen("tcp", ":0")
}

func ZeroV6(t *testing.T) {
	net.Listen("tcp", "[::1]:0")
}

func ZeroPacket(t *testing.T) {
	net.ListenPacket("udp", ":0")
}

func ZeroHTTP(t *testing.T) {
	http.ListenAndServe(":0", nil)
}

func ZeroTLS(t *testing.T) {
	http.ListenAndServeTLS(":0", "cert", "key", nil)
}

func ZeroConfig(t *testing.T) {
	lc := net.ListenConfig{}
	lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
}

func Drawn(t *testing.T) {
	p := 1234
	net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
	net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
}

func ZeroTCPAddr(t *testing.T) {
	net.ListenTCP("tcp", &net.TCPAddr{Port: 0})
	net.ListenTCP("tcp", &net.TCPAddr{})
}

func UnixBind(t *testing.T) {
	net.Listen("unix", "/tmp/x.sock")
}

func Dial(t *testing.T) {
	net.Dial("tcp", "127.0.0.1:1")
}
`

const badPathFixture = `package p

import (
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
)

const lockPath = "/var/tmp/ccmorph.lock"

func WriteFixed(t *testing.T) {
	os.WriteFile("/tmp/ccmorph-fixed", nil, 0o600) // want
}

func MkdirAllFixed(t *testing.T) {
	os.MkdirAll("/var/tmp/ccmtest", 0o700) // want
}

func MkdirPrivate(t *testing.T) {
	os.Mkdir("/private/tmp/ccmtest", 0o700) // want
}

func CreateConst(t *testing.T) {
	os.Create(lockPath) // want
}

func CreateConcat(t *testing.T) {
	os.Create("/tmp/" + "ccm") // want
}

func CreateJoinTemp(t *testing.T) {
	os.Create(filepath.Join(os.TempDir(), "ccmorph.lock")) // want
}

func OpenPathJoin(t *testing.T) {
	os.OpenFile(path.Join(os.TempDir(), "a", "b"), os.O_RDONLY, 0) // want
}

func CreateTempPlus(t *testing.T) {
	os.Create(os.TempDir() + "/ccm") // want
}

func WriteRelative(t *testing.T) {
	os.WriteFile("relative.txt", nil, 0o600) // want
}

func WriteLegacy(t *testing.T) {
	ioutil.WriteFile("/tmp/legacy", nil, 0o600) // want
}

func SymlinkFixed(t *testing.T) {
	os.Symlink("target", "/tmp/ccm-link") // want
}

func LinkFixed(t *testing.T) {
	os.Link("src", "/tmp/ccm-hard") // want
}

func RenameFixed(t *testing.T) {
	os.Rename("src", "/tmp/ccm-dst") // want
}

func SocketFixed(t *testing.T) {
	net.Listen("unix", "/tmp/ccm.sock") // want
}

func SocketAbstract(t *testing.T) {
	net.Listen("unix", "@ccmorph") // want
}

func DatagramFixed(t *testing.T) {
	net.ListenPacket("unixgram", "/tmp/ccm.dgram") // want
}

func ListenUnixFixed(t *testing.T) {
	net.ListenUnix("unix", &net.UnixAddr{Name: "/tmp/ccm2.sock", Net: "unix"}) // want
}
`

const goodPathFixture = `package p

import (
	"net"
	"os"
	"path/filepath"
)

func UnderTemp(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x"), nil, 0o600)
}

func MkdirTempDraw(t *testing.T) {
	os.MkdirTemp("", "ccm*")
}

func CreateTempDraw(t *testing.T) {
	os.Create` + `Temp(os.TempDir(), "ccm*")
}

func DevNull(t *testing.T) {
	os.OpenFile("/dev/null", os.O_WRONLY, 0)
	os.OpenFile(os.DevNull, os.O_WRONLY, 0)
}

func Testdata(t *testing.T) {
	os.WriteFile("testdata/x.golden", nil, 0o600)
}

func GlobRead(t *testing.T) {
	filepath.Glob(filepath.Join(os.TempDir(), "ccmorph-session-end.*"))
	os.ReadFile("/etc/hosts")
}

func SymlinkSource(t *testing.T) {
	dir := t.TempDir()
	os.Symlink("/tmp/target", filepath.Join(dir, "l"))
	os.Rename("/tmp/src", filepath.Join(dir, "d"))
}

func DrawnName(name string) {
	os.Create(filepath.Join(os.TempDir(), name))
}

func UnixBindTemp(t *testing.T) {
	net.Listen("unix", filepath.Join(t.TempDir(), "s"))
}
`

const badKeychainFixture = `package p

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
)

const tool = "security"

func TestNoOpPath(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
	_ = exec.Command("security", "default-keychain").Run() // want
}

func TestJoinOnly(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Join(dir, "security")
	_ = exec.Command("security", "default-keychain").Run() // want
}

func TestHandStub(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "security"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	KeychainDelete("x") // want
}

func TestStubAfter(t *testing.T) {
	KeychainDelete("x") // want
	stubSecurity(t)
}

func TestStubInIf(t *testing.T) {
	if true {
		stubSecurity(t)
	}
	KeychainDelete("x") // want
}

func TestStubDeferred(t *testing.T) {
	defer stubSecurity(t)
	KeychainDelete("x") // want
}

func TestPathWrittenAfter(t *testing.T) {
	stubSecurity(t)
	t.Setenv("PATH", "/usr/bin")
	KeychainDelete("x") // want
}

func TestPathUnsetAfter(t *testing.T) {
	stubSecurity(t)
	_ = os.Unsetenv("PATH")
	KeychainDelete("x") // want
}

func TestClearenvAfter(t *testing.T) {
	stubSecurity(t)
	_ = os.Clearenv()
	KeychainDelete("x") // want
}

func TestHelperRestoresAfter(t *testing.T) {
	stubSecurity(t)
	restorePath(t)
	KeychainDelete("x") // want
}

func TestAbsoluteSecurity(t *testing.T) {
	stubSecurity(t)
	_ = exec.Command("/usr/bin/security", "default-keychain").Run() // want
}

func TestCommandContext(t *testing.T) {
	_ = exec.CommandContext(context.Background(), "security", "default-keychain").Run() // want
	stubSecurity(t)
}

func TestConstName(t *testing.T) {
	_ = exec.Command(tool, "default-keychain").Run() // want
	stubSecurity(t)
}

func TestMethodCall(t *testing.T) {
	h := helper{}
	h.stubSecurity(t)
	KeychainDelete("x") // want
}

func TestLocalStub(t *testing.T) {
	stubSecurity := func(*testing.T) string { return "" }
	stubSecurity(t)
	KeychainDelete("x") // want
}

func TestFuncValueBeforeStub(t *testing.T) {
	del := KeychainDelete // want
	stubSecurity(t)
	del("x")
}

func TestSiblingClosure(t *testing.T) {
	t.Run("a", func(t *testing.T) { stubSecurity(t) })
	t.Run("b", func(t *testing.T) { KeychainDelete("x") }) // want
}

var helperValue = func() { KeychainDelete("x") } // want

func restorePath(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
}
`

const goodKeychainFixture = `package p

import (
	"os"
	"os/exec"

	"github.com/akleizor/ccmorph/profile"
)

func TestStubFirst(t *testing.T) {
	stubSecurity(t)
	KeychainDelete("x") // first-reach
}

func TestStubThroughSkip(t *testing.T) {
	if true {
		t.Skip("darwin only")
	}
	state := stubSecurity(t)
	_ = state
	_ = KeychainGet("keychain:x.api_key") // first-reach
}

func TestClosureAfterTopStub(t *testing.T) {
	stubSecurity(t)
	t.Run("a", func(t *testing.T) { KeychainDelete("x") }) // first-reach
}

func TestClosureOwnStub(t *testing.T) {
	t.Run("a", func(t *testing.T) {
		stubSecurity(t)
		KeychainDelete("x") // first-reach
	})
}

func TestOnlySetsPath(t *testing.T) {
	t.Setenv("PATH", os.Getenv("PATH"))
}

func TestOnlyReadsService(t *testing.T) {
	_ = profile.KeychainService
}

func TestOtherTool(t *testing.T) {
	stubSecurity(t)
	_ = exec.Command("true").Run()
}

var _ = os.Getenv
`
