package httpapi

import (
	"context"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
)

type recipeReposFake map[string]bool

func (f recipeReposFake) ReadsRepository(_ context.Context, _, repositoryID string) (bool, error) {
	return f[repositoryID], nil
}

type ruleFake controlplane.ConfigMachineRuleView

func (f ruleFake) MachineRule(context.Context, string, string) (controlplane.ConfigMachineRuleView, error) {
	return controlplane.ConfigMachineRuleView(f), nil
}

// A recipe machine reads its recipe's [config] repository, or the one its workspace policy's
// config_sync tag rule picks for it (F11-06), and no other.
func TestRecipeMayReadOnlyItsOwnOrItsRulesRepository(t *testing.T) {
	p := principal{User: auth.User{ID: "account_1"}, Client: &auth.ClientPrincipal{RecipeID: "rcp_1", MachineID: "machine_1"}}
	recipes := recipeReposFake{"repository_recipe": true}
	rule := ruleFake{Matched: true, RepositoryID: "repository_rule"}
	for repository, want := range map[string]bool{"repository_recipe": true, "repository_rule": true, "repository_other": false} {
		if got, err := recipeMayReadRepository(context.Background(), p, repository, recipes, rule); err != nil || got != want {
			t.Fatalf("%s: %v %v", repository, got, err)
		}
	}
	if got, _ := recipeMayReadRepository(context.Background(), p, "repository_rule", recipes, ruleFake{Matched: false, RepositoryID: "repository_rule"}); got {
		t.Fatal("an unmatched rule allowed a read")
	}
}
