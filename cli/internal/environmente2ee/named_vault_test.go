package environmente2ee

import (
	"bytes"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// A named vault key wrapped for the account and its team opens with the account's keys, and is
// re-wrapped for a recipe so that only the recipe's token opens it (F26-04).
func TestNamedVaultWrapsOpenAndRewrapForARecipe(t *testing.T) {
	keys, err := NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	teamKey, _ := recipe.NewVaultKey()
	keys.Teams = []VaultTeamKey{{TeamID: "acme", Epoch: 2, Key: teamKey}}
	key, _ := recipe.NewVaultKey()
	wraps, err := NamedVaultWraps(&keys, "acct_1", "acme", "canary-env", key)
	if err != nil || len(wraps) != 2 || wraps[0].Kind != "account" || wraps[1].Kind != "team" {
		t.Fatalf("wraps %v %v", wraps, err)
	}
	if got, err := OpenNamedVaultKey(&keys, "acct_1", "canary-env", wraps[1:]); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("team wrap: %v", err)
	}
	if got, err := OpenNamedVaultKey(&keys, "acct_1", "canary-env", wraps[:1]); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("account wrap: %v", err)
	}
	if _, err := OpenNamedVaultKey(&keys, "acct_2", "canary-env", wraps[:1]); err == nil {
		t.Fatal("another account's wrap opened")
	}
	token, _ := recipe.NewToken("")
	forRecipe, err := WrapNamedVaultForRecipe(&keys, "acct_1", "canary-env", wraps, token.PublicKey(), token.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := recipe.UnwrapWithPrivate(token.PrivateKey(), recipe.RecipeVaultInfo("canary-env", token.ID), forRecipe); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("recipe unwrap: %v", err)
	}
	if _, err := NamedVaultWraps(&keys, "acct_1", "other-team", "canary-env", key); err == nil {
		t.Fatal("a team without a key on this computer was wrapped for")
	}
}
