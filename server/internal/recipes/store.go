package recipes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

const recipeColumns = `r.id, r.token_id, r.account_id, coalesce(r.team_id,''), r.name, r.revision, r.source, r.source_format,
 r.canonical, 'user:'||u.primary_email, r.created_at, r.updated_at, r.expires_at, r.last_used_at, r.rotated_at, r.revoked_at,
 coalesce(r.max_machines,0), (SELECT count(*) FROM recipe_machines rm JOIN live_user_machines m ON m.id=rm.machine_id WHERE rm.recipe_id=r.id)`

// Create stores a new recipe for actor in ws. A repeated create with the same token id and auth
// (a client that lost the response) returns the recipe it made.
func (s *Service) Create(ctx context.Context, actor string, ws workspace.Scope, in Input) (Recipe, error) {
	if err := checkToken(in); err != nil {
		return Recipe{}, err
	}
	r, err := parse(in)
	if err != nil {
		return Recipe{}, err
	}
	id, err := newID()
	if err != nil {
		return Recipe{}, fmt.Errorf("create recipe id: %w", err)
	}
	var out Recipe
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		var existing string
		err := tx.QueryRow(ctx, `SELECT id FROM recipes WHERE token_id=$1 AND auth_hash=$2 AND account_id=$3 AND revoked_at IS NULL`,
			in.TokenID, recipe.AuthHash(in.Auth), actor).Scan(&existing)
		if err == nil {
			out, err = getTx(ctx, tx, existing, false)
			return err
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		rows, err := s.checkWorkspaceTx(ctx, tx, actor, ws, r)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if _, err := tx.Exec(ctx, `INSERT INTO recipes(id,token_id,account_id,team_id,name,source,source_format,canonical,auth_hash,
		 max_machines,remove_after_offline,machine_lifetime,created_at,updated_at,expires_at)
		 VALUES($1,$2,$3,nullif($4,''),$5,$6,$7,$8,$9,nullif($10,0),$11,$12,$13,$13,$14)`,
			id, in.TokenID, actor, ws.TeamID, in.Name, in.Source, in.Format, r.Canonical(), recipe.AuthHash(in.Auth),
			r.MaxMachines, interval(r.OfflineAfter()), interval(r.Lifetime()), now, now.Add(r.ExpiresIn())); err != nil {
			return mapInsertError(err)
		}
		if err := insertReachTx(ctx, tx, id, rows); err != nil {
			return err
		}
		if err := syncRecipeWrapsTx(ctx, tx, id, actor, ws, r.Vaults(), in.Wraps); err != nil {
			return err
		}
		if out, err = getTx(ctx, tx, id, false); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "recipe.created",
			ResourceType: "recipe", ResourceID: id, Metadata: map[string]any{"workspace": ws.ID, "name": in.Name, "expires_at": out.ExpiresAt}})
	})
	if err != nil {
		return Recipe{}, fmt.Errorf("create recipe: %w", err)
	}
	return out, nil
}

// Update replaces a recipe's file and raises its revision; the token stays. It returns the
// recipe before and after, so the client can print what changed.
func (s *Service) Update(ctx context.Context, actor string, ws workspace.Scope, in Input) (before, after Recipe, err error) {
	r, err := parse(in)
	if err != nil {
		return Recipe{}, Recipe{}, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := s.authorizeWriteTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		if before, err = byNameTx(ctx, tx, ws, actor, in.Name, true); err != nil {
			return err
		}
		rows, err := s.checkWorkspaceTx(ctx, tx, before.AccountID, ws, r)
		if err != nil {
			return err
		}
		expires := before.ExpiresAt
		if r.Expires != before.Recipe.Expires {
			expires = s.now().UTC().Add(r.ExpiresIn()) // a changed expires counts from now
		}
		if _, err := tx.Exec(ctx, `UPDATE recipes SET source=$2,source_format=$3,canonical=$4,revision=revision+1,max_machines=nullif($5,0),
		 remove_after_offline=$6,machine_lifetime=$7,expires_at=$8,updated_at=$9 WHERE id=$1`,
			before.ID, in.Source, in.Format, r.Canonical(), r.MaxMachines, interval(r.OfflineAfter()), interval(r.Lifetime()), expires, s.now().UTC()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM recipe_reach_rules WHERE recipe_id=$1`, before.ID); err != nil {
			return err
		}
		if err := insertReachTx(ctx, tx, before.ID, rows); err != nil {
			return err
		}
		if err := syncRecipeWrapsTx(ctx, tx, before.ID, before.AccountID, ws, r.Vaults(), in.Wraps); err != nil {
			return err
		}
		if after, err = getTx(ctx, tx, before.ID, false); err != nil {
			return err
		}
		if err := publishRecipeTx(ctx, tx, before.ID); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorUser, EventType: "recipe.updated",
			ResourceType: "recipe", ResourceID: before.ID, Metadata: map[string]any{"workspace": ws.ID, "name": in.Name, "revision": after.Revision}})
	})
	if err != nil {
		return Recipe{}, Recipe{}, fmt.Errorf("update recipe: %w", err)
	}
	return before, after, nil
}

// checkWorkspaceTx resolves the parts of r that name workspace things: reach targets against the
// policy, vaults and the config repository. It returns the reach enforcement rows.
func (s *Service) checkWorkspaceTx(ctx context.Context, tx *db.Tx, creator string, ws workspace.Scope, r recipe.Recipe) ([]policy.Row, error) {
	var rows []policy.Row
	if len(r.Reach) > 0 {
		w := teams.Workspace{TeamID: ws.TeamID}
		if ws.TeamID == "" {
			w.Owner = creator
		}
		var errs policy.Errors
		var err error
		if rows, errs, err = teams.RecipeReachRowsTx(ctx, tx, w, r.Reach); err != nil {
			return nil, err
		}
		if len(errs) > 0 {
			return nil, &ReachError{Errors: errs}
		}
	}
	if r.Config != nil {
		known, err := repositoryNamesTx(ctx, tx, creator)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(known, r.Config.Repository) {
			return nil, &NameError{Kind: "repository", Name: r.Config.Repository, Workspace: ws.ID, Known: known, Suggestion: recipe.Suggest(r.Config.Repository, known)}
		}
	}
	for _, v := range r.Vaults() {
		known, err := vaultNamesTx(ctx, tx, creator, ws)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(known, v) {
			return nil, &NameError{Kind: "vault", Name: v, Workspace: ws.ID, Known: known, Suggestion: recipe.Suggest(v, known)}
		}
	}
	return rows, nil
}

// repositoryNamesTx lists the creator's connected config repositories by the names a recipe
// may use: "owner/name" and "name".
func repositoryNamesTx(ctx context.Context, tx *db.Tx, account string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT display_name FROM control_config_repositories WHERE owner_user_id=$1 AND state='active' ORDER BY display_name`, account)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range names {
		out = append(out, n)
		if _, short, ok := strings.Cut(n, "/"); ok {
			out = append(out, short)
		}
	}
	return out, nil
}

// vaultNamesTx lists the workspace's named ENV vaults.
func vaultNamesTx(ctx context.Context, tx *db.Tx, account string, ws workspace.Scope) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT name FROM environment_named_vaults WHERE deleted_at IS NULL
	 AND coalesce(team_id,'')=$1 AND (team_id IS NOT NULL OR owner_account=$2) ORDER BY name`, ws.TeamID, account)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func insertReachTx(ctx context.Context, tx *db.Tx, id string, rows []policy.Row) error {
	for _, r := range rows {
		for _, p := range r.Ports {
			if _, err := tx.Exec(ctx, `INSERT INTO recipe_reach_rules(recipe_id,entry,target_kind,target_id,action,ports) VALUES($1,$2,$3,$4,$5,int4range($6,$7,'[]'))`,
				id, r.Rule, r.TargetKind, r.TargetID, r.Action, p.From, p.To); err != nil {
				return err
			}
		}
	}
	return nil
}

// authorizeWriteTx: personal recipes are their owner's; team recipes need an owner or admin.
func (s *Service) authorizeWriteTx(ctx context.Context, tx *db.Tx, actor string, ws workspace.Scope) error {
	if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
		return err
	}
	admin, err := manages(ctx, tx, actor, ws)
	if err != nil {
		return err
	}
	if ws.TeamID != "" && !admin {
		return ErrForbidden
	}
	return nil
}

func mapInsertError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		switch pg.ConstraintName {
		case "recipes_live_name":
			return ErrNameTaken
		case "recipes_token_id_key", "recipes_auth_hash_key":
			return ErrTokenTaken
		}
	}
	return err
}

// List returns the workspace's live recipes, by name. Every member may read them.
// shortcut: no paging; a workspace keeps few recipes. Add a cursor if lists pass a few hundred.
func (s *Service) List(ctx context.Context, actor string, ws workspace.Scope) ([]Recipe, error) {
	var out []Recipe
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+recipeColumns+` FROM recipes r JOIN users u ON u.id=r.account_id
		 WHERE r.revoked_at IS NULL AND coalesce(r.team_id,'')=$1 AND (r.team_id IS NOT NULL OR r.account_id=$2) ORDER BY r.name LIMIT 500`, ws.TeamID, actor)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recipe, error) { return scanRecipe(row) })
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list recipes: %w", err)
	}
	return out, nil
}

// Get returns a live recipe of ws by name with its machines.
func (s *Service) Get(ctx context.Context, actor string, ws workspace.Scope, name string) (Recipe, []MachineState, error) {
	var out Recipe
	var machines []MachineState
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := workspace.ValidateTx(ctx, tx, actor, ws); err != nil {
			return err
		}
		var err error
		if out, err = byNameTx(ctx, tx, ws, actor, name, false); err != nil {
			return err
		}
		machines, err = machinesTx(ctx, tx, out.ID)
		return err
	})
	if err != nil {
		return Recipe{}, nil, fmt.Errorf("get recipe: %w", err)
	}
	return out, machines, nil
}

// MachineState is one machine of a recipe with what it applied.
type MachineState struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	Online          bool            `json:"online"`
	LastSeenAt      *time.Time      `json:"last_seen_at"`
	AppliedRevision int             `json:"applied_revision"`
	ApplyState      json.RawMessage `json:"apply_state"`
	RemoveAt        *time.Time      `json:"remove_at"`
}

func machinesTx(ctx context.Context, tx *db.Tx, recipeID string) ([]MachineState, error) {
	rows, err := tx.Query(ctx, `SELECT m.id, m.alias, m.online, m.last_seen_at, rm.applied_revision, rm.apply_state,
	 CASE WHEN rm.remove_at IS NULL THEN rm.offline_remove_at WHEN rm.offline_remove_at IS NULL THEN rm.remove_at ELSE least(rm.remove_at, rm.offline_remove_at) END
	 FROM recipe_machines rm JOIN live_user_machines m ON m.id=rm.machine_id WHERE rm.recipe_id=$1 ORDER BY m.alias LIMIT 1000`, recipeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (MachineState, error) {
		var m MachineState
		var seen, remove sql.NullTime
		err := row.Scan(&m.ID, &m.Name, &m.Online, &seen, &m.AppliedRevision, &m.ApplyState, &remove)
		m.LastSeenAt, m.RemoveAt = timePtr(seen), timePtr(remove)
		return m, err
	})
}

func byNameTx(ctx context.Context, tx *db.Tx, ws workspace.Scope, actor, name string, lock bool) (Recipe, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM recipes WHERE revoked_at IS NULL AND name=$1 AND coalesce(team_id,'')=$2 AND (team_id IS NOT NULL OR account_id=$3)`,
		name, ws.TeamID, actor).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Recipe{}, ErrNotFound
	}
	if err != nil {
		return Recipe{}, err
	}
	return getTx(ctx, tx, id, lock)
}

func getTx(ctx context.Context, tx *db.Tx, id string, lock bool) (Recipe, error) {
	query := `SELECT ` + recipeColumns + ` FROM recipes r JOIN users u ON u.id=r.account_id WHERE r.id=$1`
	if lock {
		query += ` FOR UPDATE OF r`
	}
	rows, err := tx.Query(ctx, query, id)
	if err != nil {
		return Recipe{}, err
	}
	out, err := pgx.CollectExactlyOneRow(rows, func(row pgx.CollectableRow) (Recipe, error) { return scanRecipe(row) })
	if errors.Is(err, pgx.ErrNoRows) {
		return Recipe{}, ErrNotFound
	}
	return out, err
}

func scanRecipe(row pgx.Row) (Recipe, error) {
	var r Recipe
	var teamID string
	var canonical []byte
	var lastUsed, rotated, revoked sql.NullTime
	if err := row.Scan(&r.ID, &r.TokenID, &r.AccountID, &teamID, &r.Name, &r.Revision, &r.Source, &r.Format, &canonical, &r.CreatedBy,
		&r.CreatedAt, &r.UpdatedAt, &r.ExpiresAt, &lastUsed, &rotated, &revoked, &r.MaxMachines, &r.Machines); err != nil {
		return Recipe{}, err
	}
	r.Workspace = scopeFor(teamID)
	r.LastUsedAt, r.RotatedAt, r.RevokedAt = timePtr(lastUsed), timePtr(rotated), timePtr(revoked)
	parsed, err := recipe.ParseJSON("stored recipe", canonical)
	if err != nil {
		return Recipe{}, fmt.Errorf("stored recipe %s: %w", r.ID, err)
	}
	r.Recipe = parsed
	return r, nil
}

// manages reports whether actor owns or administers team ws.
func manages(ctx context.Context, tx *db.Tx, actor string, ws workspace.Scope) (bool, error) {
	if ws.TeamID == "" {
		return false, nil
	}
	var admin bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM teams t JOIN team_members m USING(team_id)
	 WHERE t.team_id=$1 AND m.account_id=$2 AND m.active AND (m.role='admin' OR t.owner_account=$2))`, ws.TeamID, actor).Scan(&admin)
	return admin, err
}

func interval(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return fmt.Sprintf("%d seconds", int64(d/time.Second))
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	return &t.Time
}
