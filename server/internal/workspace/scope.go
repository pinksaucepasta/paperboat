// Package workspace defines the authenticated resource scope selected by a
// user. A team membership permits selecting a team; it does not grant access
// to any resource in that team.
package workspace

import (
	"context"
	"errors"
	"regexp"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
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
//
// It runs before nearly every workspace-scoped request and decides nothing it writes, so it reads one snapshot
// without the team lock: holding that server-wide lock made every account's requests wait behind any team change
// or team read in progress. Only a team that requires SSO needs a write (the sign-in check records a bypass), in
// its own transaction.
func Validate(ctx context.Context, database *db.DB, account string, scope Scope) error {
	if database == nil {
		return ErrForbidden
	}
	var ssoRequired bool
	err := database.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		ssoRequired, err = checkTx(ctx, tx, account, scope)
		return err
	})
	if err != nil || !ssoRequired {
		return err
	}
	return database.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		return teams.SSOGuardTx(ctx, tx, scope.TeamID, account)
	})
}

// ValidateTx checks the selection inside the caller's transaction, for callers that act on it there. It
// takes no lock: it only reads membership, suspension and deletion, and the SERIALIZABLE transaction orders it
// against a concurrent change of those (one of the two retries). Taking the global team write lock here put
// every scoped read-then-act on the platform in one queue, and mid-transaction, after the snapshot (Lane X S2).
// Callers that write team state take teams.Lock themselves.
func ValidateTx(ctx context.Context, tx *db.Tx, account string, scope Scope) error {
	if tx == nil {
		return ErrInvalid
	}
	ssoRequired, err := checkTx(ctx, tx, account, scope)
	if err != nil || !ssoRequired {
		return err
	}
	return teams.SSOGuardTx(ctx, tx, scope.TeamID, account)
}

// checkTx reports whether the account may select scope, and whether its team requires SSO.
func checkTx(ctx context.Context, tx *db.Tx, account string, scope Scope) (ssoRequired bool, err error) {
	if account == "" || !validScope(scope) {
		return false, ErrInvalid
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, account).Scan(&active); err != nil {
		return false, err
	}
	if !active {
		return false, ErrForbidden
	}
	if scope.TeamID == "" {
		return false, nil
	}
	var member bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(
	 SELECT 1 FROM teams t JOIN team_members m USING(team_id)
	 WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended
	   AND m.account_id=$2 AND m.active
	),coalesce((SELECT sso_required FROM teams WHERE team_id=$1),false)`, scope.TeamID, account).Scan(&member, &ssoRequired); err != nil {
		return false, err
	}
	if !member {
		return false, ErrForbidden
	}
	return ssoRequired, nil
}

func validScope(scope Scope) bool {
	if scope.ID == "personal" {
		return scope.TeamID == ""
	}
	return scope.TeamID != "" && scope.ID == scope.TeamID && teamSlug.MatchString(scope.TeamID)
}
