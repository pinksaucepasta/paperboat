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

// A cold database applies ~470 migrations, minutes under -race on a loaded host. A test that migrates with the same
// context it made for its body (`ctx, cancel := context.WithTimeout(...)` then db.Migrate(ctx, store)) lets that cost
// eat the body's budget, and the first test of every package failed "run migrations: context deadline exceeded" 1 run
// in 20. Migrate with context.Background() (go test -timeout bounds it) or a context used for nothing else.
//
// A heuristic on one function body: it flags db.Migrate(V, ...) when V is assigned from context.WithTimeout/WithDeadline
// in the same function and V is referenced anywhere else (by name, so a shadowing variable in a nested literal counts as
// the same one). It does not see a context handed in as a parameter, built by a helper, or derived through
// context.WithCancel(parent).
func TestMigrateDoesNotShareATestDeadline(t *testing.T) {
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
				for _, bad := range migratesWithASharedDeadline(fn.Body) {
					t.Errorf("%s: db.Migrate uses a deadline context that the rest of the function also uses; pass context.Background()", fset.Position(bad.Pos()))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigrateDeadlineGuardShapes(t *testing.T) {
	shared := `package p
func a() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {}
	run(ctx)
}`
	migrationOnly := `package p
func a() {
	setup, cancel := context.WithTimeout(context.Background(), time.Minute)
	err := db.Migrate(setup, store)
	cancel()
	ctx, stop := context.WithTimeout(context.Background(), time.Minute)
	run(ctx)
}`
	background := `package p
func a() {
	db.Migrate(context.Background(), store)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	run(ctx)
}`
	for name, tc := range map[string]struct {
		src  string
		want int
	}{"shared": {shared, 1}, "migration only": {migrationOnly, 0}, "background": {background, 0}} {
		file, err := parser.ParseFile(token.NewFileSet(), "x.go", tc.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(migratesWithASharedDeadline(file.Decls[0].(*ast.FuncDecl).Body)); got != tc.want {
			t.Errorf("%s: flagged %d, want %d", name, got, tc.want)
		}
	}
}

// migratesWithASharedDeadline returns the db.Migrate calls in body whose first argument is a context variable made by
// context.WithTimeout/WithDeadline and mentioned anywhere else in body.
func migratesWithASharedDeadline(body *ast.BlockStmt) []*ast.CallExpr {
	deadlines := map[string]bool{}
	uses := map[string]int{}
	var migrates []*ast.CallExpr
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Ident:
			uses[n.Name]++
		case *ast.AssignStmt:
			if len(n.Rhs) == 1 && len(n.Lhs) > 0 && isDeadlineContext(n.Rhs[0]) {
				if id, ok := n.Lhs[0].(*ast.Ident); ok {
					deadlines[id.Name] = true
				}
			}
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Migrate" && len(n.Args) > 0 && isIdent(sel.X, "db") {
				migrates = append(migrates, n)
			}
		}
		return true
	})
	var bad []*ast.CallExpr
	for _, call := range migrates {
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok || !deadlines[arg.Name] {
			continue
		}
		migrateUses := 0 // the argument itself, and the variable's own assignment target, are not sharing
		for _, other := range migrates {
			if id, ok := other.Args[0].(*ast.Ident); ok && id.Name == arg.Name {
				migrateUses++
			}
		}
		if uses[arg.Name] > 1+migrateUses {
			bad = append(bad, call)
		}
	}
	return bad
}

func isDeadlineContext(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	return ok && isIdent(sel.X, "context") && strings.HasPrefix(sel.Sel.Name, "With") &&
		(strings.Contains(sel.Sel.Name, "Timeout") || strings.Contains(sel.Sel.Name, "Deadline"))
}

func isIdent(expr ast.Expr, name string) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == name
}
