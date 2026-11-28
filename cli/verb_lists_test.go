package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The verb lists keep different facts (verbArity: how many words; ownVerbs:
// never forwarded to Claude Code; retiredVerbs: answer with the successor),
// so they stay apart; this pins them to the one set of words dispatch answers.
func TestDispatchAnswersExactlyTheListedVerbs(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	cased := map[string]bool{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if c, ok := n.(*ast.CaseClause); ok {
				for _, e := range c.List {
					if lit, ok := e.(*ast.BasicLit); ok {
						w, _ := strconv.Unquote(lit.Value)
						if w != "" && !strings.HasPrefix(w, "_") && !strings.HasPrefix(w, "-") {
							cased[w] = true
						}
					}
				}
			}
			return true
		})
	}
	if len(cased) == 0 {
		t.Fatal("found no dispatch cases")
	}
	known := slices.Concat(realVerbs, retiredVerbs)
	for w := range cased {
		if !slices.Contains(known, w) {
			t.Errorf("dispatch answers %q, which realVerbs and retiredVerbs do not list", w)
		}
	}
	for _, w := range known {
		if w != retiredVerb && !cased[w] {
			t.Errorf("%q is listed but dispatch has no case for it", w)
		}
	}
	for w := range ownVerbs {
		if !slices.Contains(known, w) {
			t.Errorf("ownVerbs reserves %q, which no list answers to", w)
		}
	}
}
