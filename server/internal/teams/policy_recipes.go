package teams

import (
	"context"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// RecipeReachRowsTx checks a recipe's reach against the workspace's current policy (machine,
// tag, preview and tunnel names) and returns its enforcement rows for recipe_reach_rules.
func RecipeReachRowsTx(ctx context.Context, tx *db.Tx, w Workspace, scope []policy.ScopeEntry) ([]policy.Row, policy.Errors, error) {
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return nil, nil, err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return nil, nil, err
	}
	rows, errs := compileStored(cur.source, inv).ScopeRows(scope)
	return rows, errs, nil
}

// SetRecipeMachineTagsTx puts a machine just paired from a recipe into the recipe's tags (names
// without "tag:"), as one policy version authored by the recipe, inside the pairing transaction
// so the machine never exists untagged. The caller holds Lock.
func SetRecipeMachineTagsTx(ctx context.Context, tx *db.Tx, w Workspace, actor, recipeName, machineID string, tags []string) error {
	if len(tags) == 0 {
		return nil
	}
	cur, err := policyTx(ctx, tx, w)
	if err != nil {
		return err
	}
	inv, err := policyInventoryTx(ctx, tx, w)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(inv.Machines, func(m policy.Machine) bool { return m.ID == machineID }) {
		return ErrPolicyMachineNotFound
	}
	name := "machine:" + inv.MachineName(machineID)
	src := external(cur.source, inv)
	d, _ := policy.Parse(src)
	have := policy.MachineTags(d, name)
	want := slices.Clone(have)
	for _, t := range tags {
		if !slices.Contains(want, "tag:"+t) {
			want = append(want, "tag:"+t)
		}
	}
	slices.Sort(want)
	if len(want) == len(have) {
		return nil // already tagged (a resumed setup)
	}
	next, err := policy.SetMachineTags(src, name, want)
	if err != nil {
		return err
	}
	_, _, err = savePolicyTx(ctx, tx, w, config.WorkspacePolicy{}, policySave{actor: actor, recipe: recipeName, source: next,
		message: "Tags of " + strings.TrimPrefix(name, "machine:") + " set by recipe " + recipeName, kind: "saved", confirmed: true})
	return err
}
