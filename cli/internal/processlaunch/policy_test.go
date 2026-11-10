package processlaunch

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every process start in code that builds for Windows must hide its window:
//   - exec.Command/CommandContext: assigned to a variable passed to ConfigureBackground in the same function;
//   - windows.CreateProcess*: a "hidden:" comment on or above the call naming where the flags are set;
//   - or a "user-console:" comment on or above that exact call (the user's own terminal shows it), or
//     "other-os:" (the call only runs on another OS).
func TestWindowsProcessStartsAreHiddenPerCallSite(t *testing.T) {
	root := filepath.Join("..", "..")
	context := build.Default
	context.GOOS, context.GOARCH, context.CgoEnabled = "windows", "amd64", false
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" || path == filepath.Join(root, "tools") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if ok, _ := context.MatchFile(filepath.Dir(path), entry.Name()); !ok {
			return nil
		}
		checkFile(t, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func checkFile(t *testing.T, path string) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	marked := map[int]bool{} // lines carrying a marker comment
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if strings.Contains(comment.Text, "user-console:") || strings.Contains(comment.Text, "hidden:") || strings.Contains(comment.Text, "other-os:") {
				marked[fset.Position(comment.End()).Line] = true
			}
		}
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil {
			continue
		}
		configured := configuredNames(function.Body)
		ast.Inspect(function.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			kind := startKind(call)
			if kind == "" {
				return true
			}
			line := fset.Position(call.Pos()).Line
			if marked[line] || marked[line-1] {
				return true
			}
			if kind == "exec" && assignedTo(function.Body, call, configured) {
				return true
			}
			t.Errorf("%s:%d starts a process without processlaunch.ConfigureBackground(<that command>) or a hidden:/user-console:/other-os: comment on the call", path, line)
			return true
		})
	}
}

func startKind(call *ast.CallExpr) string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := selector.X.(*ast.Ident); ok {
		if pkg.Name == "exec" && (selector.Sel.Name == "Command" || selector.Sel.Name == "CommandContext") {
			return "exec"
		}
		if (pkg.Name == "windows" || pkg.Name == "syscall") && strings.HasPrefix(selector.Sel.Name, "CreateProcess") {
			return "create"
		}
		if (pkg.Name == "os" || pkg.Name == "syscall") && selector.Sel.Name == "StartProcess" {
			return "create"
		}
	}
	return ""
}

// configuredNames collects the identifiers (and exec calls) passed to ConfigureBackground in a function body.
func configuredNames(body *ast.BlockStmt) map[string]bool {
	names := map[string]bool{}
	ast.Inspect(body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "ConfigureBackground" {
			if name, ok := call.Args[0].(*ast.Ident); ok {
				names[name.Name] = true
			}
		}
		return true
	})
	return names
}

// assignedTo reports whether call is the right-hand side of `name := call` / `name = call` with name configured,
// or is itself the argument of ConfigureBackground.
func assignedTo(body *ast.BlockStmt, call *ast.CallExpr, configured map[string]bool) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.AssignStmt:
			for index, right := range value.Rhs {
				if right == call && index < len(value.Lhs) {
					if name, ok := value.Lhs[index].(*ast.Ident); ok && configured[name.Name] {
						found = true
					}
				}
			}
		case *ast.CallExpr:
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "ConfigureBackground" && len(value.Args) == 1 && value.Args[0] == call {
				found = true
			}
		}
		return !found
	})
	return found
}
