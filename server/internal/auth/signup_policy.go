package auth

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/emailpolicy"
	"strings"
)

// EmailAlreadyRegisteredError refuses a new provider identity whose canonical
// address (lowercased, first +tag stripped) already belongs to an account
// (F13-07). A +tag can be a different mailbox, so accounts are never merged by
// alias: the second sign-up, in either order, is refused.
type EmailAlreadyRegisteredError struct{ Canonical string }

func (e *EmailAlreadyRegisteredError) Error() string {
	return "An account for " + e.Canonical + " already exists. Sign in with that address."
}

func hasPlusTag(email string) bool {
	local, _, _ := strings.Cut(strings.TrimSpace(email), "@")
	return strings.Contains(local, "+")
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
		// A known identity whose provider email now names another account's
		// address is refused, never given a duplicate or that address.
		// Only a change of address is checked, so legacy accounts that already
		// share a canonical address keep signing in through their own subject.
		var other bool
		if err = tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM paperboat.users WHERE id=$2 AND
 coalesce(canonical_email,paperboat.canonical_email_address(primary_email))=$1)
 AND EXISTS(SELECT 1 FROM paperboat.users WHERE id<>$2 AND
 (canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1))`, canonical, id).Scan(&other); err != nil {
			return "", "", err
		}
		if other && !profile.EmailVerified {
			return "", "", &WorkOSVerificationError{Reason: "email_not_verified"}
		}
		if other {
			return canonical, id, &EmailAlreadyRegisteredError{Canonical: canonical}
		}
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
	// F13-07: only a plus address, or the exact address itself, is refused when
	// the canonical address has an account. A plain address whose canonical
	// form is held only by +tag accounts is its own new account (never merged).
	var exact bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.users WHERE lower(btrim(primary_email))=$1)`,
		strings.ToLower(strings.TrimSpace(profile.Email))).Scan(&exact); err != nil {
		return "", "", err
	}
	if !hasPlusTag(profile.Email) && !exact {
		return canonical, newID("user"), nil
	}
	// An unverified address must not learn whether an account exists.
	if !profile.EmailVerified {
		return "", "", &WorkOSVerificationError{Reason: "email_not_verified"}
	}
	// The existing account's id is returned with the refusal so the caller can
	// report a ban on it first; it is never used to sign in.
	return canonical, id, &EmailAlreadyRegisteredError{Canonical: canonical}
}
