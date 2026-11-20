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
// A heuristic on one function body: it flags `defer V.Close()` when a t.Cleanup literal of the same function mentions V,
// or when the function passes V (or an expression built from V: store.SQL()) to a same-package test helper whose own
// t.Cleanup literal mentions that parameter. It looks one helper deep. It does not see a pool reached through another
// variable inside the helper (db := store.SQL()), a pool held by a receiver, or a close inside a nested function literal.
// A cleanup inside a t.Run literal belongs to the subtest's t and runs before the outer defer, so it is not flagged.
func TestDeferredCloseDoesNotPrecedeCleanupsThatUseThePool(t *testing.T) {
	fset := token.NewFileSet()
	files := map[string][]*ast.File{}
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files[filepath.Dir(path)] = append(files[filepath.Dir(path)], file)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range files {
		helpers := funcsByName(pkg)
		for _, file := range pkg {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
					for _, bad := range deferredClosesUsedByCleanups(fn.Body, helpers) {
						name := bad.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name
						t.Errorf("%s: defer %s.Close() runs before the t.Cleanup that uses %s; close it from the first t.Cleanup", fset.Position(bad.Pos()), name, name)
					}
				}
			}
		}
	}
}

// The guard has to flag both shapes and none of the safe ones.
func TestDeferredCloseGuardShapes(t *testing.T) {
	const src = `package p
func direct(t *T)  { defer store.Close(); t.Cleanup(func() { store.X() }) }
func viaHelper(t *T) { defer store.Close(); seed(t, 1, store) }
func derived(t *T)   { defer store.Close(); seed(t, 1, store.SQL()) }
func method(t *T)    { defer store.Close(); f.insert(t, store) }
func cancelOnly(t *T) { defer store.Close(); watch(t, store) }
func subtest(t *T)   { defer store.Close(); inSubtest(t, store) }
func unrelated(t *T) { defer store.Close(); seed(t, 1, other) }
func seed(t *T, n int, pool P)         { t.Cleanup(func() { pool.Delete() }) }
func (f F) insert(t *T, pool P)        { t.Cleanup(func() { pool.Delete() }) }
func watch(t *T, pool P)               { t.Cleanup(func() { cancel() }) }
func inSubtest(t *T, pool P)           { t.Run("x", func(t *T) { t.Cleanup(func() { pool.Delete() }) }) }
`
	file, err := parser.ParseFile(token.NewFileSet(), "shapes.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	helpers := funcsByName([]*ast.File{file})
	want := map[string]bool{"direct": true, "viaHelper": true, "derived": true, "method": true}
	for _, decl := range file.Decls {
		fn := decl.(*ast.FuncDecl)
		if got := len(deferredClosesUsedByCleanups(fn.Body, helpers)) > 0; got != want[fn.Name.Name] {
			t.Errorf("%s flagged = %v, want %v", fn.Name.Name, got, want[fn.Name.Name])
		}
	}
}

func funcsByName(files []*ast.File) map[string][]*ast.FuncDecl {
	out := map[string][]*ast.FuncDecl{}
	for _, file := range files {
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				out[fn.Name.Name] = append(out[fn.Name.Name], fn)
			}
		}
	}
	return out
}

// scanBody returns the `defer V.Close()` calls, the t.Cleanup literals and the other calls made directly in body (not
// inside a nested function literal: a literal is its own scope, and cleanup literals are found from their call).
func scanBody(body *ast.BlockStmt) (deferred []*ast.CallExpr, cleanups []*ast.FuncLit, calls []*ast.CallExpr) {
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
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
			} else {
				calls = append(calls, n)
			}
		}
		return true
	})
	return deferred, cleanups, calls
}

// deferredClosesUsedByCleanups returns the `defer V.Close()` calls made directly in body whose V is mentioned by a
// t.Cleanup literal of the same body, or passed to a same-package helper whose t.Cleanup literal mentions the parameter.
func deferredClosesUsedByCleanups(body *ast.BlockStmt, helpers map[string][]*ast.FuncDecl) []*ast.CallExpr {
	deferred, cleanups, calls := scanBody(body)
	var bad []*ast.CallExpr
	for _, call := range deferred {
		name := call.Fun.(*ast.SelectorExpr).X.(*ast.Ident).Name
		if anyMentions(cleanups, name) || helperCleanupUses(calls, helpers, name) {
			bad = append(bad, call)
		}
	}
	return bad
}

// helperCleanupUses reports whether one of calls hands name (or an expression mentioning it) to a helper as a
// parameter that the helper's own t.Cleanup literal mentions.
func helperCleanupUses(calls []*ast.CallExpr, helpers map[string][]*ast.FuncDecl, name string) bool {
	for _, call := range calls {
		var callee string
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			callee = fun.Name
		case *ast.SelectorExpr:
			callee = fun.Sel.Name
		}
		for _, helper := range helpers[callee] {
			_, cleanups, _ := scanBody(helper.Body)
			if len(cleanups) == 0 {
				continue
			}
			var params []string
			for _, field := range helper.Type.Params.List {
				for _, id := range field.Names {
					params = append(params, id.Name)
				}
			}
			for i, arg := range call.Args {
				if i < len(params) && mentionsExpr(arg, name) && anyMentions(cleanups, params[i]) {
					return true
				}
			}
		}
	}
	return false
}

func anyMentions(lits []*ast.FuncLit, name string) bool {
	for _, lit := range lits {
		if mentionsExpr(lit, name) {
			return true
		}
	}
	return false
}

func mentionsExpr(expr ast.Node, name string) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		if id, ok := node.(*ast.Ident); ok && id.Name == name {
			found = true
		}
		return !found
	})
	return found
}
