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
	"unicode"
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

// trustedDrawerFile and trustedDrawerFunc name the one helper allowed to bind
// a port it computed: the fixed-port rule is an allowlist, because a denylist
// of constants can always be evaded by one more spelling. A bind passes only
// when the guard folds it to port 0, or when it sits inside that helper. The
// helper draws below every ephemeral range (#176, #188), and
// TestPortOutsideTheEphemeralPoolDrawsInTheWindow pins it at runtime.
const (
	trustedDrawerFile = "adapter/adapter_test.go"
	trustedDrawerFunc = "portOutsideTheEphemeralPool"
)

// identityFix is the half of every message that names the cause and the fix,
// so a reader of any finding learns both without opening this file.
const identityFix = " Since Go 1.24 the base of a t.TempDir() path is the constant \"001\" — the per-run randomness is its parent — so whatever is built from it is the same identity for every run, checkout and worktree, and concurrent runs collide in it: the keychain answered add-generic-password: returned -25299, \"The specified item already exists\". Fix: derive the identity from the random parent (filepath.Base(filepath.Dir(t.TempDir()))); keep files and sockets under t.TempDir(); bind :0 or draw a port the way portOutsideTheEphemeralPool in adapter/adapter_test.go does; reach the keychain only through stubSecurity(t) in util/keychain_test.go (#830)."

const (
	tempDirBaseMsg = "this derives a value from the base of a t.TempDir() path." + identityFix
	fixedPortMsg   = "this binds a constant port: one number per machine, so a concurrent run or any parallel gate worker holding it fails this test." + identityFix
	unseenPortMsg  = "this binds a port the guard cannot see is 0: bind a constant :0 (for a server, net.Listen(\"tcp\", \"127.0.0.1:0\") and Serve(l)), or take the number from portOutsideTheEphemeralPool, the one helper that may bind a port it computed." + identityFix
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
	fset, files, root := parseRepoTestFiles(t)
	for _, f := range files {
		drawerTrusted := relTo(root, fset.Position(f.Pos()).Filename) == trustedDrawerFile
		for _, finding := range fixedPortFindings(f, drawerTrusted) {
			t.Errorf("%s: %s", fset.Position(finding.pos), finding.msg)
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
	for _, msg := range []string{tempDirBaseMsg, fixedPortMsg, unseenPortMsg, fixedPathMsg, unstubbedMsg, strayStubMsg} {
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

	checkDetector(t, "badport_test.go", badPortFixture, "// want", portPositions(false))
	checkDetector(t, "goodport_test.go", goodPortFixture, "// want", portPositions(false))
	// The drawer is trusted only in its own file and only as that plain func:
	// trusted, its computed bind passes and its constant bind is still refused;
	// elsewhere every computed bind is unseen.
	checkDetectorMarks(t, trustedDrawerFile, drawerFixture, []string{"// want"}, portPositions(true))
	checkDetectorMarks(t, trustedDrawerFile, drawerFixture, []string{"// want", "// untrusted"}, portPositions(false))

	for _, c := range []struct {
		port int64
		note bool
	}{{1, true}, {80, true}, {1023, true}, {1024, false}, {4000, false}, {32767, false}, {32768, true}, {49152, true}, {49173, true}, {65535, true}} {
		if got := fixedPortNote(c.port) != ""; got != c.note {
			t.Errorf("fixedPortNote(%d) non-empty = %v, want %v", c.port, got, c.note)
		}
	}
	checkPortMsg(t, `package p
import "net"
func A(t *testing.T) { net.Listen("tcp", "127.0.0.1:4000") }
func B(t *testing.T) { net.Listen("tcp", "127.0.0.1:49173") }
func C(t *testing.T, addr string) { net.Listen("tcp", addr) }
`, `127.0.0.1:4000`, fixedPortMsg)
	checkPortMsg(t, `package p
import "net"
func A(t *testing.T) { net.Listen("tcp", "127.0.0.1:4000") }
func B(t *testing.T) { net.Listen("tcp", "127.0.0.1:49173") }
func C(t *testing.T, addr string) { net.Listen("tcp", addr) }
`, `127.0.0.1:49173`, fixedPortMsg+fixedPortNote(49173))
	checkPortMsg(t, `package p
import "net"
func A(t *testing.T) { net.Listen("tcp", "127.0.0.1:4000") }
func B(t *testing.T) { net.Listen("tcp", "127.0.0.1:49173") }
func C(t *testing.T, addr string) { net.Listen("tcp", addr) }
`, `net.Listen("tcp", addr)`, unseenPortMsg)

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

// TestTheSharedIdentityGuardFoldsOnlyConstants pins the one folder every
// detector uses: a constant hidden behind a local variable, an alias,
// strconv.Itoa, fmt.Sprintf, filepath.Join/Clean or t.Name() folds; a
// reassignment to a different value, a +=, an address, a range variable, a
// multi-value assignment, a parameter, another package's selector or a call
// the folder does not know does not. Lines end in "// = <literal>",
// "// const" or "// not".
func TestTheSharedIdentityGuardFoldsOnlyConstants(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "consts_test.go", constsFixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := newGuardConsts(f)
	byLine := map[int]*ast.AssignStmt{}
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name == "_" {
			byLine[fset.Position(as.Pos()).Line] = as
		}
		return true
	})
	checked := 0
	for i, line := range strings.Split(constsFixture, "\n") {
		idx := strings.LastIndex(line, "//")
		if idx < 0 {
			continue
		}
		mark := strings.TrimSpace(line[idx+2:])
		if !strings.HasPrefix(mark, "= ") && mark != "const" && mark != "not" {
			continue
		}
		as, ok := byLine[i+1]
		if !ok {
			t.Errorf("line %d: no `_ = expr` statement: %s", i+1, line)
			continue
		}
		got, isConst := c.value(as.Rhs[0], 0)
		switch {
		case strings.HasPrefix(mark, "= "):
			lit := strings.TrimSpace(mark[2:])
			var want any
			if strings.HasPrefix(lit, `"`) {
				s, err := strconv.Unquote(lit)
				if err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				want = s
			} else {
				n, err := strconv.ParseInt(lit, 0, 64)
				if err != nil {
					t.Fatalf("line %d: %v", i+1, err)
				}
				want = n
			}
			if !isConst || !sameConst(got, want) {
				t.Errorf("line %d %q: got %v (constant=%v), want %v", i+1, strings.TrimSpace(line), got, isConst, want)
			}
		case mark == "const":
			if !isConst {
				t.Errorf("line %d %q: did not fold", i+1, strings.TrimSpace(line))
			}
		case mark == "not":
			if isConst {
				t.Errorf("line %d %q: folded to %v; want not constant", i+1, strings.TrimSpace(line), got)
			}
		}
		checked++
	}
	if checked < 25 {
		t.Errorf("only %d folder cases checked; the fixture lost its pins", checked)
	}
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
	checkDetectorMarks(t, name, src, []string{marker}, detect)
}

// checkDetectorMarks is checkDetector with several markers: the flagged lines
// must be exactly the union of the lines carrying any of them.
func checkDetectorMarks(t *testing.T, name, src string, markers []string, detect func(*ast.File) []token.Pos) {
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
	var want []int
	for _, marker := range markers {
		want = append(want, markedLines(src, marker)...)
	}
	want = uniqueLines(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: flagged lines %v, want %v (lines ending %v)", name, got, want, markers)
	}
}

// portPositions adapts fixedPortFindings to the detector harness.
func portPositions(drawerTrusted bool) func(*ast.File) []token.Pos {
	return func(f *ast.File) []token.Pos {
		var out []token.Pos
		for _, finding := range fixedPortFindings(f, drawerTrusted) {
			out = append(out, finding.pos)
		}
		return out
	}
}

// checkPortMsg requires the finding at the line containing needle to carry
// exactly want: the message, not just the position, is the contract.
func checkPortMsg(t *testing.T, src, needle, want string) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "portmsg_test.go", src, 0)
	if err != nil {
		t.Fatalf("portmsg_test.go: %v", err)
	}
	for _, finding := range fixedPortFindings(f, false) {
		line := fset.Position(finding.pos).Line
		text := strings.Split(src, "\n")[line-1]
		if strings.Contains(text, needle) {
			if finding.msg != want {
				t.Errorf("%q: message %q, want %q", needle, finding.msg, want)
			}
			return
		}
	}
	t.Errorf("no port finding on the line containing %q", needle)
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

// guardConsts folds one file's constants before any detector judges an
// expression, so a constant hidden behind a local variable, strconv.Itoa,
// fmt.Sprintf or filepath.Join is still the constant it is. A var folds only
// when every assignment to it (in any function or closure of the file) folds
// to the same value; a reassignment to another value, a +=, an address, a
// range variable or a multi-value assignment makes it unknown, never a guess.
// os.TempDir() folds to this machine's path and t.Name() to the constant
// "t.Name()": both are the same on every run.
type constMemo struct {
	val   any
	ok    bool
	inUse bool
}

type guardConsts struct {
	assigns map[*ast.Object][]ast.Expr
	opaque  map[*ast.Object]bool
	memo    map[*ast.Object]constMemo
}

func newGuardConsts(f *ast.File) *guardConsts {
	c := &guardConsts{
		assigns: map[*ast.Object][]ast.Expr{},
		opaque:  map[*ast.Object]bool{},
		memo:    map[*ast.Object]constMemo{},
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if x.Tok == token.DEFINE || x.Tok == token.ASSIGN {
				if len(x.Lhs) == len(x.Rhs) {
					for i, l := range x.Lhs {
						id, ok := l.(*ast.Ident)
						if !ok || id.Name == "_" || id.Obj == nil {
							continue
						}
						c.assigns[id.Obj] = append(c.assigns[id.Obj], x.Rhs[i])
					}
					return true
				}
			}
			for _, l := range x.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Obj != nil {
					c.opaque[id.Obj] = true
				}
			}
		case *ast.ValueSpec:
			for i, n := range x.Names {
				if n.Obj == nil || n.Name == "_" {
					continue
				}
				if i < len(x.Values) {
					c.assigns[n.Obj] = append(c.assigns[n.Obj], x.Values[i])
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{x.Key, x.Value} {
				if id, ok := e.(*ast.Ident); ok && id.Obj != nil {
					c.opaque[id.Obj] = true
				}
			}
		case *ast.IncDecStmt:
			if id, ok := unparen(x.X).(*ast.Ident); ok && id.Obj != nil {
				c.opaque[id.Obj] = true
			}
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				if id, ok := unparen(x.X).(*ast.Ident); ok && id.Obj != nil {
					c.opaque[id.Obj] = true
				}
			}
		}
		return true
	})
	return c
}

func (c *guardConsts) value(e ast.Expr, depth int) (any, bool) {
	if depth > 16 || e == nil {
		return nil, false
	}
	switch x := unparen(e).(type) {
	case *ast.BasicLit:
		switch x.Kind {
		case token.STRING:
			s, err := strconv.Unquote(x.Value)
			return s, err == nil
		case token.INT:
			n, err := strconv.ParseInt(x.Value, 0, 64)
			return n, err == nil
		}
	case *ast.BinaryExpr:
		l, ok := c.value(x.X, depth+1)
		if !ok {
			return nil, false
		}
		r, ok := c.value(x.Y, depth+1)
		if !ok {
			return nil, false
		}
		switch x.Op {
		case token.ADD:
			switch lv := l.(type) {
			case string:
				if rv, ok := r.(string); ok {
					return lv + rv, true
				}
			case int64:
				if rv, ok := r.(int64); ok {
					return lv + rv, true
				}
			}
		case token.SUB:
			if lv, ok := l.(int64); ok {
				if rv, ok := r.(int64); ok {
					return lv - rv, true
				}
			}
		case token.MUL:
			if lv, ok := l.(int64); ok {
				if rv, ok := r.(int64); ok {
					return lv * rv, true
				}
			}
		}
	case *ast.Ident:
		return c.identValue(x, depth)
	case *ast.CallExpr:
		return c.callValue(x, depth)
	}
	return nil, false
}

func (c *guardConsts) identValue(id *ast.Ident, depth int) (any, bool) {
	if id.Obj == nil {
		return nil, false
	}
	switch id.Obj.Kind {
	case ast.Con:
		vs, ok := id.Obj.Decl.(*ast.ValueSpec)
		if !ok {
			return nil, false
		}
		for i, n := range vs.Names {
			if n.Name == id.Name && i < len(vs.Values) {
				return c.value(vs.Values[i], depth+1)
			}
		}
		return nil, false
	case ast.Var:
		if c.opaque[id.Obj] {
			return nil, false
		}
		if m, seen := c.memo[id.Obj]; seen {
			if m.inUse {
				return nil, false
			}
			return m.val, m.ok
		}
		exprs, ok := c.assigns[id.Obj]
		if !ok || len(exprs) == 0 {
			return nil, false
		}
		c.memo[id.Obj] = constMemo{inUse: true}
		var val any
		for i, ex := range exprs {
			v, ok := c.value(ex, depth+1)
			if !ok {
				c.memo[id.Obj] = constMemo{}
				return nil, false
			}
			if i > 0 && !sameConst(val, v) {
				c.memo[id.Obj] = constMemo{}
				return nil, false
			}
			val = v
		}
		c.memo[id.Obj] = constMemo{val: val, ok: true}
		return val, true
	}
	return nil, false
}

func sameConst(a, b any) bool {
	switch av := a.(type) {
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	}
	return false
}

func (c *guardConsts) callValue(call *ast.CallExpr, depth int) (any, bool) {
	if call.Ellipsis != token.NoPos {
		return nil, false
	}
	sel, ok := unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil, false
	}
	if id, ok := unparen(sel.X).(*ast.Ident); ok && id.Obj != nil && sel.Sel.Name == "Name" && len(call.Args) == 0 && isTestingParam(id) {
		return "t.Name()", true
	}
	pkg, ok := unparen(sel.X).(*ast.Ident)
	if !ok || pkg.Obj != nil {
		return nil, false
	}
	switch pkg.Name {
	case "filepath", "path":
		switch sel.Sel.Name {
		case "Join":
			if len(call.Args) == 0 {
				return "", true
			}
			parts := make([]string, len(call.Args))
			for i, a := range call.Args {
				s, ok := c.str(a, depth+1)
				if !ok {
					return nil, false
				}
				parts[i] = s
			}
			if pkg.Name == "path" {
				return path.Join(parts...), true
			}
			return filepath.Join(parts...), true
		case "Clean":
			if len(call.Args) != 1 {
				return nil, false
			}
			s, ok := c.str(call.Args[0], depth+1)
			if !ok {
				return nil, false
			}
			if pkg.Name == "path" {
				return path.Clean(s), true
			}
			return filepath.Clean(s), true
		}
	case "fmt":
		switch sel.Sel.Name {
		case "Sprintf":
			if len(call.Args) == 0 {
				return nil, false
			}
			format, ok := c.str(call.Args[0], depth+1)
			if !ok {
				return nil, false
			}
			args := make([]any, len(call.Args)-1)
			for i, a := range call.Args[1:] {
				v, ok := c.value(a, depth+1)
				if !ok {
					return nil, false
				}
				args[i] = v
			}
			return fmt.Sprintf(format, args...), true
		case "Sprint":
			var b strings.Builder
			for _, a := range call.Args {
				v, ok := c.value(a, depth+1)
				if !ok {
					return nil, false
				}
				b.WriteString(fmt.Sprint(v))
			}
			return b.String(), true
		}
	case "strconv":
		switch sel.Sel.Name {
		case "Itoa":
			if len(call.Args) == 1 {
				if n, ok := c.num(call.Args[0], depth+1); ok {
					return strconv.Itoa(int(n)), true
				}
			}
		case "FormatInt":
			if len(call.Args) == 2 {
				n, ok := c.num(call.Args[0], depth+1)
				if !ok {
					return nil, false
				}
				base, ok := c.num(call.Args[1], depth+1)
				if !ok || base < 2 || base > 36 {
					return nil, false
				}
				return strconv.FormatInt(n, int(base)), true
			}
		}
	case "os":
		if sel.Sel.Name == "TempDir" && len(call.Args) == 0 {
			return os.TempDir(), true
		}
	}
	return nil, false
}

func isTestingParam(id *ast.Ident) bool {
	if id.Obj == nil || id.Obj.Kind != ast.Var {
		return false
	}
	field, ok := id.Obj.Decl.(*ast.Field)
	if !ok {
		return false
	}
	t := unparen(field.Type)
	if star, ok := t.(*ast.StarExpr); ok {
		t = unparen(star.X)
	}
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "testing" {
		return false
	}
	switch sel.Sel.Name {
	case "T", "B", "F", "TB":
		return true
	}
	return false
}

func (c *guardConsts) str(e ast.Expr, depth int) (string, bool) {
	v, ok := c.value(e, depth)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (c *guardConsts) num(e ast.Expr, depth int) (int64, bool) {
	v, ok := c.value(e, depth)
	if !ok {
		return 0, false
	}
	n, ok := v.(int64)
	return n, ok
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
// <anything>.Base(x) or <anything>.Split(x) where x is derived from a
// t.TempDir() path: inline, through a variable the same file assigned it to
// (directly, through var, or through an alias), through filepath.Clean,
// ToSlash or FromSlash, through a one-argument Join, through a trailing
// separator, or as the first result of filepath.Abs or EvalSymlinks. Variables
// are followed by *ast.Object, so a same-spelled dir bound to "/usr/bin"
// elsewhere, or shadowed in an inner block, is never flagged.
func tempDirBaseFindings(f *ast.File) []token.Pos {
	c := newGuardConsts(f)
	temps := map[*ast.Object]bool{}
	mark := func(lhs, rhs []ast.Expr) {
		for i, r := range rhs {
			if i >= len(lhs) {
				break
			}
			id, ok := lhs[i].(*ast.Ident)
			if !ok || id.Name == "_" || id.Obj == nil {
				continue
			}
			if tempDerived(c, r, temps) {
				temps[id.Obj] = true
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			if len(x.Lhs) == 2 && len(x.Rhs) == 1 {
				if call, ok := unparen(x.Rhs[0]).(*ast.CallExpr); ok {
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && (sel.Sel.Name == "Abs" || sel.Sel.Name == "EvalSymlinks") && len(call.Args) == 1 && tempDerived(c, call.Args[0], temps) {
						if id, ok := x.Lhs[0].(*ast.Ident); ok && id.Obj != nil {
							temps[id.Obj] = true
						}
					}
				}
			}
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
			if fun.Sel.Name != "Base" && fun.Sel.Name != "Split" {
				return true
			}
		case *ast.Ident:
			if fun.Name != "Base" && fun.Name != "Split" {
				return true
			}
		default:
			return true
		}
		if tempDerived(c, call.Args[0], temps) {
			out = append(out, call.Pos())
		}
		return true
	})
	return out
}

// tempDerived reports whether e names a path derived from a temp dir in a way
// that keeps its last element: the base of such a path is the constant "001".
func tempDerived(c *guardConsts, e ast.Expr, temps map[*ast.Object]bool) bool {
	e = unparen(e)
	if isTempDirCall(e) || isMarkedTemp(e, temps) {
		return true
	}
	switch x := e.(type) {
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		id, _ := sel.X.(*ast.Ident)
		pkg := ""
		if id != nil {
			pkg = id.Name
		}
		if pkg != "filepath" && pkg != "path" {
			return false
		}
		if sel.Sel.Name == "Join" && len(x.Args) == 1 {
			return tempDerived(c, x.Args[0], temps)
		}
		if (sel.Sel.Name == "Clean" || sel.Sel.Name == "ToSlash" || sel.Sel.Name == "FromSlash") && len(x.Args) == 1 {
			return tempDerived(c, x.Args[0], temps)
		}
	case *ast.BinaryExpr:
		if x.Op == token.ADD && tempDerived(c, x.X, temps) {
			if s, ok := c.str(x.Y, 0); ok && s != "" && strings.Trim(s, "/") == "" {
				return true
			}
		}
	}
	return false
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

// fixedPortNote names the second reason a constant port outside the drawer's
// window is refused: below 1024 a bind needs privileges on Linux, and above
// 32767 the number lies in an ephemeral range an OS hands to any process's
// bind-to-zero or outbound connection, so it can be held by an unrelated
// process between the draw and the bind (#176, #188).
func fixedPortNote(port int64) string {
	if port < 1024 || port > 32767 {
		return " Port " + strconv.FormatInt(port, 10) + " is also outside 1024-32767: below it a bind needs privileges on Linux; above it the number lies in an OS ephemeral range (32768-60999 Linux, 49152-65535 macOS) that any process's bind-to-zero or outbound connection can be handed (#176, #188)."
	}
	return ""
}

type portFinding struct {
	pos token.Pos
	msg string
}

// fixedPortFindings is an allowlist: a bind passes only when the folded
// address has port 0, or when drawerTrusted names this file and the bind sits
// in the plain func portOutsideTheEphemeralPool (adapter/adapter_test.go), the
// one helper that may bind a port it computed. Anything else — a constant
// port, or an address the guard cannot fold — is refused, because a denylist
// of constants can always be hidden behind one more spelling.
func fixedPortFindings(f *ast.File, drawerTrusted bool) []portFinding {
	c := newGuardConsts(f)
	var out []portFinding
	add := func(pos token.Pos, msg string) {
		if msg != "" {
			out = append(out, portFinding{pos: pos, msg: msg})
		}
	}
	for _, d := range f.Decls {
		inDrawer := drawerTrusted
		if fn, ok := d.(*ast.FuncDecl); !ok || fn.Recv != nil || fn.Name.Name != trustedDrawerFunc {
			inDrawer = false
		}
		ast.Inspect(d, func(n ast.Node) bool {
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
					if netw, ok := c.str(x.Args[0], 0); ok && isUnixNetwork(netw) {
						return true
					}
					add(x.Pos(), addrMsg(c, x.Args[1], inDrawer))
				case pkgName == "http" && (sel.Sel.Name == "ListenAndServe" || sel.Sel.Name == "ListenAndServeTLS"):
					if len(x.Args) >= 1 {
						add(x.Pos(), addrMsg(c, x.Args[0], inDrawer))
					}
				case pkgName != "net" && pkgName != "http" && (sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket") && len(x.Args) == 3:
					// a net.ListenConfig's Listen(ctx, network, address)
					if netw, ok := c.str(x.Args[1], 0); ok && isUnixNetwork(netw) {
						return true
					}
					add(x.Pos(), addrMsg(c, x.Args[2], inDrawer))
				case pkgName == "net" && (sel.Sel.Name == "ListenTCP" || sel.Sel.Name == "ListenUDP"):
					if len(x.Args) >= 2 {
						add(x.Pos(), sockaddrMsg(c, x.Args[1], inDrawer))
					}
				case pkgName == "net" && sel.Sel.Name == "ListenMulticastUDP":
					if len(x.Args) >= 3 {
						add(x.Pos(), sockaddrMsg(c, x.Args[2], inDrawer))
					}
				case pkgName != "http" && sel.Sel.Name == "ListenAndServe" && len(x.Args) == 0:
					// a server whose Addr this guard cannot see in the call
					if !inDrawer {
						add(x.Pos(), unseenPortMsg)
					}
				case pkgName != "http" && sel.Sel.Name == "ListenAndServeTLS" && len(x.Args) == 2:
					if !inDrawer {
						add(x.Pos(), unseenPortMsg)
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
					if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Addr" {
						add(x.Pos(), addrMsg(c, kv.Value, inDrawer))
						break
					}
				}
			}
			return true
		})
	}
	return out
}

func isUnixNetwork(network string) bool {
	switch network {
	case "unix", "unixgram", "unixpacket":
		return true
	}
	return false
}

// portMsg answers for a folded port: empty and numeric 0 pass; a number is a
// constant port with the window note; a named service is a constant port.
func portMsg(port string) string {
	if port == "" {
		return ""
	}
	if n, err := strconv.Atoi(port); err == nil {
		if n == 0 {
			return ""
		}
		return fixedPortMsg + fixedPortNote(int64(n))
	}
	return fixedPortMsg // a named port such as http
}

// addrMsg reports a host:port address expression: folded addresses answer by
// their port, a folded JoinHostPort by its port operand, and anything else is
// unseen unless the bind is the trusted drawer's.
func addrMsg(c *guardConsts, e ast.Expr, inDrawer bool) string {
	if s, ok := c.str(e, 0); ok {
		_, port, err := netSplitHostPort(s)
		if err != nil {
			return "" // nothing can bind an address with no port
		}
		return portMsg(port)
	}
	if call, ok := unparen(e).(*ast.CallExpr); ok {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "JoinHostPort" && len(call.Args) == 2 {
			if p, ok := c.str(call.Args[1], 0); ok {
				return portMsg(p)
			}
		}
	}
	if inDrawer {
		return ""
	}
	return unseenPortMsg
}

// sockaddrMsg reports a net.TCPAddr or net.UDPAddr sockaddr: a folded zero
// port (or no Port at all) passes, a folded nonzero port is a constant port,
// and anything else is unseen unless the bind is the trusted drawer's.
func sockaddrMsg(c *guardConsts, e ast.Expr, inDrawer bool) string {
	if id, ok := unparen(e).(*ast.Ident); ok && id.Name == "nil" && id.Obj == nil {
		return ""
	}
	e = unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = unparen(u.X)
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok || (!isType(lit.Type, "net", "TCPAddr") && !isType(lit.Type, "net", "UDPAddr")) {
		if inDrawer {
			return ""
		}
		return unseenPortMsg
	}
	var port ast.Expr
	keyed := false
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		keyed = true
		if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "Port" {
			port = kv.Value
		}
	}
	if port == nil && !keyed && len(lit.Elts) >= 2 {
		port = lit.Elts[1]
	}
	if port == nil {
		return ""
	}
	if n, ok := c.num(port, 0); ok {
		if n == 0 {
			return ""
		}
		return fixedPortMsg + fixedPortNote(n)
	}
	if inDrawer {
		return ""
	}
	return unseenPortMsg
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

// --- fixed paths -----------------------------------------------------------

// touchPathArgs names the os and ioutil calls that create, change or remove a
// file or directory, and which argument is the path. Rename has both sides:
// a constant source or a constant destination is a shared name.
var touchPathArgs = map[string][]int{
	"Create": {0}, "WriteFile": {0}, "Mkdir": {0}, "MkdirAll": {0}, "OpenFile": {0},
	"Truncate": {0}, "Remove": {0}, "RemoveAll": {0}, "Chmod": {0}, "Chtimes": {0},
	"Chown": {0}, "Lchown": {0}, "Rename": {0, 1}, "Symlink": {1}, "Link": {1},
}

// fixedPathFindings returns the position of every creation, change or removal
// of a file or socket at a constant path: the name is one system namespace,
// and concurrent runs on one machine collide in it.
func fixedPathFindings(f *ast.File) []token.Pos {
	c := newGuardConsts(f)
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
		var args []int
		if pkgName == "os" || pkgName == "ioutil" {
			args = touchPathArgs[sel.Sel.Name]
			if sel.Sel.Name == "OpenFile" && len(call.Args) >= 2 && readOnlyFlag(c, call.Args[1]) {
				return true
			}
		}
		if (sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket") && pkgName != "net" && len(call.Args) == 3 {
			// a net.ListenConfig's Listen(ctx, network, address)
			if netw, ok := c.str(call.Args[1], 0); ok && isUnixNetwork(netw) {
				args = []int{2}
			}
		}
		if pkgName == "net" {
			switch sel.Sel.Name {
			case "Listen", "ListenPacket":
				if len(call.Args) >= 2 {
					if netw, ok := c.str(call.Args[0], 0); ok && isUnixNetwork(netw) {
						args = []int{1}
					}
				}
			case "ListenUnix", "ListenUnixgram":
				if len(call.Args) >= 2 && fixedUnixName(c, call.Args[1]) {
					out = append(out, call.Pos())
				}
			}
		}
		for _, arg := range args {
			if arg < len(call.Args) && fixedPath(c, call.Args[arg]) {
				out = append(out, call.Pos())
				break
			}
		}
		return true
	})
	return out
}

// readOnlyFlag reports a folded 0 flag, or a selector named O_RDONLY: an open
// that writes nothing needs no per-run name.
func readOnlyFlag(c *guardConsts, e ast.Expr) bool {
	if n, ok := c.num(e, 0); ok {
		return n == 0
	}
	sel, ok := unparen(e).(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "O_RDONLY"
}

// fixedPath reports a folded constant path, unless it is /dev/null or under
// testdata/. A path built from os.TempDir(), a test's t.Name(), MkdirTemp or
// CreateTemp keeps a per-run component and does not fold, so it is allowed.
func fixedPath(c *guardConsts, e ast.Expr) bool {
	s, ok := c.str(e, 0)
	if !ok {
		return false
	}
	s = filepath.Clean(s)
	return s != "/dev/null" && s != "testdata" && !strings.HasPrefix(s, "testdata/")
}

func fixedUnixName(c *guardConsts, e ast.Expr) bool {
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
			s, ok := c.str(kv.Value, 0)
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
// Keychain (KeychainService excepted), keychainPut, keychainMove,
// keychainWalk, or an exec
// of security — without a stubSecurity(t) statement in an enclosing body
// before the reach, with a trusted stub declared, and with no PATH write
// anywhere in the declaration. An absolute path to security is never covered:
// no PATH stub can intercept it.
func unstubbedKeychainRefs(f *ast.File, trusted bool, pathWriters map[string]bool) []keychainRefSite {
	c := newGuardConsts(f)
	var out []keychainRefSite
	for _, decl := range f.Decls {
		switch x := decl.(type) {
		case *ast.FuncDecl:
			if x.Body == nil {
				continue
			}
			if pos, ok := firstUncoveredReach(c, x.Body, trusted, pathWriters); ok {
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
							if p, _, ok := keychainReach(c, n); ok {
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

func firstUncoveredReach(c *guardConsts, body *ast.BlockStmt, trusted bool, pathWriters map[string]bool) (token.Pos, bool) {
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
		if call, ok := n.(*ast.CallExpr); ok && pathWriteCall(c, call, pathWriters) {
			pathWrite = true
		}
		return true
	})
	var first token.Pos
	ast.Inspect(body, func(n ast.Node) bool {
		if p, absolute, ok := keychainReach(c, n); ok && first == token.NoPos {
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

func keychainReach(c *guardConsts, n ast.Node) (token.Pos, bool, bool) {
	switch x := n.(type) {
	case *ast.Ident:
		if strings.HasPrefix(x.Name, "Keychain") && x.Name != "KeychainService" {
			return x.Pos(), false, true
		}
		if x.Name == "keychainPut" || x.Name == "keychainMove" || x.Name == "keychainWalk" {
			return x.Pos(), false, true
		}
	case *ast.CallExpr:
		sel, ok := x.Fun.(*ast.SelectorExpr)
		if !ok {
			return 0, false, false
		}
		id, ok := unparen(sel.X).(*ast.Ident)
		if !ok {
			return 0, false, false
		}
		switch {
		case id.Name == "exec" && sel.Sel.Name == "Command":
			return securityReach(c, x.Args...)
		case id.Name == "exec" && sel.Sel.Name == "CommandContext" && len(x.Args) > 0:
			return securityReach(c, x.Args[1:]...)
		case id.Name == "exec" && sel.Sel.Name == "LookPath" && len(x.Args) > 0:
			return securityReach(c, x.Args[0])
		case id.Name == "os" && sel.Sel.Name == "StartProcess" && len(x.Args) >= 1:
			return securityReach(c, append([]ast.Expr{x.Args[0]}, stringElems(x.Args[1])...)...)
		case id.Name == "syscall" && sel.Sel.Name == "Exec" && len(x.Args) >= 1:
			return securityReach(c, append([]ast.Expr{x.Args[0]}, stringElems(x.Args[1])...)...)
		}
	case *ast.CompositeLit:
		if !isType(x.Type, "exec", "Cmd") {
			return 0, false, false
		}
		var exprs []ast.Expr
		for _, el := range x.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				switch key.Name {
				case "Path":
					exprs = append(exprs, kv.Value)
				case "Args":
					exprs = append(exprs, stringElems(kv.Value)...)
				}
			}
		}
		return securityReach(c, exprs...)
	}
	return 0, false, false
}

// stringElems returns the folded-string elements of a []string composite, so
// an argv list is judged word by word.
func stringElems(e ast.Expr) []ast.Expr {
	lit, ok := unparen(e).(*ast.CompositeLit)
	if !ok {
		return nil
	}
	return lit.Elts
}

// commandWords splits a folded command string into the words a shell would
// run: on whitespace and on every control or quoting rune. It is deliberately
// blunt — a word "security" anywhere in a command is a reach — so
// `sh -c "security ..."` cannot hide the tool behind a shell.
func commandWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		if unicode.IsSpace(r) {
			return true
		}
		return strings.ContainsRune(";&|()<>`$'\"", r)
	})
}

// securityIn reports whether s runs security, and whether any spelling of it
// carried a path (which no PATH stub can intercept).
func securityIn(s string) (relative, absolute bool) {
	for _, w := range commandWords(s) {
		if w == "security" {
			relative = true
		}
		if strings.Contains(w, "/") && path.Base(w) == "security" {
			absolute = true
		}
	}
	return relative, absolute
}

func securityReach(c *guardConsts, exprs ...ast.Expr) (token.Pos, bool, bool) {
	var pos token.Pos
	absolute := false
	for _, e := range exprs {
		s, ok := c.str(e, 0)
		if !ok {
			continue
		}
		rel, abs := securityIn(s)
		if !rel && !abs {
			continue
		}
		if pos == token.NoPos {
			pos = e.Pos()
		}
		if abs {
			absolute = true
		}
	}
	return pos, absolute, pos != token.NoPos
}

func pathWriteCall(c *guardConsts, call *ast.CallExpr, pathWriters map[string]bool) bool {
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
		k, ok := c.str(call.Args[0], 0)
		return ok && k == "PATH"
	}
	return false
}

// pathWriterFuncs names the plain same-module helpers that write PATH, so a
// function that calls one after its stub is refused; one level is followed.
func pathWriterFuncs(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		c := newGuardConsts(f)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil || fn.Name.Name == "stubSecurity" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && pathWriteCall(c, call, nil) {
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

func SplitInline(t *testing.T) {
	_, _ = filepath.Split(t.TempDir()) // want
}

func SplitAssign(t *testing.T) {
	dir := t.TempDir()
	_, _ = filepath.Split(dir) // want
}

func ThroughClean(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Base(filepath.Clean(dir)) // want
}

func ThroughSeparator(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Base(dir + "/") // want
}

func ThroughJoinOne(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Base(filepath.Join(dir)) // want
}

func ThroughToSlash(t *testing.T) {
	dir := t.TempDir()
	_ = filepath.Base(filepath.ToSlash(dir)) // want
}

func ThroughAbs(t *testing.T) {
	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	_ = filepath.Base(abs) // want
}

func ThroughEvalSymlinks(t *testing.T) {
	dir := t.TempDir()
	r, _ := filepath.EvalSymlinks(dir)
	_ = path.Base(r) // want
}
`

const goodTempBaseFixture = `package p

import (
	"path/filepath"
	"strings"
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

func SplitOfParent(t *testing.T) {
	dir := t.TempDir()
	_, _ = filepath.Split(filepath.Dir(dir))
	_, _ = strings.Split(dir, "/")
	_ = filepath.Base(dir + "/x")
}
`

const badPortFixture = `package p

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
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

func HiddenByVar(t *testing.T) {
	addr := ":7002"
	net.Listen("tcp", addr) // want
}

func HiddenByItoa(t *testing.T) {
	p := 49173
	net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))) // want
}

func HiddenBySprintf(t *testing.T) {
	port := 4000
	net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)) // want
}

func ThroughHelper(t *testing.T) {
	listenOn(t, ":7003")
}

func listenOn(t *testing.T, addr string) {
	net.Listen("tcp", addr) // want
}

func FromListenerAddr(t *testing.T, l net.Listener) {
	net.Listen("tcp", l.Addr().String()) // want
}

func FromSockaddrVar(t *testing.T, a *net.TCPAddr) {
	net.ListenTCP("tcp", a) // want
}

func PositionalTCPAddr(t *testing.T, ip net.IP) {
	net.ListenTCP("tcp", &net.TCPAddr{ip, 4004, ""}) // want
}

func PortFromParam(t *testing.T, p int) {
	net.ListenTCP("tcp", &net.TCPAddr{Port: p}) // want
}

func MulticastFixed(t *testing.T, ip net.IP) {
	net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: ip, Port: 5353}) // want
}

func ServerNoAddr(t *testing.T, srv *http.Server) {
	srv.ListenAndServe() // want
	srv.ListenAndServeTLS("cert", "key") // want
}

func ServerAddrParam(t *testing.T, addr string) {
	_ = http.Server{Addr: addr} // want
}

func JoinedPortParam(t *testing.T, p string) {
	net.Listen("tcp", net.JoinHostPort("127.0.0.1", p)) // want
}
`

const drawerFixture = `package p

import (
	"math/rand"
	"net"
	"strconv"
	"testing"
)

func portOutsideTheEphemeralPool(t *testing.T) int {
	for range 64 {
		p := 1024 + rand.IntN(32768-1024)
		l, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p)) // untrusted
		if err != nil {
			continue
		}
		l.Close()
		return p
	}
	t.Fatal("no port")
	return 0
}

func ConstantInsideDrawer(t *testing.T) {
	net.Listen("tcp4", "127.0.0.1:4000") // want
}

type helper struct{}

func (h helper) portOutsideTheEphemeralPool(t *testing.T) int {
	p := 1024 + rand.IntN(32768-1024)
	net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p)) // want
	return p
}

func Elsewhere(t *testing.T) {
	p := 1024 + rand.IntN(32768-1024)
	net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(p)) // want
}
`

const goodPortFixture = `package p

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
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

func ZeroThroughVar(t *testing.T) {
	addr := "127.0.0.1:0"
	net.Listen("tcp", addr)
	const zeroAddr = ":0"
	net.Listen("tcp", zeroAddr)
}

func ZeroHostOnly(t *testing.T) {
	net.Listen("tcp", "127.0.0.1:")
}

func ZeroJoined(t *testing.T, host string) {
	net.Listen("tcp", net.JoinHostPort(host, "0"))
	net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 0))
}

func NilSockaddr(t *testing.T) {
	net.ListenTCP("tcp", nil)
}

func ZeroTCPAddr(t *testing.T, ip net.IP) {
	net.ListenTCP("tcp", &net.TCPAddr{Port: 0})
	net.ListenTCP("tcp", &net.TCPAddr{})
	net.ListenUDP("udp", &net.UDPAddr{IP: ip})
	net.ListenTCP("tcp", &net.TCPAddr{ip, 0, ""})
}

func NoServerAddr(t *testing.T, h http.Handler) {
	_ = http.Server{Handler: h}
}

func UnixBind(t *testing.T) {
	net.Listen("unix", "/tmp/x.sock")
	lc := net.ListenConfig{}
	lc.Listen(context.Background(), "unix", filepath.Join(t.TempDir(), "s"))
}

func Dial(t *testing.T) {
	net.Dial("tcp", "127.0.0.1:1")
}
`

const badPathFixture = `package p

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"time"
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
	os.OpenFile(path.Join(os.TempDir(), "a", "b"), os.O_CREATE|os.O_WRONLY, 0o600) // want
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

func HiddenByVar(t *testing.T) {
	p := "/tmp/ccm-var"
	os.WriteFile(p, nil, 0o600) // want
}

func HiddenBySprintf(t *testing.T) {
	os.WriteFile(fmt.Sprintf("/tmp/ccm-%d", 1), nil, 0o600) // want
}

func HiddenByName(t *testing.T) {
	os.Create(filepath.Join(os.TempDir(), t.Name())) // want
}

func Cleaned(t *testing.T) {
	os.MkdirAll(filepath.Clean("/tmp/ccm/../ccm2"), 0o700) // want
}

func OpenCreate(t *testing.T) {
	os.OpenFile("/tmp/ccm-open", os.O_CREATE|os.O_WRONLY, 0o600) // want
}

func RemoveFixed(t *testing.T) {
	os.Remove("/tmp/ccm-rm") // want
}

func RemoveAllTemp(t *testing.T) {
	os.RemoveAll(filepath.Join(os.TempDir(), "ccm-dir")) // want
}

func ChmodFixed(t *testing.T) {
	os.Chmod("/tmp/ccm-mode", 0o600) // want
}

func TruncateFixed(t *testing.T) {
	os.Truncate("/tmp/ccm-trunc", 0) // want
}

func ChtimesFixed(t *testing.T) {
	os.Chtimes("/tmp/ccm-times", time.Time{}, time.Time{}) // want
}

func WriteRelativeDot(t *testing.T) {
	os.WriteFile("./relative.txt", nil, 0o600) // want
}

func RenameSourceTemp(t *testing.T) {
	dir := t.TempDir()
	os.Rename("/tmp/src", filepath.Join(dir, "d")) // want
}

func ListenConfigUnix(t *testing.T) {
	lc := net.ListenConfig{}
	lc.Listen(context.Background(), "unix", "/tmp/ccm3.sock") // want
}
`

const goodPathFixture = `package p

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	os.Link("/tmp/src", filepath.Join(dir, "h"))
}

func DrawnName(name string) {
	os.Create(filepath.Join(os.TempDir(), name))
}

func DrawnByProcess(t *testing.T) {
	os.Create(filepath.Join(os.TempDir(), fmt.Sprintf("ccm-%d", os.Getpid())))
	os.Create(filepath.Join(os.TempDir(), "ccm-"+strconv.Itoa(os.Getpid())))
}

func ReadOnly(t *testing.T) {
	os.OpenFile("/etc/hosts", os.O_RDONLY, 0)
	os.OpenFile("/etc/hosts", 0, 0)
	os.Open("/etc/hosts")
	os.Stat("/tmp")
}

func TestdataDot(t *testing.T) {
	os.WriteFile("./testdata/x.golden", nil, 0o600)
}

func UnderVarTemp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x")
	os.WriteFile(p, nil, 0o600)
}

func UnixBindTemp(t *testing.T) {
	net.Listen("unix", filepath.Join(t.TempDir(), "s"))
}
`

const badKeychainFixture = `package p

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
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

func TestNameVar(t *testing.T) {
	name := "security"
	_ = exec.Command(name, "default-keychain").Run() // want
}

func TestSprintfName(t *testing.T) {
	_ = exec.Command(fmt.Sprintf("%s", "security"), "default-keychain").Run() // want
}

func TestJoinedAbsolute(t *testing.T) {
	stubSecurity(t)
	_ = exec.Command(filepath.Join("/usr/bin", "security"), "default-keychain").Run() // want
}

func TestShellWord(t *testing.T) {
	_ = exec.Command("sh", "-c", "security default-keychain").Run() // want
}

func TestEnvWord(t *testing.T) {
	_ = exec.Command("/usr/bin/env", "security").Run() // want
}

func TestShellAbsolute(t *testing.T) {
	stubSecurity(t)
	_ = exec.Command("sh", "-c", "/usr/bin/security x").Run() // want
}

func TestLookPath(t *testing.T) {
	_, _ = exec.LookPath("security") // want
}

func TestStartProcess(t *testing.T) {
	stubSecurity(t)
	_, _ = os.StartProcess("/usr/bin/security", []string{"security"}, nil) // want
}

func TestSyscallExec(t *testing.T) {
	stubSecurity(t)
	_ = syscall.Exec("/usr/bin/security", nil, nil) // want
}

func TestCmdLiteral(t *testing.T) {
	stubSecurity(t)
	_ = &exec.Cmd{Path: "/usr/bin/security"} // want
}

func TestPathKeyVar(t *testing.T) {
	k := "PATH"
	stubSecurity(t)
	t.Setenv(k, "/usr/bin")
	KeychainDelete("x") // want
}

func TestWalkWithoutStub(t *testing.T) {
	keychainWalk(t.TempDir()) // want
}

func TestWalkAfterStub(t *testing.T) {
	stubSecurity(t)
	keychainWalk(t.TempDir())
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

func TestWordContainsName(t *testing.T) {
	_ = exec.Command("echo", "insecurity").Run()
	_ = exec.Command("git", "config", "core.security-x").Run()
}

var _ = os.Getenv
`

// constsFixture pins the folder: "// = <literal>" is the exact value,
// "// const" is any constant, and "// not" must not fold.
const constsFixture = `package p

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
)

const packageConst = "/tmp/pkg"

var packageVar = "/tmp/var"

var a2, b2 string

var self = self + "b"

func Fold(t *testing.T, param string) {
	_ = "lit" // = "lit"
	_ = 1 + 2*3 // = 7
	_ = (1 + 2) * 3 // = 9
	_ = 0x10 // = 16
	_ = "a" + "b" // = "ab"
	_ = packageConst // = "/tmp/pkg"
	_ = packageVar // = "/tmp/var"
	_ = filepath.Join("/tmp", "ccm") // = "/tmp/ccm"
	_ = path.Join("a", "b") // = "a/b"
	_ = filepath.Clean("/tmp/a/../b") // = "/tmp/b"
	_ = fmt.Sprintf("%s-%d", "x", 2) // = "x-2"
	_ = fmt.Sprint("x", 2) // = "x2"
	_ = strconv.Itoa(49173) // = "49173"
	_ = strconv.FormatInt(255, 16) // = "ff"
	_ = os.TempDir() // const
	_ = t.Name() // = "t.Name()"
	p := 49173
	p = 5
	_ = p // not
	q := "/tmp/q"
	q = "/tmp/q"
	_ = q // = "/tmp/q"
	s := "/tmp/s"
	s += "/x"
	_ = s // not
	r := 1
	r = r + 1
	_ = r // not
	param = param
	_ = param // not
	_ = os.Getpid() // not
	_ = fmt.Sprintf(param, 1) // not
	_ = fmt.Sprintf("%s", []string{"a"}...) // not
	_ = os.DevNull // not
	a2, b2 = b2, a2
	_ = a2 // not
	for i := range 3 {
		_ = i // not
	}
	v := 7
	w := &v
	_ = w
	_ = v // not
	_ = nil // not
	_ = self // not
}
`
