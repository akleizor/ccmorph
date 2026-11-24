package ctxwindow

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// #767. Kind (.github/REVIEW.md): a reader that turns unparseable input into a
// confident verdict. A 200 body that is not a listing (a proxy's error
// envelope) has no data list; a reader that takes the "data" key itself reads
// it as an empty listing, and `ccmorph context` then says the model "is not on
// this route". ListingShape (#747) is the one reader of that key. This guard
// type-checks every non-test .go file of the module with go/types (the files
// of every build constraint together, so a darwin-only reader fails it on
// linux too; every other import from its export data, `go list -export`) and
// fails, by file and line, on any other read of the key. A name reads it when
// it folds to data the widest way any encoding/json API matches a name: case,
// dashes and underscores ignored (DATA, da_ta). The reads are:
//
//   - key: a constant string that is such a name, or holds one as a segment
//     between . [ ] / " ' or its ends (a path "data.0.id", a pointer
//     "/data/0", JSON text `{"data":`): a literal, a concatenation, a named or
//     typed constant, a conversion; in an index, any call, a case or a const;
//     on one line or across several.
//   - field: a struct field declared in the module that encoding/json fills
//     from the key, by its own field rules under either build of it (the v2
//     engine Go 1.27 builds by default, or the original under
//     GOEXPERIMENT=nojsonv2): a field named or tagged data; an embedded
//     non-struct type named Data; a member that an embedded struct the tag
//     leaves unnamed (or, in the v2 engine, a field with the embed option)
//     promotes from any package, reported at that field, unless a shallower or
//     tagged member of the name hides it or two at one depth cancel. An
//     embedded struct that promotes no such member is no read.
//   - type: any expression or type in the module whose type is, or holds
//     through pointers, slices, arrays and maps, a struct declared outside the
//     module (the standard library, an SDK's listing type) that encoding/json
//     fills from the key: a decode into a type the field rule cannot see.
//
// Whether a value is ever decoded is not judged: a type that would read the key
// is reported, and one that never meets JSON is named in dataKeyOwners with
// why. A writer is not a reader: the first argument of a .Set call is not a
// key, and a struct tag is judged as its field. A key built at run time, not a
// constant, is out of its reach. A struct's fields are judged even when it has
// its own UnmarshalJSON. An import the guard cannot resolve fails it rather
// than leave a type unjudged.

// dataKeyOwners are the only declarations that may read the key. Every entry
// must still match, so the list cannot hide a stale or misspelt exemption.
var dataKeyOwners = map[string]string{
	"ctxwindow/listing.go ListingShape":        "the shared verdict itself",
	"quota/balance.go balanceReaders":          "Moonshot's and SiliconFlow's balance replies (balanceField paths), not a /v1/models listing",
	"quota/balance.go openrouterBalance":       "OpenRouter's credits reply, not a /v1/models listing",
	"quota/balance.go zhipuBalance":            "Zhipu's balance reply, not a /v1/models listing",
	"util/paths/atomic.go WriteIn":             "the bytes of an atomic write, never decoded from JSON",
	"util/paths/usagestore.go UsageArchiveRow": "a usage receipt's bytes, never decoded from JSON",
	"util/paths/usagestore.go ArchiveStamp":    "the content file's stat stamp of a usage archive, built from os.Stat, never decoded from JSON",
	"shim/usagearchive.go storedUsageRow":      "embeds paths.UsageArchiveRow (a receipt's bytes); an in-memory row, never decoded from JSON",
	"shim/sse.go bareDataField":                "the one compare of an SSE line against the bare \"data\" field name (no colon, #689): raw bytes of a streamed reply, never a /v1/models body and never decoded from JSON",
}

// dataKeyRead is one read of the data key: where, in which declaration, and
// how: a constant "key", a struct "field" encoding/json fills from it, or a
// "type" from outside the module that it fills.
type dataKeyRead struct {
	Rel   string
	Line  int
	Owner string // the enclosing func, method, type, var or const; "" for none
	Kind  string // "key", "field" or "type"
}

func (r dataKeyRead) String() string {
	return r.Rel + ":" + strconv.Itoa(r.Line) + " (" + r.Owner + ", " + r.Kind + ")"
}

// source is one non-test .go file: its slash path from the module root, its
// module's path, its syntax, and the type information of its package.
type source struct {
	rel    string
	module string
	file   *ast.File
	info   *types.Info
}

// importerFunc adapts a function to types.Importer.
type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// exportData is the export data file of each of paths, and of everything they
// import, from one `go list -export` run in root: the standard library, and any
// module go.mod requires. A path with none is left out, and its import fails.
func exportData(t *testing.T, root string, paths []string) map[string]string {
	t.Helper()
	files := map[string]string{}
	args := []string{"list", "-e", "-export", "-deps", "-f", "{{.ImportPath}} {{.Export}}"}
	listed := map[string]bool{"unsafe": true, "C": true}
	for _, p := range paths {
		if !listed[p] {
			listed[p] = true
			args = append(args, p)
		}
	}
	if len(listed) == 2 {
		return files
	}
	var stderr bytes.Buffer
	cmd := exec.Command("go", args...)
	cmd.Dir = root
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -export: %v\n%s", err, stderr.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p, file, ok := strings.Cut(line, " "); ok && file != "" {
			files[p] = file
		}
	}
	return files
}

// moduleSources is every non-test .go file of this module, slash paths from
// the module root. It skips what `go build ./...` skips: directories named
// with a leading . or _, testdata, and nested modules (another checkout).
func moduleSources(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// modulePath is the module line of the checkout's go.mod.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

// loadModule parses and type-checks every source of the module at root, each
// package from its source and every other import from its export data. The
// files of every build constraint are checked together, so a type error is
// tolerated (a file for another platform need not check here); an import that
// cannot be resolved fails the test.
func loadModule(t *testing.T, root string) (*token.FileSet, []*source) {
	t.Helper()
	module := modulePath(t, root)
	fset := token.NewFileSet()
	var sources []*source
	byPackage := map[string][]*source{}
	for _, rel := range moduleSources(t, root) {
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		importPath := module
		if dir := path.Dir(rel); dir != "." {
			importPath += "/" + dir
		}
		src := &source{rel: rel, module: module, file: file}
		byPackage[importPath] = append(byPackage[importPath], src)
		sources = append(sources, src)
	}
	var imports []string
	for _, src := range sources {
		for _, spec := range src.file.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && byPackage[p] == nil {
				imports = append(imports, p)
			}
		}
	}
	exports := exportData(t, root, imports)
	external := importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		file, ok := exports[p]
		if !ok {
			return nil, fmt.Errorf("go list -export named no export data")
		}
		return os.Open(file)
	})
	checked := map[string]*types.Package{}
	var unresolved []string
	var check func(importPath string) *types.Package
	imp := importerFunc(func(p string) (*types.Package, error) {
		if _, ok := byPackage[p]; ok {
			if pkg := check(p); pkg != nil {
				return pkg, nil
			}
			unresolved = append(unresolved, p+": import cycle")
			return nil, os.ErrInvalid
		}
		pkg, err := external.Import(p)
		if err != nil {
			unresolved = append(unresolved, p+": "+err.Error())
		}
		return pkg, err
	})
	check = func(importPath string) *types.Package {
		if pkg, ok := checked[importPath]; ok {
			return pkg
		}
		checked[importPath] = nil // a cycle meets nil
		info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
		var files []*ast.File
		for _, src := range byPackage[importPath] {
			src.info = info
			files = append(files, src.file)
		}
		conf := types.Config{Importer: imp, Error: func(error) {}}
		pkg, _ := conf.Check(importPath, fset, files, info)
		checked[importPath] = pkg
		return pkg
	}
	var paths []string
	for importPath := range byPackage {
		paths = append(paths, importPath)
	}
	sort.Strings(paths)
	for _, importPath := range paths {
		check(importPath)
	}
	if len(unresolved) > 0 {
		t.Fatalf("the guard could not resolve every import, so it cannot judge every type: %s", strings.Join(unresolved, "; "))
	}
	return fset, sources
}

// constString folds a string literal, a parenthesised one, or a + of them:
// the fallback where go/types recorded no constant value.
func constString(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return constString(x.X)
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok := constString(x.X)
		b, ok2 := constString(x.Y)
		return a + b, ok && ok2
	}
	return "", false
}

// constKey is the constant string e denotes: go/types' value when it folded
// one, else the literals' own.
func constKey(info *types.Info, e ast.Expr) (string, bool) {
	if tv, ok := info.Types[e]; ok && tv.Value != nil {
		if tv.Value.Kind() != constant.String {
			return "", false
		}
		return constant.StringVal(tv.Value), true
	}
	return constString(e)
}

// foldsToData is whether name folds to data the widest way any encoding/json
// API matches a member name: case, dashes and underscores ignored.
func foldsToData(name string) bool {
	return strings.EqualFold(strings.NewReplacer("-", "", "_", "").Replace(name), "data")
}

// namesDataKey is whether a constant string is, or holds as a segment between
// . [ ] / " ' or its ends, a name that folds to data.
func namesDataKey(s string) bool {
	for _, seg := range strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune(".[]/\"'", r) }) {
		if foldsToData(seg) {
			return true
		}
	}
	return false
}

// jsonBuild is one of the two encoding/json implementations the toolchain
// builds: the v2 engine (Go 1.27's default, which encoding/json/v2 shares) or
// the original one (GOEXPERIMENT=nojsonv2). They take a member name from a tag
// differently, and only the v2 engine honours an `embed` option, so a struct
// is judged by both.
type jsonBuild int

const (
	jsonOriginal jsonBuild = iota
	jsonV2Engine
)

var jsonBuilds = []jsonBuild{jsonOriginal, jsonV2Engine}

// jsonField is one member encoding/json decodes into a struct: the name it
// matches keys against, whether a tag gave that name, and its index path,
// whose first element is the struct's own field it comes from.
type jsonField struct {
	name   string
	tagged bool
	index  []int
}

// tagName is the member name build reads from a json tag, and whether the tag
// gives one; when it gives none, the field keeps its Go name.
func tagName(build jsonBuild, tag string) (string, bool) {
	if build == jsonOriginal {
		// encoding/json parseTag and isValidTag.
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			return "", false
		}
		for _, c := range name {
			if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", c) && !unicode.IsLetter(c) && !unicode.IsDigit(c) {
				return "", false
			}
		}
		return name, true
	}
	// encoding/json/v2 parseFieldOptions: the name runs to a comma, backslash
	// or quote; past any but a comma only a leading identifier is kept, and a
	// tag that does not open with one gives no name.
	if tag == "" || strings.HasPrefix(tag, ",") {
		return "", false
	}
	n := strings.IndexAny(tag, ",\\'\"`")
	if n < 0 {
		return tag, true
	}
	if tag[n] == ',' {
		return tag[:n], true
	}
	if r, _ := utf8.DecodeRuneInString(tag); r != '_' && !unicode.IsLetter(r) {
		return "", false
	}
	ident := strings.TrimLeftFunc(tag, func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsNumber(r) })
	return tag[:len(tag)-len(ident)], true
}

// embedOption is whether a json tag carries the v2 engine's `embed` option.
func embedOption(tag string) bool {
	_, opts, _ := strings.Cut(tag, ",")
	for _, opt := range strings.Split(opts, ",") {
		if opt == "embed" {
			return true
		}
	}
	return false
}

// heldStruct is the struct a field of type t holds, through an alias and one
// pointer; nil when it holds none.
func heldStruct(t types.Type) *types.Struct {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	st, _ := t.Underlying().(*types.Struct)
	return st
}

// promotes is the struct whose members build promotes through field sf, with
// json tag tag, in place of a member of its own; nil when it reads sf as one
// member.
func promotes(build jsonBuild, sf *types.Var, tag string) *types.Struct {
	if !sf.Embedded() && !sf.Exported() {
		return nil
	}
	if _, tagged := tagName(build, tag); tagged {
		return nil
	}
	if !sf.Embedded() && !(build == jsonV2Engine && embedOption(tag)) {
		return nil
	}
	return heldStruct(sf.Type())
}

// jsonFields is the member set build decodes into root, by its own field
// rules (typeFields in the original, makeStructFields in the v2 engine):
// json:"-" dropped; an unexported field dropped unless it embeds a struct; a
// promoting field explored for its struct's members; every other field kept
// under its tag name or Go name. Then for each name the shallowest member
// wins, a tagged one over an untagged one at the same depth, and two alike at
// the same depth cancel.
func jsonFields(root *types.Struct, build jsonBuild) []jsonField {
	type level struct {
		st       *types.Struct
		index    []int
		children bool // whether its own promoting fields are explored
	}
	var fields []jsonField
	seen := map[*types.Struct]bool{}
	queue := []level{{root, nil, true}}
	for q := 0; q < len(queue); q++ {
		lv := queue[q]
		for i := 0; i < lv.st.NumFields(); i++ {
			sf := lv.st.Field(i)
			tag, _ := reflect.StructTag(lv.st.Tag(i)).Lookup("json")
			if tag == "-" {
				continue
			}
			index := append(append([]int(nil), lv.index...), i)
			if st := promotes(build, sf, tag); st != nil {
				if lv.children {
					queue = append(queue, level{st, index, !seen[st]})
					seen[st] = true
				}
				continue
			}
			if !sf.Exported() && !(sf.Embedded() && heldStruct(sf.Type()) != nil) {
				continue
			}
			name, tagged := tagName(build, tag)
			if !tagged {
				name = sf.Name()
			}
			fields = append(fields, jsonField{name, tagged, index})
		}
	}
	sort.SliceStable(fields, func(i, j int) bool {
		a, b := fields[i], fields[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if len(a.index) != len(b.index) {
			return len(a.index) < len(b.index)
		}
		return a.tagged && !b.tagged
	})
	var out []jsonField
	for i := 0; i < len(fields); {
		j := i + 1
		for j < len(fields) && fields[j].name == fields[i].name {
			j++
		}
		if j-i == 1 || len(fields[i].index) != len(fields[i+1].index) || fields[i].tagged != fields[i+1].tagged {
			out = append(out, fields[i])
		}
		i = j
	}
	return out
}

// dataMembers is the indices of st's own fields from which encoding/json, in
// either build, fills a member named data.
func dataMembers(st *types.Struct) map[int]bool {
	at := map[int]bool{}
	for _, build := range jsonBuilds {
		for _, f := range jsonFields(st, build) {
			if foldsToData(f.name) {
				at[f.index[0]] = true
			}
		}
	}
	return at
}

// inModule is whether import path p is a package of module.
func inModule(p, module string) bool {
	return p == module || strings.HasPrefix(p, module+"/")
}

// externalDataStruct is whether t is, or holds through pointers, slices,
// arrays and maps, a struct declared outside module that encoding/json fills
// from the data key. A type of the module is judged where it is declared.
func externalDataStruct(t types.Type, module string) bool {
	for {
		switch x := types.Unalias(t).(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Slice:
			t = x.Elem()
		case *types.Array:
			t = x.Elem()
		case *types.Map:
			t = x.Elem()
		case *types.Named:
			if pkg := x.Obj().Pkg(); pkg == nil || inModule(pkg.Path(), module) {
				return false
			}
			st, ok := x.Underlying().(*types.Struct)
			if !ok {
				t = x.Underlying()
				continue
			}
			return len(dataMembers(st)) > 0
		case *types.Struct:
			// An unnamed struct: its fields name the package that declared it.
			if x.NumFields() == 0 {
				return false
			}
			if pkg := x.Field(0).Pkg(); pkg == nil || inModule(pkg.Path(), module) {
				return false
			}
			return len(dataMembers(x)) > 0
		default:
			return false
		}
	}
}

// dataKeyReads is every read of the data key in one source, one per line and
// declaration.
func dataKeyReads(fset *token.FileSet, src *source) []dataKeyRead {
	var found []dataKeyRead
	reported := map[dataKeyRead]bool{}
	report := func(pos token.Pos, owner, kind string) {
		line := fset.Position(pos).Line
		if key := (dataKeyRead{src.rel, line, owner, ""}); !reported[key] {
			reported[key] = true
			found = append(found, dataKeyRead{src.rel, line, owner, kind})
		}
	}
	notKey := map[token.Pos]bool{}   // a .Set key, a struct tag
	promoting := map[ast.Expr]bool{} // the type of a field whose members both builds promote
	visit := func(owner string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Set" && len(x.Args) > 0 {
					notKey[x.Args[0].Pos()] = true
				}
			case *ast.StructType:
				st, ok := src.info.Types[x].Type.(*types.Struct)
				if !ok {
					report(x.Pos(), owner, "field") // untyped: reported, never passed
					return true
				}
				members := dataMembers(st)
				i := 0
				for _, f := range x.Fields.List {
					if f.Tag != nil {
						notKey[f.Tag.Pos()] = true
					}
					n := len(f.Names)
					if n == 0 {
						n = 1
					}
					for k := i; k < i+n && k < st.NumFields(); k++ {
						if members[k] {
							report(f.Pos(), owner, "field")
						}
					}
					if len(f.Names) == 0 && i < st.NumFields() {
						tag, _ := reflect.StructTag(st.Tag(i)).Lookup("json")
						if promotes(jsonOriginal, st.Field(i), tag) != nil && promotes(jsonV2Engine, st.Field(i), tag) != nil {
							ast.Inspect(f.Type, func(m ast.Node) bool {
								if e, ok := m.(ast.Expr); ok {
									promoting[e] = true
								}
								return true
							})
						}
					}
					i += n
				}
				return true
			}
			e, ok := n.(ast.Expr)
			if !ok {
				return true
			}
			if s, ok := constKey(src.info, e); ok {
				if !notKey[e.Pos()] && namesDataKey(s) {
					report(e.Pos(), owner, "key")
				}
				return false
			}
			if tv, ok := src.info.Types[e]; ok && !promoting[e] && externalDataStruct(tv.Type, src.module) {
				report(e.Pos(), owner, "type")
			}
			return true
		})
	}
	for _, decl := range src.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			visit(d.Name.Name, d)
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue
			}
			for _, spec := range d.Specs {
				owner := ""
				switch s := spec.(type) {
				case *ast.TypeSpec:
					owner = s.Name.Name
				case *ast.ValueSpec:
					owner = s.Names[0].Name
				}
				visit(owner, spec)
			}
		}
	}
	return found
}

// moduleReads is every read of the data key in the module at root, by file
// and line.
func moduleReads(t *testing.T, root string) []dataKeyRead {
	t.Helper()
	fset, sources := loadModule(t, root)
	var reads []dataKeyRead
	for _, src := range sources {
		reads = append(reads, dataKeyReads(fset, src)...)
	}
	sort.SliceStable(reads, func(i, j int) bool {
		if reads[i].Rel != reads[j].Rel {
			return reads[i].Rel < reads[j].Rel
		}
		return reads[i].Line < reads[j].Line
	})
	return reads
}

func writeFixture(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDataKeyReadsCatchesEverySpelling runs the guard over a small module per
// case: body is x/x.go after "package x\n\n" (so its first line is line 3),
// other is other/other.go after "package other\n\n", imported as fixture/other.
func TestDataKeyReadsCatchesEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		name, body, other string
		want              []string
	}{
		{"index", "func r(top map[string]any) { _ = top[\"data\"] }", "", []string{"x/x.go:3 (r, key)"}},
		{"raw-string index", "func r(top map[string]any) { _ = top[`data`] }", "", []string{"x/x.go:3 (r, key)"}},
		{"multiline helper", "func r(top any) {\n\t_ = getv(\n\t\ttop,\n\t\t\"data\",\n\t)\n}", "", []string{"x/x.go:6 (r, key)"}},
		{"multiline index", "func r(top map[string]any) {\n\t_ = ListingRows(\n\t\ttop[\"data\"],\n\t)\n}", "", []string{"x/x.go:5 (r, key)"}},
		{"any helper", "func r(top any) { _ = lookup(top, \"data\") }", "", []string{"x/x.go:3 (r, key)"}},
		{"method Get", "func (c *cat) read(o *Obj) { _, _ = o.Get(\"data\") }", "", []string{"x/x.go:3 (read, key)"}},
		{"const", "const k = \"data\"", "", []string{"x/x.go:3 (k, key)"}},
		{"concatenation", "func r(top map[string]any) { _ = top[\"da\" + \"ta\"] }", "", []string{"x/x.go:3 (r, key)"}},
		{"named constant in a concatenation", "const d = \"da\"\n\nfunc r(top map[string]any) { _ = top[d+\"ta\"] }", "", []string{"x/x.go:5 (r, key)"}},
		{"named constant", "const k = \"data\"\n\nfunc r(top map[string]any) { _ = top[k] }", "", []string{"x/x.go:3 (k, key)", "x/x.go:5 (r, key)"}},
		{"constant from another package", "import \"fixture/other\"\n\nfunc r(top map[string]any) { _ = top[other.Key] }", "const Key = \"da\" + \"ta\"", []string{"other/other.go:3 (Key, key)", "x/x.go:5 (r, key)"}},
		{"typed conversion", "type key string\n\nfunc r(top map[key]any) { _ = top[key(\"data\")] }", "", []string{"x/x.go:5 (r, key)"}},
		{"case", "func r(k string) bool {\n\tswitch k {\n\tcase \"data\":\n\t\treturn true\n\t}\n\treturn false\n}", "", []string{"x/x.go:5 (r, key)"}},
		{"another case", "func r(top map[string]any) { _ = top[\"DATA\"] }", "", []string{"x/x.go:3 (r, key)"}},
		{"dashes and underscores", "func r(top any) { _ = lookup(top, \"da_ta\") }", "", []string{"x/x.go:3 (r, key)"}},
		{"package-level var", "var keys = []string{\"id\", \"data\"}", "", []string{"x/x.go:3 (keys, key)"}},
		{"path segment", "func r(body string) any { return query(body, \"data.0.id\") }", "", []string{"x/x.go:3 (r, key)"}},
		{"pointer segment", "func r(doc any) any { return pointer(doc, \"/data/0\") }", "", []string{"x/x.go:3 (r, key)"}},
		{"JSON text", "func r(body string) bool { return contains(body, `{\"data\":`) }", "", []string{"x/x.go:3 (r, key)"}},
		{"tagged field", "type listing struct {\n\tRows []any `json:\"data\"`\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"tag in another case", "type listing struct {\n\tRows []any `json:\"DATA,omitempty\"`\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"tag folded by case:ignore", "type listing struct {\n\tRows []any `json:\"da_ta,case:ignore\"`\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"untagged field", "func r() { var l struct{ Data []any }; _ = l }", "", []string{"x/x.go:3 (r, field)"}},
		{"a tag only the original build falls back from", "type listing struct {\n\tData []any `json:\"da'ta\"`\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"a tag only the v2 engine cuts to data", "type listing struct {\n\tRows []any `json:\"data'x\"`\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"an embed option only the v2 engine honours", "import \"testing/fstest\"\n\ntype listing struct {\n\tFile fstest.MapFile `json:\",embed\"`\n}", "", []string{"x/x.go:6 (listing, field)"}},
		{"unexported embedded struct named by its tag", "type inner struct{ Count int }\ntype Event struct {\n\tinner `json:\"data\"`\n}", "", []string{"x/x.go:5 (Event, field)"}},
		{"second of several names", "type listing struct {\n\tA, Data []any\n}", "", []string{"x/x.go:4 (listing, field)"}},
		{"embedded tagged field", "type Rows []any\ntype listing struct {\n\tRows `json:\"data\"`\n}", "", []string{"x/x.go:5 (listing, field)"}},
		{"embedded pointer tagged field", "type Rows []any\ntype listing struct {\n\t*Rows `json:\"data\"`\n}", "", []string{"x/x.go:5 (listing, field)"}},
		{"embedded generic tagged field", "type Rows[T any] []T\ntype listing struct {\n\tRows[int] `json:\"data\"`\n}", "", []string{"x/x.go:5 (listing, field)"}},
		{"embedded tagged struct", "type Data struct{ Count int }\ntype Event struct {\n\tData `json:\"data\"`\n}", "", []string{"x/x.go:5 (Event, field)"}},
		{"embedded standard-library type tagged", "import \"encoding/json\"\n\ntype listing struct {\n\tjson.RawMessage `json:\"data\"`\n}", "", []string{"x/x.go:6 (listing, field)"}},
		{"embedded untagged slice", "type Data []any\ntype listing struct {\n\tData\n}", "", []string{"x/x.go:5 (listing, field)"}},
		{"embedded untagged map", "type Data map[string]any\ntype Event struct {\n\tData\n}", "", []string{"x/x.go:5 (Event, field)"}},
		{"embedded untagged alias of a slice", "type Rows = []any\ntype Data Rows\ntype Event struct {\n\tData\n}", "", []string{"x/x.go:6 (Event, field)"}},
		{"embedded slice from another package", "import \"fixture/other\"\n\ntype Event struct {\n\tother.Data\n}", "type Data []any", []string{"x/x.go:6 (Event, field)"}},
		{"embedded map from another package, by pointer", "import \"fixture/other\"\n\ntype Event struct {\n\t*other.Data\n}", "type Data map[string]any", []string{"x/x.go:6 (Event, field)"}},
		{"promoted from another package", "import \"fixture/other\"\n\ntype Event struct {\n\tother.Envelope\n}", "type Envelope struct{ Data []any }", []string{"other/other.go:3 (Envelope, field)", "x/x.go:6 (Event, field)"}},
		{"promoted from the standard library", "import \"testing/fstest\"\n\ntype Event struct {\n\tfstest.MapFile\n}", "", []string{"x/x.go:6 (Event, field)"}},
		{"promoted through an unexported struct", "type inner struct{ Rows []any `json:\"data\"` }\ntype Event struct {\n\tinner\n}", "", []string{"x/x.go:3 (inner, field)", "x/x.go:5 (Event, field)"}},
		{"a tagged field hides a promoted one", "import \"fixture/other\"\n\ntype Event struct {\n\tother.Envelope\n\tRows []any `json:\"Data\"`\n}", "type Envelope struct{ Data []any }", []string{"other/other.go:3 (Envelope, field)", "x/x.go:7 (Event, field)"}},
		{"two promoted at one depth cancel", "import \"fixture/other\"\n\ntype Event struct {\n\tother.A\n\tother.B\n}", "type A struct{ Data []any }\ntype B struct{ Data []any }", []string{"other/other.go:3 (A, field)", "other/other.go:4 (B, field)"}},
		{"embedded struct promotes no data", "type Data struct{ Count int }\ntype Event struct{ Data }", "", nil},
		{"embedded pointer struct promotes no data", "type Data struct{ Count int }\ntype Event struct {\n\t*Data\n}", "", nil},
		{"embedded struct from another package promotes no data", "import \"fixture/other\"\n\ntype Event struct {\n\tother.Data\n}", "type Data struct{ Count int }", nil},
		{"embedded standard-library map keeps its own name", "import \"net/http\"\n\ntype Event struct {\n\thttp.Header\n}", "", nil},
		{"a standard-library struct with no data member", "import \"net/url\"\n\nfunc r() { var u url.URL; _ = u }", "", nil},
		{"decoded into a standard-library struct", "import \"testing/fstest\"\n\nfunc r(body []byte) { var f fstest.MapFile; decode(body, &f) }", "", []string{"x/x.go:5 (r, type)"}},
		{"a field of a standard-library struct type", "import \"testing/fstest\"\n\ntype Event struct {\n\tFile fstest.MapFile\n}", "", []string{"x/x.go:6 (Event, type)"}},
		{"two standard-library promotions at one depth cancel", "import \"testing/fstest\"\n\ntype A struct{ fstest.MapFile }\ntype B struct{ fstest.MapFile }\ntype Pair struct {\n\tA\n\tB\n}", "", []string{"x/x.go:5 (A, field)", "x/x.go:6 (B, field)"}},
		{"a quoted tag name neither build reads", "type listing struct {\n\tRows []any `json:\"'data'\"`\n}", "", nil},
		{"comment", "// payload[\"data\"] and getv(top, \"data\")\nfunc r() {}", "", nil},
		{"writer", "func w(top *Obj, rows []any) { top.Set(\"data\", rows) }", "", nil},
		{"tag text is not a key", "type listing struct {\n\tRows []any \"data\"\n}", "", nil},
		{"prose is not a key", "func r() string { return \"no data yet\" }", "", nil},
		{"an event-stream prefix is not a key", "const prefix = \"data: \"", "", nil},
		{"unexported field", "func r() { var l struct{ data []byte }; _ = l }", "", nil},
		{"unexported embedded slice", "type data []any\ntype Event struct {\n\tdata\n}", "", nil},
		{"json-ignored field", "type t struct {\n\tData []byte `json:\"-\"`\n}", "", nil},
		{"other keys", "func r(top map[string]any) { _, _ = top[\"datas\"], top[\"metadata\"] }", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "go.mod", "module fixture\n")
			writeFixture(t, root, "x/x.go", "package x\n\n"+tc.body+"\n")
			if tc.other != "" {
				writeFixture(t, root, "other/other.go", "package other\n\n"+tc.other+"\n")
			}
			var got []string
			for _, r := range moduleReads(t, root) {
				got = append(got, r.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDataKeyReadsJudgesTypesFromAnotherModule: a type declared outside the
// module (here example.com/ext, required through a replace) is resolved from
// its export data and judged like one declared inside it. body is x/x.go after
// its package clause and import (so its first line is line 5); ext is
// ext/ext.go after "package ext\n\n".
func TestDataKeyReadsJudgesTypesFromAnotherModule(t *testing.T) {
	for _, tc := range []struct {
		name, ext, body string
		want            []string
	}{
		{"embedded slice", "type Data []any", "type Event struct {\n\text.Data\n}", []string{"x/x.go:6 (Event, field)"}},
		{"embedded map by pointer", "type Data map[string]any", "type Event struct {\n\t*ext.Data\n}", []string{"x/x.go:6 (Event, field)"}},
		{"promoted field", "type Envelope struct{ Data []any }", "type Event struct {\n\text.Envelope\n}", []string{"x/x.go:6 (Event, field)"}},
		{"decoded directly", "type Envelope struct{ Data []any }", "func r(body []byte) { var e ext.Envelope; decode(body, &e) }", []string{"x/x.go:5 (r, type)"}},
		{"reached without naming it", "type Envelope struct{ Data []any }\ntype Pages map[string]Envelope", "func r(p ext.Pages) {\n\te := p[\"x\"]\n\t_ = e\n}", []string{"x/x.go:5 (r, type)", "x/x.go:6 (r, type)", "x/x.go:7 (r, type)"}},
		{"a type made from it", "type Envelope struct{ Data []any }", "type Listing ext.Envelope", []string{"x/x.go:5 (Listing, type)"}},
		{"a field of its type", "type Envelope struct{ Data []any }", "type Event struct {\n\tL ext.Envelope\n}", []string{"x/x.go:6 (Event, type)"}},
		{"two promotions at one depth cancel", "type A struct{ Data []any }\ntype B struct{ Data []any }", "type Event struct {\n\text.A\n\text.B\n}", nil},
		{"an unnamed struct it declares", "var Reply struct{ Data []any }", "func r(body []byte) { decode(body, &ext.Reply) }", []string{"x/x.go:5 (r, type)"}},
		{"an unnamed struct with no data member", "var Reply struct{ Count int }", "func r(body []byte) { decode(body, &ext.Reply) }", nil},
		{"embedded struct promotes no data", "type Data struct{ Count int }", "type Event struct {\n\text.Data\n}", nil},
		{"constant", "const Key = \"data\"", "func r(top map[string]any) { _ = top[ext.Key] }", []string{"x/x.go:5 (r, key)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, "go.mod", "module fixture\n\ngo 1.22\n\nrequire example.com/ext v0.0.0\n\nreplace example.com/ext => ./ext\n")
			writeFixture(t, root, "ext/go.mod", "module example.com/ext\n\ngo 1.22\n")
			writeFixture(t, root, "ext/ext.go", "package ext\n\n"+tc.ext+"\n")
			writeFixture(t, root, "x/x.go", "package x\n\nimport \"example.com/ext\"\n\n"+tc.body+"\n")
			var got []string
			for _, r := range moduleReads(t, root) {
				got = append(got, r.String())
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDataKeyReadsJudgesEveryBuild: a reader behind a build constraint this
// platform does not build is judged all the same.
func TestDataKeyReadsJudgesEveryBuild(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "go.mod", "module fixture\n")
	writeFixture(t, root, "x/x.go", "package x\n")
	writeFixture(t, root, "x/x_windows.go", "//go:build windows\n\npackage x\n\nimport \"syscall\"\n\ntype listing struct {\n\tH    syscall.Handle\n\tData []any\n}\n")
	var got []string
	for _, r := range moduleReads(t, root) {
		got = append(got, r.String())
	}
	if want := []string{"x/x_windows.go:9 (listing, field)"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestOnlyListingShapeReadsTheDataKey(t *testing.T) {
	used := map[string]bool{}
	var offenders []string
	for _, r := range moduleReads(t, "..") {
		key := r.Rel + " " + r.Owner
		if _, ok := dataKeyOwners[key]; ok {
			used[key] = true
			continue
		}
		offenders = append(offenders, r.String())
	}
	for key := range dataKeyOwners {
		if !used[key] {
			t.Errorf("exemption %q matched no read: the walk missed it, or it is stale and must go", key)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("a /v1/models reader reads the data key itself, so a 200 that is not a listing "+
			"reads as an empty one (REVIEW.md kind: a reader that turns unparseable input into a "+
			"confident verdict, #767). Read the body through ctxwindow.ListingShape: %s",
			strings.Join(offenders, "; "))
	}
}
