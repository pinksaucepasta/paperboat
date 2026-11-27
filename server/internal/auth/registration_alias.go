package auth

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/emailpolicy"
)

// ExistingAccountForAlias answers a sign-up before WorkOS creates the login. First sign-up wins (F13-07,
// user decision 2026-11-27): when an account exists whose canonical address (lowercased, +tag stripped) matches
// the submitted one under any other spelling, it returns that account's exact address so the sign-up is refused.
// An account registered under the submitted spelling itself is WorkOS's duplicate to handle (it signs in).
func (s *Service) ExistingAccountForAlias(ctx context.Context, submitted string) (string, bool, error) {
	submitted = strings.ToLower(strings.TrimSpace(submitted))
	var existing string
	var exists bool
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		canonical, err := emailpolicy.Normalize(ctx, tx, submitted)
		if err != nil {
			return err
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM paperboat.users WHERE lower(primary_email)=$1 LIMIT 1`, submitted).Scan(&id)
		if err == nil {
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT primary_email FROM paperboat.users WHERE canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1
 ORDER BY (canonical_email=$1) DESC NULLS LAST,created_at,id LIMIT 1`, canonical).Scan(&existing)
		if err == pgx.ErrNoRows {
			return nil
		}
		exists = err == nil
		return err
	})
	return existing, exists, err
}
