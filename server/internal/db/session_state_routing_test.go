package db

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Rule: session state (a session advisory lock, LISTEN, UNLISTEN) runs on the direct pool (DB.Direct). A function
// that holds session state and also reaches the pooled handle (Pool or SQL) can run half of the session through
// a transaction pooler, where the unlock or the LISTEN lands on another backend. Such a function fails this test.
// Functions that take their pool as a parameter are checked where the pool is passed in (app wiring uses Direct).
func TestSessionStateUsesDirectPoolNotPooledHandle(t *testing.T) {
	sessionSQL := regexp.MustCompile(`(?i)pg_(try_)?advisory_(un)?lock\(|\b(LISTEN|UNLISTEN)\s`)
	fset := token.NewFileSet()
	var violations []string
	sites := 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			session, pooled := false, false
			ast.Inspect(fn, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.BasicLit:
					if value, err := strconv.Unquote(x.Value); err == nil && sessionSQL.MatchString(value) {
						session = true
					}
				case *ast.SelectorExpr:
					pooled = pooled || x.Sel.Name == "Pool" || x.Sel.Name == "SQL"
				}
				return true
			})
			if !session {
				continue
			}
			sites++
			if pooled {
				violations = append(violations, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scan must see the known session-state sites, or the rule is checking nothing.
	if sites < 5 {
		t.Fatalf("found %d session-state functions, want at least 5 (the scan is broken)", sites)
	}
	if len(violations) > 0 {
		t.Fatalf("rule: session advisory locks and LISTEN must run on the direct pool (db.DB.Direct), not the pooled handle (Pool/SQL); violations:\n%s",
			strings.Join(violations, "\n"))
	}
}
