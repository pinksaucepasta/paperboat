package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// InventoryFilters selects authorized resources before the cursor page is read.
type InventoryFilters struct{ Q, State, Owner, MachineID string }
type inventoryFiltersKey struct{}

func WithInventoryFilters(ctx context.Context, filters InventoryFilters) context.Context {
	return context.WithValue(ctx, inventoryFiltersKey{}, filters)
}
func InventoryFiltersFromContext(ctx context.Context) InventoryFilters {
	filters, _ := ctx.Value(inventoryFiltersKey{}).(InventoryFilters)
	return filters
}
func InventoryCursorPrincipal(ctx context.Context, principal string) string {
	filters := InventoryFiltersFromContext(ctx)
	if filters == (InventoryFilters{}) {
		return principal
	}
	sum := sha256.Sum256([]byte(filters.Q + "\x00" + filters.State + "\x00" + filters.Owner + "\x00" + filters.MachineID))
	return principal + "\x1e" + hex.EncodeToString(sum[:])
}
