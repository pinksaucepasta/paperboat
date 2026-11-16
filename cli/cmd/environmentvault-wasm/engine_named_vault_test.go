package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// The browser makes a vault's wraps and wraps its key for a recipe with the CLI's code: the recipe's
// token opens what the browser wrapped (F26, console Recipes page).
func TestBrowserNamedVaultWrapsForARecipe(t *testing.T) {
	e, v := initialized(t)
	out, err := e.run(context.Background(), request{Action: "named-vault-wraps", Vault: v, Name: "canary-env"})
	if err != nil {
		t.Fatal(err)
	}
	wraps := out.(map[string]any)["wraps"].([]environmente2ee.NamedVaultWrap)
	token, _ := recipe.NewToken("")
	out, err = e.run(context.Background(), request{Action: "named-vault-recipe-wrap", Vault: v, Name: "canary-env", VaultWraps: wraps, RecipePublicKey: token.PublicKey(), TokenID: token.ID})
	if err != nil {
		t.Fatal(err)
	}
	wrapped := out.(map[string]any)["wrapped"].([]byte)
	fromRecipe, err := recipe.UnwrapWithPrivate(token.PrivateKey(), recipe.RecipeVaultInfo("canary-env", token.ID), wrapped)
	if err != nil {
		t.Fatal(err)
	}
	fromAccount, err := environmente2ee.OpenNamedVaultKey(&e.keys, e.head.AccountID, "canary-env", wraps)
	if err != nil || !bytes.Equal(fromRecipe, fromAccount) {
		t.Fatalf("keys differ: %v", err)
	}
	if _, err := e.run(context.Background(), request{Action: "named-vault-recipe-wrap", Vault: v, Name: "canary-env", VaultWraps: wraps, RecipePublicKey: []byte{1}, TokenID: token.ID}); err == nil {
		t.Fatal("a short public key was accepted")
	}
}
