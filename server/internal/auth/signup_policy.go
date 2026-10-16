package auth

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

// Resolve verified provider identities before account creation. The canonical
// lock makes concurrent first-use aliases create one account and two sessions.
func resolveWorkOSIdentity(ctx context.Context, tx *db.Tx, profile WorkOSProfile) (string, string, error) {
	canonical, err := emailpolicy.Normalize(ctx, tx, profile.Email)
	if err != nil {
		return "", "", err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,174))`, canonical); err != nil {
		return "", "", err
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT u.id FROM paperboat.users u WHERE u.workos_subject=$1 OR EXISTS(
 SELECT 1 FROM paperboat.user_identities i WHERE i.user_id=u.id AND i.provider='workos' AND i.provider_subject=$1)
 ORDER BY u.id LIMIT 1`, profile.Subject).Scan(&id)
	if err == nil {
		return canonical, id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM paperboat.users WHERE canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1
 ORDER BY (canonical_email=$1) DESC NULLS LAST,created_at,id LIMIT 1`, canonical).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return canonical, newID("user"), nil
	}
	if err != nil {
		return "", "", err
	}
	if !profile.EmailVerified {
		return "", "", &WorkOSVerificationError{Reason: "email_not_verified"}
	}
	return canonical, id, nil
}
