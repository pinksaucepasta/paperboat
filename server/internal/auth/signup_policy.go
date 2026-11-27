package auth

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/emailpolicy"
)

// EmailAlreadyRegisteredError refuses a new provider identity whose canonical
// address (lowercased, +tag stripped) already belongs to an account, in any
// order: first sign-up wins (F13-07, user decision 2026-11-27). Existing is that
// account's exact address, the one to sign in with.
type EmailAlreadyRegisteredError struct{ Existing string }

func (e *EmailAlreadyRegisteredError) Error() string {
	return "An account with " + e.Existing + " already exists. Sign in with that address."
}

// Resolve verified provider identities before account creation. A known
// provider subject signs into its account; a new subject gets a new account
// unless its canonical address is taken (any spelling), which is refused. The canonical lock
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
		// A known identity whose provider email now names another account's
		// address is refused, never given a duplicate or that address.
		// Only a change of address is checked, so legacy accounts that already
		// share a canonical address keep signing in through their own subject.
		var other bool
		var existing string
		if err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM paperboat.users WHERE id=$2 AND
 coalesce(canonical_email,paperboat.canonical_email_address(primary_email))=$1),
 coalesce((SELECT primary_email FROM paperboat.users WHERE id<>$2 AND
 (canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1) ORDER BY (canonical_email=$1) DESC NULLS LAST,created_at,id LIMIT 1),'')`, canonical, id).Scan(&other, &existing); err != nil {
			return "", "", err
		}
		other = other && existing != ""
		if other && !profile.EmailVerified {
			return "", "", &WorkOSVerificationError{Reason: "email_not_verified"}
		}
		if other {
			return canonical, id, &EmailAlreadyRegisteredError{Existing: existing}
		}
		return canonical, id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", "", err
	}
	var existing string
	err = tx.QueryRow(ctx, `SELECT id,primary_email FROM paperboat.users WHERE canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1
 ORDER BY (canonical_email=$1) DESC NULLS LAST,created_at,id LIMIT 1`, canonical).Scan(&id, &existing)
	if errors.Is(err, pgx.ErrNoRows) {
		return canonical, newID("user"), nil
	}
	if err != nil {
		return "", "", err
	}
	// F13-07: first sign-up wins. Any other spelling of the canonical address
	// (x after x+tag, x+1 after x+2, x+tag after x) is refused, naming the
	// existing account's exact address.
	// An unverified address must not learn whether an account exists.
	if !profile.EmailVerified {
		return "", "", &WorkOSVerificationError{Reason: "email_not_verified"}
	}
	// The existing account's id is returned with the refusal so the caller can
	// report a ban on it first; it is never used to sign in.
	return canonical, id, &EmailAlreadyRegisteredError{Existing: existing}
}
