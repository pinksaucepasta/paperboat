package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// parallelAllowed names the only tests of this package that may call t.Parallel. Many other tests swap package
// variables (hooks such as loginHTTPClient and tunnelClientForCommand, restored in t.Cleanup) that the code under
// test reads, so a test running beside them would race those swaps. A test that needs different waits or deadlines
// takes them from its own context (commandTimings, withTimings) instead.
var parallelAllowed = []string{
	"TestMachineBlipTimingBelongsToEachRun",
	"TestSSHDialWaitsPastTheFirstStreamOpenTimeoutForANewPeer",
}

// parallelTests returns the top-level tests in source that call t.Parallel.
func parallelTests(t *testing.T, name, source string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || !strings.HasPrefix(function.Name.Name, "Test") {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Parallel" {
					found = append(found, function.Name.Name)
					return false
				}
			}
			return true
		})
	}
	return found
}

func TestParallelTestsAreTheAllowlistedOnes(t *testing.T) {
	synthetic := parallelTests(t, "scratch_test.go", "package main\nimport \"testing\"\nfunc TestScratch(t *testing.T) { t.Parallel() }\n")
	if !slices.Equal(synthetic, []string{"TestScratch"}) || slices.Contains(parallelAllowed, "TestScratch") {
		t.Fatalf("the scan missed a parallel test: %v", synthetic)
	}
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files found: %v", err)
	}
	for _, file := range files {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range parallelTests(t, file, string(source)) {
			if !slices.Contains(parallelAllowed, name) {
				t.Errorf("%s: %s calls t.Parallel, but other tests in this package swap package-level hook variables "+
					"(loginHTTPClient, tunnelClientForCommand, ...) that it may read: run it sequentially, or give it "+
					"its own state (commandTimings through withTimings) and add it to parallelAllowed", file, name)
			}
		}
	}
}
