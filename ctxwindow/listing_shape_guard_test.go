package ctxwindow

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #767. Kind (.github/REVIEW.md): a reader that turns unparseable input into a
// confident verdict. A 200 body that is not a listing (a proxy's error
// envelope) has no data list; a reader that takes the "data" key itself reads
// it as an empty listing, and `ccmorph context` then says the model "is not on
// this route". ListingShape (#747) is the one reader of that key. This guard
// parses every non-test .go file in the module and fails, by file and line, on
// any other read of it, in two forms:
//
//   - a string constant spelling data: an index, Get, getv or getKey call, any
//     other helper, a const, on one line or across several, including a
//     concatenation of literals ("da" + "ta"). Comments are not read.
//   - an encoding/json struct field filled from data: a json tag naming data,
//     or an untagged field whose key (its name, or an embedded type's name) is
//     Data. An embedded struct is not one of them: encoding/json promotes its
//     fields, so `type Data struct{ Count int }; struct{ Data }` reads Count,
//     never a data key — only a tagged embed, or an embed of a non-struct type
//     such as `type Rows []any`, takes the key.
//
// A writer is not a reader: the first argument of a .Set call is exempt. It
// reads spellings, not run-time values: a key assembled from named constants
// is out of its reach. A type declared outside this module is not judged, so a
// shape the guard cannot see is never reported on a guess.

// dataKeyOwners are the only declarations that may read the key. Every entry
// must still match, so the list cannot hide a stale or misspelt exemption.
var dataKeyOwners = map[string]string{
	"ctxwindow/listing.go ListingShape":        "the shared verdict itself",
	"quota/balance.go openrouterBalance":       "OpenRouter's credits reply, not a /v1/models listing",
	"quota/balance.go zhipuBalance":            "Zhipu's balance reply, not a /v1/models listing",
	"util/paths/atomic.go WriteIn":             "the bytes of an atomic write, never decoded from JSON",
	"util/paths/usagestore.go UsageArchiveRow": "a usage receipt's bytes, never decoded from JSON",
}

type dataKeyRead struct {
	Line  int
	Owner string // the enclosing func, method or type; "" at package level
	Kind  string // "key" or "field"
}

// parsed is one non-test .go file with what a field's type needs: the import
// path of its package, the names its imports bind, and its syntax.
type parsed struct {
	rel        string
	importPath string
	file       *ast.File
	imports    map[string]string // the name a file refers to -> import path
}

// typeDef is a package-level type declaration and the file that declared it,
// whose imports resolve any package it names in turn.
type typeDef struct {
	src  *parsed
	expr ast.Expr
}

// moduleIndex resolves, module-wide, whether an embedded field's type is a
// struct: encoding/json promotes such a field's fields rather than reading a
// key named after the type.
type moduleIndex struct {
	types map[string]map[string]typeDef // import path -> type name -> declaration
}

func newModuleIndex(sources []*parsed) *moduleIndex {
	m := &moduleIndex{types: map[string]map[string]typeDef{}}
	for _, src := range sources {
		if m.types[src.importPath] == nil {
			m.types[src.importPath] = map[string]typeDef{}
		}
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				m.types[src.importPath][ts.Name.Name] = typeDef{src: src, expr: ts.Type}
			}
		}
	}
	return m
}

// structKind is whether expr's type is a struct (true), a type that is not one
// (false), or a type this module cannot see (known false).
func (m *moduleIndex) structKind(src *parsed, expr ast.Expr, seen map[string]bool) (isStruct, known bool) {
	switch x := expr.(type) {
	case *ast.ParenExpr:
		return m.structKind(src, x.X, seen)
	case *ast.StarExpr:
		return m.structKind(src, x.X, seen)
	case *ast.IndexExpr:
		return m.structKind(src, x.X, seen)
	case *ast.IndexListExpr:
		return m.structKind(src, x.X, seen)
	case *ast.Ident:
		def, ok := m.types[src.importPath][x.Name]
		if !ok {
			return false, false
		}
		key := src.importPath + "." + x.Name
		if seen[key] {
			return false, false
		}
		seen[key] = true
		return m.structKind(def.src, def.expr, seen)
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return false, false
		}
		importPath, ok := src.imports[pkg.Name]
		if !ok {
			return false, false
		}
		def, ok := m.types[importPath][x.Sel.Name]
		if !ok {
			return false, false
		}
		key := importPath + "." + x.Sel.Name
		if seen[key] {
			return false, false
		}
		seen[key] = true
		return m.structKind(def.src, def.expr, seen)
	case *ast.StructType:
		return true, true
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType:
		return false, true
	}
	return false, false
}

// constString folds a string literal, a parenthesised one, or a + of them.
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

// jsonKeyOf is the object key encoding/json fills a field from: the tag's name
// when it gives one, else fallback. ok is false for json:"-" (never filled).
func jsonKeyOf(f *ast.Field, fallback string) (key string, tagged, ok bool) {
	if f.Tag == nil {
		return fallback, false, true
	}
	raw, err := strconv.Unquote(f.Tag.Value)
	if err != nil {
		return fallback, false, true
	}
	tag, ok := reflect.StructTag(raw).Lookup("json")
	if !ok {
		return fallback, false, true
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return "", false, false
	}
	if name == "" {
		return fallback, false, true
	}
	return name, true, true
}

// embeddedName is the type name of a field declared without one; "" when the
// type expression names nothing embeddable.
func embeddedName(f *ast.Field) string {
	for e := f.Type; ; {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x.Name
		case *ast.SelectorExpr:
			return x.Sel.Name
		default:
			return ""
		}
	}
}

// fieldReads is every read of the data key f makes. A named field's key is its
// name or tag. An embedded field's key is its tag, or its type's name — but an
// untagged embedded struct is not a read at all: its own fields are promoted.
func (m *moduleIndex) fieldReads(fset *token.FileSet, src *parsed, f *ast.Field, owner string) []dataKeyRead {
	report := func() []dataKeyRead {
		return []dataKeyRead{{fset.Position(f.Pos()).Line, owner, "field"}}
	}
	if len(f.Names) > 0 {
		for _, n := range f.Names {
			if !ast.IsExported(n.Name) {
				continue
			}
			if key, _, ok := jsonKeyOf(f, n.Name); ok && strings.EqualFold(key, "data") {
				return report()
			}
		}
		return nil
	}
	name := embeddedName(f)
	if name == "" || !ast.IsExported(name) {
		return nil
	}
	key, tagged, ok := jsonKeyOf(f, name)
	if !ok || !strings.EqualFold(key, "data") {
		return nil
	}
	if tagged {
		return report() // the tag names the key, whatever the type is
	}
	isStruct, known := m.structKind(src, f.Type, map[string]bool{})
	if !known || isStruct {
		return nil // promoted fields, or a type this module cannot see
	}
	return report()
}

// dataKeyReads is every read of the data key in one parsed file.
func dataKeyReads(fset *token.FileSet, src *parsed, m *moduleIndex) []dataKeyRead {
	var found []dataKeyRead
	writer := map[token.Pos]bool{}
	visit := func(owner string, root ast.Node) {
		ast.Inspect(root, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Set" && len(x.Args) > 0 {
					writer[x.Args[0].Pos()] = true
				}
			case *ast.BasicLit, *ast.BinaryExpr:
				if s, ok := constString(x.(ast.Expr)); ok && s == "data" {
					if !writer[x.Pos()] {
						found = append(found, dataKeyRead{fset.Position(x.Pos()).Line, owner, "key"})
					}
					return false
				}
			case *ast.StructType:
				for _, f := range x.Fields.List {
					found = append(found, m.fieldReads(fset, src, f, owner)...)
				}
			}
			return true
		})
	}
	for _, decl := range src.file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			visit(d.Name.Name, d)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				owner := ""
				if ts, ok := spec.(*ast.TypeSpec); ok {
					owner = ts.Name.Name
				}
				visit(owner, spec)
			}
		}
	}
	return found
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

// importNames is the name each import of a file binds.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := path.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." {
			continue // binds no name the file can write
		}
		names[name] = importPath
	}
	return names
}

// parseModule parses every source of the checkout into the context the field
// resolution needs.
func parseModule(t *testing.T, root string) ([]*parsed, *moduleIndex, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	module := modulePath(t, root)
	var sources []*parsed
	for _, rel := range moduleSources(t, root) {
		file, err := parser.ParseFile(fset, filepath.Join(root, filepath.FromSlash(rel)), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		dir := path.Dir(rel)
		importPath := module
		if dir != "." {
			importPath = module + "/" + dir
		}
		sources = append(sources, &parsed{rel: rel, importPath: importPath, file: file, imports: importNames(file)})
	}
	return sources, newModuleIndex(sources), fset
}

func TestDataKeyReadsCatchesEverySpelling(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []dataKeyRead
	}{
		{"index", "func r(top map[string]any) { _ = top[\"data\"] }", []dataKeyRead{{3, "r", "key"}}},
		{"raw-string index", "func r(top map[string]any) { _ = top[`data`] }", []dataKeyRead{{3, "r", "key"}}},
		{"multiline helper", "func r(top any) {\n\t_ = getv(\n\t\ttop,\n\t\t\"data\",\n\t)\n}", []dataKeyRead{{6, "r", "key"}}},
		{"multiline index", "func r(top map[string]any) {\n\t_ = ListingRows(\n\t\ttop[\"data\"],\n\t)\n}", []dataKeyRead{{5, "r", "key"}}},
		{"any helper", "func r(top any) { _ = lookup(top, \"data\") }", []dataKeyRead{{3, "r", "key"}}},
		{"method Get", "func (c *cat) read(o *Obj) { _, _ = o.Get(\"data\") }", []dataKeyRead{{3, "read", "key"}}},
		{"const", "const k = \"data\"", []dataKeyRead{{3, "", "key"}}},
		{"concatenation", "func r(top map[string]any) { _ = top[\"da\" + \"ta\"] }", []dataKeyRead{{3, "r", "key"}}},
		{"tagged field", "type listing struct {\n\tRows []any `json:\"data\"`\n}", []dataKeyRead{{4, "listing", "field"}}},
		{"untagged field", "func r() { var l struct{ Data []any }; _ = l }", []dataKeyRead{{3, "r", "field"}}},
		{"embedded tagged field", "type Rows []any\ntype listing struct {\n\tRows `json:\"data\"`\n}", []dataKeyRead{{5, "listing", "field"}}},
		{"embedded pointer tagged field", "type Rows []any\ntype listing struct {\n\t*Rows `json:\"data\"`\n}", []dataKeyRead{{5, "listing", "field"}}},
		{"embedded untagged slice", "type Data []any\ntype listing struct {\n\tData\n}", []dataKeyRead{{5, "listing", "field"}}},
		{"embedded untagged map", "type Data map[string]any\ntype Event struct {\n\tData\n}", []dataKeyRead{{5, "Event", "field"}}},
		{"embedded untagged alias of a slice", "type Rows = []any\ntype Data Rows\ntype Event struct {\n\tData\n}", []dataKeyRead{{6, "Event", "field"}}},
		{"embedded tagged struct is a read", "type Data struct{ Count int }\ntype Event struct {\n\tData `json:\"data\"`\n}", []dataKeyRead{{5, "Event", "field"}}},
		{"embedded selector tagged field", "type listing struct {\n\tjson.RawMessage `json:\"data\"`\n}", []dataKeyRead{{4, "listing", "field"}}},
		{"second of several names", "type listing struct {\n\tA, Data []any\n}", []dataKeyRead{{4, "listing", "field"}}},
		{"embedded generic tagged field", "type Rows[T any] []T\ntype listing struct {\n\tRows[int] `json:\"data\"`\n}", []dataKeyRead{{5, "listing", "field"}}},
		{"embedded struct promotes its fields", "type Data struct{ Count int }\ntype Event struct{ Data }", nil},
		{"embedded pointer struct promotes its fields", "type Data struct{ Count int }\ntype Event struct {\n\t*Data\n}", nil},
		{"embedded external type is not judged", "type Event struct {\n\tother.Data\n}", nil},
		{"comment", "// payload[\"data\"] and getv(top, \"data\")\nfunc r() {}", nil},
		{"writer", "func w(top *Obj, rows []any) { top.Set(\"data\", rows) }", nil},
		{"unexported field", "func r() { var l struct{ data []byte }; _ = l }", nil},
		{"unexported embedded slice", "type data []any\ntype Event struct {\n\tdata\n}", nil},
		{"json-ignored field", "type t struct {\n\tData []byte `json:\"-\"`\n}", nil},
		{"other key", "func r(top map[string]any) { _ = top[\"datas\"] }", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, "x.go", "package x\n\n"+tc.body+"\n", parser.ParseComments)
			if err != nil {
				t.Fatal(err)
			}
			src := &parsed{rel: "x.go", importPath: "x", file: file, imports: importNames(file)}
			got := dataKeyReads(fset, src, newModuleIndex([]*parsed{src}))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestOnlyListingShapeReadsTheDataKey(t *testing.T) {
	root := ".."
	sources, index, fset := parseModule(t, root)
	used := map[string]bool{}
	var offenders []string
	for _, src := range sources {
		for _, r := range dataKeyReads(fset, src, index) {
			key := src.rel + " " + r.Owner
			if _, ok := dataKeyOwners[key]; ok {
				used[key] = true
				continue
			}
			offenders = append(offenders, src.rel+":"+strconv.Itoa(r.Line)+" ("+r.Owner+", "+r.Kind+")")
		}
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
