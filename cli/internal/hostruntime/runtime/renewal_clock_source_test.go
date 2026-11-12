package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// X-27: a machine whose clock is 10 minutes ahead went offline for its peers. The runtime's token renewers
// were built with Clock: time.Now, so renewal proofs carried the machine's clock, the server refused them
// (it accepts a proof within a minute of its own time), the tokens ran out and runtime observations stopped.
// The renewer's own default is the server's clock (enrollment.TestRenewalWorksOnAMachineTenMinutesBehind):
// no production renewer here may set another.
func TestRuntimeTokenRenewersUseTheServerClock(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			literal, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			selector, ok := literal.Type.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "RenewingTokenConfig" {
				return true
			}
			found++
			for _, element := range literal.Elts {
				if field, ok := element.(*ast.KeyValueExpr); ok {
					if key, ok := field.Key.(*ast.Ident); ok && key.Name == "Clock" {
						t.Errorf("%s: a runtime token renewer sets its own Clock; renewal proofs must use the server's clock", name)
					}
				}
			}
			return true
		})
	}
	if found == 0 {
		t.Fatal("no runtime token renewer found; the test no longer looks at the right code")
	}
}
