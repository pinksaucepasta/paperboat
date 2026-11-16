package usermachines

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// ErrRecipeUnavailable: the session's recipe was revoked while the machine was pairing.
var ErrRecipeUnavailable = errors.New("recipe revoked")

// RecipeFullError: the recipe already has max_machines machines.
type RecipeFullError struct {
	Name string
	Max  int
}

func (e *RecipeFullError) Error() string { return "recipe " + e.Name + " has reached max_machines" }

// attachRecipeMachineTx makes a machine set up by a recipe session the recipe's (PLAN F26 section
// 6), in the setup transaction: counted against max_machines, shared into a team recipe's team,
// tagged with the recipe's tags, given its machine_lifetime deadline, and bound to the session so
// the reach never limits the machine's own pb.
func attachRecipeMachineTx(ctx context.Context, tx *db.Tx, account, machineID, session string) error {
	recipeID, ok := auth.RecipeFromContext(ctx)
	if !ok {
		return nil
	}
	var teamID, name string
	var canonical []byte
	var max, machines int
	err := tx.QueryRow(ctx, `SELECT coalesce(r.team_id,''), r.name, r.canonical, coalesce(r.max_machines,0),
	 (SELECT count(*) FROM recipe_machines rm JOIN live_user_machines m ON m.id=rm.machine_id WHERE rm.recipe_id=r.id AND rm.machine_id<>$3)
	 FROM recipes r WHERE r.id=$1 AND r.account_id=$2 AND r.revoked_at IS NULL FOR UPDATE`, recipeID, account, machineID).Scan(&teamID, &name, &canonical, &max, &machines)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRecipeUnavailable
	}
	if err != nil {
		return err
	}
	if max > 0 && machines >= max {
		return &RecipeFullError{Name: name, Max: max}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO recipe_machines(machine_id,recipe_id,remove_at)
	 SELECT $1, r.id, now() + r.machine_lifetime FROM recipes r WHERE r.id=$2 ON CONFLICT (machine_id) DO NOTHING`, machineID, recipeID); err != nil {
		return err
	}
	if session != "" {
		if _, err := tx.Queries().BindCLIClientSessionToUserMachine(ctx, dbsqlc.BindCLIClientSessionToUserMachineParams{TargetMachineID: sql.NullString{String: machineID, Valid: true}, TargetCLISessionID: session, TargetUserID: account}); err != nil {
			return err
		}
	}
	w := teams.Workspace{Owner: account}
	if teamID != "" {
		if _, err := teams.ShareEnrolledMachineTx(ctx, tx, account, teamID, machineID); err != nil {
			return err
		}
		w = teams.Workspace{TeamID: teamID}
	}
	var stored struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(canonical, &stored); err != nil {
		return err
	}
	return teams.SetRecipeMachineTagsTx(ctx, tx, w, account, name, machineID, stored.Tags)
}
