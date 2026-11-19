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

// `defer store.Close()` runs when the test function returns, before every t.Cleanup. A cleanup that deletes the
// fixture's rows through the same store then hits a closed pool, the error is swallowed, and the next -count iteration
// or test of the package collides with the leftovers (operator: 3 tests failed 19 of 20 for this). The store is closed
// by the first t.Cleanup instead: cleanups run last-in first-out, so it runs after all the others.
//
// A heuristic on one function body: it flags `defer V.Close()` when a t.Cleanup literal of the same function mentions V.
// It does not see a cleanup registered inside a helper that was handed the store (seedX(t, store)), a pool reached
// through another variable (db := store.SQL()), or a close inside a nested function literal.
func TestDeferredCloseDoesNotPrecedeCleanupsThatUseThePool(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				for _, bad := range deferredClosesUsedByCleanups(fn.Body) {
					t.Errorf("%s: defer %s.Close() runs before the t.Cleanup that uses %s; close it from the first t.Cleanup", fset.Position(bad.Pos()), bad.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name, bad.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// deferredClosesUsedByCleanups returns the `defer V.Close()` calls made directly in body (not inside a nested function
// literal) whose V is mentioned by a t.Cleanup literal of the same body.
func deferredClosesUsedByCleanups(body *ast.BlockStmt) []*ast.CallExpr {
	var deferred []*ast.CallExpr
	var cleanups []*ast.FuncLit
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false // a nested literal is its own scope; cleanup literals are found from their call below
		case *ast.DeferStmt:
			if sel, ok := n.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Close" && len(n.Call.Args) == 0 {
				if _, ok := sel.X.(*ast.Ident); ok {
					deferred = append(deferred, n.Call)
				}
			}
		case *ast.CallExpr:
			if len(n.Args) == 1 && isSelector(n.Fun, "Cleanup") && isTestingT(n.Fun.(*ast.SelectorExpr).X) {
				if lit, ok := n.Args[0].(*ast.FuncLit); ok {
					cleanups = append(cleanups, lit)
				}
			}
		}
		return true
	})
	var bad []*ast.CallExpr
	for _, call := range deferred {
		name := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name
		for _, lit := range cleanups {
			if mentions(lit, name) {
				bad = append(bad, call)
				break
			}
		}
	}
	return bad
}

func mentions(lit *ast.FuncLit, name string) bool {
	found := false
	ast.Inspect(lit, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
