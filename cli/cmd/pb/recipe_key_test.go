package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
	"github.com/spf13/cobra"
)

// A machine paired from a recipe opens the vaults the recipe reads with the saved recipe key, with
// nobody online, and the runtime's own values win over a vault value of the same name (F26-04).
func TestRecipeMachineReadsItsVaultValues(t *testing.T) {
	token, _ := recipe.NewToken("")
	key, _ := recipe.NewVaultKey()
	wrapped, err := recipe.WrapToPublic(token.PublicKey(), recipe.RecipeVaultInfo("canary-env", token.ID), key)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := recipe.SealValues(key, "canary-env", 1, map[string]string{"API_KEY": "s3cret", "HOME_URL": "vault"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/recipe-machine" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		writeAPIData(t, w, map[string]any{"id": "recipe_1", "name": "ci", "revision": 1, "recipe": map[string]any{},
			"vaults": []map[string]any{{"name": "canary-env", "key_epoch": 1, "revision": 1, "envelope": envelope, "wrapped": wrapped}}})
	}))
	defer srv.Close()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	writeTestProfile(t, root, path, srv.URL)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRecipeKey(cfg, token); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.Flags().String("config", path, "")
	command.SetContext(context.Background())
	values, err := withRecipeVaultValues(context.Background(), command, []string{"HOME_URL=runtime"})
	slices.Sort(values)
	if err != nil || !slices.Equal(values, []string{"API_KEY=s3cret", "HOME_URL=runtime"}) {
		t.Fatalf("values %v %v", values, err)
	}
}
