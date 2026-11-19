package testutil

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// testing.T.Context is canceled just before Cleanup functions run, so a database statement in a cleanup that uses it
// fails at once and the error is swallowed: the fixture's rows stay behind and the next iteration of -count, or the
// next test of the package, collides with them on a unique key. Cleanups use context.Background().
func TestCleanupsDoNotUseTheCanceledTestContext(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) != 1 || !isSelector(call.Fun, "Cleanup") {
				return true
			}
			body, ok := call.Args[0].(*ast.FuncLit)
			if !ok {
				return true
			}
			ast.Inspect(body, func(inner ast.Node) bool {
				if c, ok := inner.(*ast.CallExpr); ok && len(c.Args) == 0 && isSelector(c.Fun, "Context") && isTestingT(c.Fun.(*ast.SelectorExpr).X) {
					t.Errorf("%s: t.Context() inside t.Cleanup is already canceled; use context.Background()", fset.Position(c.Pos()))
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func isSelector(expr ast.Expr, name string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == name
}

// isTestingT matches t and a field named t (n.t.Context()), the repository's spelling of *testing.T.
func isTestingT(expr ast.Expr) bool {
	switch x := expr.(type) {
	case *ast.Ident:
		return x.Name == "t"
	case *ast.SelectorExpr:
		return x.Sel.Name == "t"
	}
	return false
}
