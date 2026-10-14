package workspace

import (
	"context"
	"testing"
)

func TestInventoryCursorBindsEveryFilter(t *testing.T) {
	base := "account\x1fpersonal"
	ctx := context.Background()
	if InventoryCursorPrincipal(ctx, base) != base {
		t.Fatal("empty filter principal changed")
	}
	seen := map[string]bool{}
	for _, filter := range []InventoryFilters{{Q: "one"}, {Q: "two"}, {State: "active"}, {State: "stopped"}, {Owner: "mine"}, {Owner: "shared"}} {
		principal := InventoryCursorPrincipal(WithInventoryFilters(ctx, filter), base)
		if principal == base || seen[principal] {
			t.Fatal("distinct filter shares cursor principal")
		}
		seen[principal] = true
	}
}
