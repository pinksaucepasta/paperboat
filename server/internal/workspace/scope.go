// Package workspace defines the authenticated resource scope selected by a
// user. A team membership permits selecting a team; it does not grant access
// to any resource in that team.
package workspace

import (
	"context"
	"errors"
	"regexp"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

var (
	ErrInvalid   = errors.New("invalid workspace selector")
	ErrForbidden = errors.New("workspace is unavailable to this account")
	// ErrSSORequired: the team requires a recent SSO sign-in (PLAN F19-04). It is also an
	// ErrForbidden and a teams.ErrSSORequired, so every existing caller still refuses.
	ErrSSORequired error = ssoRequired{}
)

type ssoRequired struct{}

func (ssoRequired) Error() string { return teams.ErrSSORequired.Error() }
func (ssoRequired) Is(target error) bool {
	return target == ErrForbidden || target == teams.ErrSSORequired
}

var teamSlug = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// Scope is a request-local selection. ID is "personal" or the canonical team
// slug; TeamID is empty for Personal and equals ID for a team.
type Scope struct {
	ID     string
	TeamID string
}

type contextKey struct{}

func Parse(value string) (Scope, error) {
	if value == "personal" {
		return Scope{ID: "personal"}, nil
	}
	if !teamSlug.MatchString(value) {
		return Scope{}, ErrInvalid
	}
	return Scope{ID: value, TeamID: value}, nil
}

func FromContext(ctx context.Context) (Scope, bool) {
	scope, ok := ctx.Value(contextKey{}).(Scope)
	return scope, ok
}

func WithContext(ctx context.Context, scope Scope) context.Context {
	return context.WithValue(ctx, contextKey{}, scope)
}

// Validate checks current account status and exact team membership. Team
// subscription entitlement is checked by each resource authority that uses it.
func Validate(ctx context.Context, database *db.DB, account string, scope Scope) error {
	if database == nil {
		return ErrForbidden
	}
	return database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return ValidateTx(ctx, tx, account, scope)
	})
}

// ValidateTx serializes the selection check with team membership, suspension,
// and deletion changes. Callers may already hold this advisory transaction
// lock; PostgreSQL advisory transaction locks are reentrant within a session.
func ValidateTx(ctx context.Context, tx *db.Tx, account string, scope Scope) error {
	if tx == nil || account == "" || !validScope(scope) {
		return ErrInvalid
	}
	if err := teams.Lock(ctx, tx); err != nil {
		return err
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, account).Scan(&active); err != nil {
		return err
	}
	if !active {
		return ErrForbidden
	}
	if scope.TeamID == "" {
		return nil
	}
	var member bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
	 SELECT 1 FROM teams t JOIN team_members m USING(team_id)
	 WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended
	   AND m.account_id=$2 AND m.active
	)`, scope.TeamID, account).Scan(&member); err != nil {
		return err
	}
	if !member {
		return ErrForbidden
	}
	return teams.SSOGuardTx(ctx, tx, scope.TeamID, account)
}

func validScope(scope Scope) bool {
	if scope.ID == "personal" {
		return scope.TeamID == ""
	}
	return scope.TeamID != "" && scope.ID == scope.TeamID && teamSlug.MatchString(scope.TeamID)
}
