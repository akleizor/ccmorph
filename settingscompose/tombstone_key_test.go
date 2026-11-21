package settingscompose

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A test that writes the tombstone key as a string literal can name a key
// tombstones() never reads, and then asserts nothing: the hooks guard test
// shipped with the old spelling for years (#740). The only accepted spelling is
// the unsetKey constant, so this gate reads every string literal in every test
// file in the package.
func TestTombstoneKeyIsOnlySpelledWithTheConstant(t *testing.T) {
	// Split, so this file's own source does not trip the check it runs.
	legacy := "__" + "unset__"
	paths, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no test files found")
	}
	fset := token.NewFileSet()
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, key := range []string{unsetKey, legacy} {
				if strings.Contains(s, key) {
					t.Errorf("%s writes the tombstone key %q as a string literal; use unsetKey",
						fset.Position(lit.Pos()), key)
				}
			}
			return true
		})
	}
}
