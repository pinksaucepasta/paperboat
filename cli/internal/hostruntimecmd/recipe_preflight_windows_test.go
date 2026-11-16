//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"testing"
)

// A recipe install that is not elevated stops before the recipe session (victus, standard user).
func TestRecipeInstallPreflightRefusesWithoutElevation(t *testing.T) {
	old := recipeProcessElevated
	t.Cleanup(func() { recipeProcessElevated = old })
	recipeProcessElevated = func() bool { return false }
	if err := RecipeInstallPreflight(context.Background()); !errors.Is(err, errRecipeNeedsRoot) {
		t.Fatalf("err = %v", err)
	}
	recipeProcessElevated = func() bool { return true }
	if err := RecipeInstallPreflight(context.Background()); err != nil {
		t.Fatalf("elevated: %v", err)
	}
}
