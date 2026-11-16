package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/recipe"
)

// ErrOutsideReach: the recipe's [[reach]] does not cover this machine action.
var ErrOutsideReach = errors.New("this recipe's reach does not cover this machine action")

// RecipeRefusal is why a recipe token cannot start a session; the client prints the matching
// PLAN F26 section 3 text. Code: recipe_invalid, recipe_expired, recipe_replaced, recipe_full.
type RecipeRefusal struct {
	Code string
	Name string
	// At is when the token expired or was replaced.
	At  time.Time
	Max int
}

func (e *RecipeRefusal) Error() string { return "recipe token refused: " + e.Code }

// RecipeSessionRequest is a machine pairing with a recipe token: TokenID and Auth come from
// shared/recipe.Token on the machine.
type RecipeSessionRequest struct {
	TokenID               string
	Auth                  []byte
	Label, DeviceType, OS string
}

// RecipeSession is a session started from a recipe, with the recipe to apply.
type RecipeSession struct {
	Tokens    TokenSet
	RecipeID  string
	Name      string
	Revision  int
	Workspace workspace.Scope
	Recipe    json.RawMessage
}

// StartRecipeSession turns a recipe token into an ordinary CLI session of the recipe's creator in
// the recipe's workspace that carries the recipe (PLAN F26 section 6): pb works as after a login,
// narrowed by the reach, and the session ends when the recipe is revoked.
func (s *DeviceService) StartRecipeSession(ctx context.Context, in RecipeSessionRequest) (RecipeSession, error) {
	if !recipe.ValidID(in.TokenID) || len(in.Auth) != 32 {
		return RecipeSession{}, &RecipeRefusal{Code: "recipe_invalid"}
	}
	if in.Label == "" || len(in.Label) > s.cfg.MaxClientLabelLength || strings.TrimSpace(in.OS) == "" || !slices.Contains([]string{"desktop", "server", "container"}, in.DeviceType) {
		return RecipeSession{}, &DeviceError{Code: "validation_failed"}
	}
	if err := planlimits.AllowSDKRequest(in.TokenID, in.TokenID, "sessions"); err != nil {
		return RecipeSession{}, err
	}
	access, refresh := randomToken(32), randomToken(32)
	sessionID := newID("session")
	scopes := normalizedScopes(s.cfg.AllowedScopes)
	var out RecipeSession
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var account, teamID string
		var hash, previous []byte
		var rotated sql.NullTime
		var expires time.Time
		var max, machines int
		err := tx.QueryRow(ctx, `SELECT r.id, r.account_id, coalesce(r.team_id,''), r.name, r.revision, r.canonical, r.auth_hash,
		 coalesce(r.previous_auth_hash,''::bytea), r.rotated_at, r.expires_at, coalesce(r.max_machines,0),
		 (SELECT count(*) FROM recipe_machines rm JOIN live_user_machines m ON m.id=rm.machine_id WHERE rm.recipe_id=r.id)
		 FROM recipes r JOIN users u ON u.id=r.account_id
		 WHERE r.token_id=$1 AND r.revoked_at IS NULL AND u.status='active'
		  AND (r.team_id IS NULL OR EXISTS(SELECT 1 FROM team_members m JOIN teams t USING(team_id) WHERE m.team_id=r.team_id AND m.account_id=r.account_id AND m.active AND t.deleted_at IS NULL))
		 FOR UPDATE OF r`, in.TokenID).Scan(&out.RecipeID, &account, &teamID, &out.Name, &out.Revision, &out.Recipe, &hash, &previous, &rotated, &expires, &max, &machines)
		if errors.Is(err, pgx.ErrNoRows) {
			return &RecipeRefusal{Code: "recipe_invalid"}
		}
		if err != nil {
			return err
		}
		got := recipe.AuthHash(in.Auth)
		switch {
		case string(got) != string(hash) && rotated.Valid && string(got) == string(previous):
			return &RecipeRefusal{Code: "recipe_replaced", Name: out.Name, At: rotated.Time}
		case string(got) != string(hash):
			return &RecipeRefusal{Code: "recipe_invalid"}
		case !s.now().Before(expires):
			return &RecipeRefusal{Code: "recipe_expired", Name: out.Name, At: expires}
		case max > 0 && machines >= max:
			return &RecipeRefusal{Code: "recipe_full", Name: out.Name, Max: max}
		}
		out.Workspace = workspace.Scope{ID: "personal"}
		if teamID != "" {
			out.Workspace = workspace.Scope{ID: teamID, TeamID: teamID}
		}
		now := s.now()
		session := dbsqlc.CreateClientSessionParams{ID: sessionID, UserID: account, ClientID: s.cfg.ClientID, ClientLabel: in.Label, DeviceType: in.DeviceType, Os: strings.TrimSpace(in.OS), Scopes: scopes, CreatedAt: now, ApprovedAt: now}
		if err := CreateClientSessionTx(ctx, tx, session, access, refresh, s.tokenPolicy()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE cli_client_sessions SET recipe_id=$2 WHERE id=$1`, sessionID, out.RecipeID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE recipes SET last_used_at=$2 WHERE id=$1`, out.RecipeID, now.UTC()); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: "auth.client.recipe", ResourceType: "client_session", ResourceID: sessionID,
			IdempotencyKey: "auth.client.recipe:" + sessionID, Metadata: map[string]any{"recipe_id": out.RecipeID, "recipe": out.Name}})
	})
	if err != nil {
		return RecipeSession{}, err
	}
	out.Tokens = TokenSet{AccessToken: access, RefreshToken: refresh, TokenType: "Bearer", ExpiresIn: int(s.cfg.AccessTokenLifetime / time.Second), Scope: scopes, CLIClientSessionID: sessionID}
	return out, nil
}

type recipeKey struct{}

type recipeContext struct{ id, machine string }

// WithRecipe marks ctx as a request of a recipe session; machine is the session's own machine
// ("" before it is paired), which the reach never limits.
func WithRecipe(ctx context.Context, recipeID, machine string) context.Context {
	return context.WithValue(ctx, recipeKey{}, recipeContext{id: recipeID, machine: machine})
}

// RecipeFromContext returns the recipe a request runs under, if any.
func RecipeFromContext(ctx context.Context) (string, bool) {
	r, ok := ctx.Value(recipeKey{}).(recipeContext)
	return r.id, ok && r.id != ""
}

// RecipeReachCheck returns the recipe whose reach must cover an action on machine: none for a
// request outside a recipe session or one on the session's own machine.
func RecipeReachCheck(ctx context.Context, machine string) (string, bool) {
	r, ok := ctx.Value(recipeKey{}).(recipeContext)
	if !ok || r.id == "" || (r.machine != "" && r.machine == machine) {
		return "", false
	}
	return r.id, true
}

// AuthorizeRecipeSession checks a recipe session's reach for one machine action on its live
// session. Creator access remains the caller's separate machine/workspace policy check.
func AuthorizeRecipeSession(ctx context.Context, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, account, session, machine, capability string, port int) error {
	id, check := RecipeReachCheck(ctx, machine)
	if !check {
		return nil
	}
	scope, selected := workspace.FromContext(ctx)
	if !selected {
		return ErrOutsideReach
	}
	var allowed bool
	err := database.QueryRowContext(ctx, `SELECT EXISTS(
 SELECT 1 FROM paperboat.cli_client_sessions c JOIN paperboat.recipes r ON r.id=c.recipe_id
 WHERE c.id=$1 AND c.user_id=$2 AND c.state='active' AND c.revoked_at IS NULL
 AND r.id=$3 AND r.account_id=$2 AND r.team_id IS NOT DISTINCT FROM nullif($4,'')
 AND paperboat.recipe_reach_allows(r.id,$5,$6,nullif($7,0)))`,
		session, account, id, scope.TeamID, machine, capability, port).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrOutsideReach
	}
	return nil
}
