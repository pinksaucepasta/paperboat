package auth

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/emailpolicy"
)

// EmailAlreadyRegisteredError refuses a new provider identity whose canonical
// address (lowercased, first +tag stripped) already belongs to an account
// (F13-07). A +tag can be a different mailbox, so accounts are never merged by
// alias: the second sign-up, in either order, is refused.
type EmailAlreadyRegisteredError struct{ Canonical string }

func (e *EmailAlreadyRegisteredError) Error() string {
	return "An account for " + e.Canonical + " already exists. Sign in with that address."
}

// Resolve verified provider identities before account creation. A known
// provider subject signs into its account; a new subject gets a new account
// unless its canonical address is taken, which is refused. The canonical lock
// makes concurrent first-use aliases create at most one account.
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
	return "", "", &EmailAlreadyRegisteredError{Canonical: canonical}
}
