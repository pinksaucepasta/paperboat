package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/environmente2ee"
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

// The browser lists, sets and removes a named vault's variables with the CLI's sealing: the CLI's own
// OpenValues reads what the browser sealed, and a wrong epoch or a bad name is refused.
func TestBrowserNamedVaultValues(t *testing.T) {
	e, v := initialized(t)
	out, err := e.run(context.Background(), request{Action: "named-vault-wraps", Vault: v, Name: "canary-env"})
	if err != nil {
		t.Fatal(err)
	}
	wraps := out.(map[string]any)["wraps"].([]environmente2ee.NamedVaultWrap)
	call := func(r request) map[string]any {
		t.Helper()
		r.Action, r.Vault, r.Name, r.VaultWraps, r.KeyEpoch = "named-vault-values", v, "canary-env", wraps, 1
		out, err := e.run(context.Background(), r)
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	empty := call(request{})
	if len(empty["names"].([]string)) != 0 || empty["envelope"] != nil {
		t.Fatalf("a new vault has values: %v", empty)
	}
	set := call(request{Variable: "TOKEN", Value: "s3cret"})
	envelope := set["envelope"].([]byte)
	set = call(request{ValuesEnvelope: envelope, Variable: "ALPHA", Value: "x"})
	if names := set["names"].([]string); len(names) != 2 || names[0] != "ALPHA" || names[1] != "TOKEN" {
		t.Fatalf("names = %v", names)
	}
	key, err := environmente2ee.OpenNamedVaultKey(&e.keys, e.head.AccountID, "canary-env", wraps)
	if err != nil {
		t.Fatal(err)
	}
	values, err := recipe.OpenValues(key, "canary-env", 1, set["envelope"].([]byte))
	if err != nil || values["TOKEN"] != "s3cret" || values["ALPHA"] != "x" {
		t.Fatalf("values = %v, %v", values, err)
	}
	removed := call(request{ValuesEnvelope: set["envelope"].([]byte), Variable: "TOKEN", Delete: true})
	if names := removed["names"].([]string); len(names) != 1 || names[0] != "ALPHA" {
		t.Fatalf("names after delete = %v", names)
	}
	if _, err := e.run(context.Background(), request{Action: "named-vault-values", Vault: v, Name: "canary-env", VaultWraps: wraps, KeyEpoch: 2, ValuesEnvelope: envelope}); err == nil {
		t.Fatal("an envelope of another key epoch opened")
	}
	if _, err := e.run(context.Background(), request{Action: "named-vault-values", Vault: v, Name: "canary-env", VaultWraps: wraps, KeyEpoch: 1, Variable: "1BAD"}); err == nil {
		t.Fatal("a bad variable name was accepted")
	}
}
