package auth

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

// ExistingAccountForAlias answers a sign-up before WorkOS creates the login. When the submitted address
// is a plus-alias (or any other spelling) of the canonical address of an account that already exists, and no
// account was registered under the submitted spelling itself, it returns that canonical address so the
// sign-up can be refused instead of silently landing in the other account. Exact duplicates are WorkOS's.
func (s *Service) ExistingAccountForAlias(ctx context.Context, submitted string) (string, bool, error) {
	submitted = strings.ToLower(strings.TrimSpace(submitted))
	var canonical string
	var exists bool
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var err error
		if canonical, err = emailpolicy.Normalize(ctx, tx, submitted); err != nil {
			return err
		}
		if canonical == submitted {
			return nil
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM paperboat.users WHERE lower(primary_email)=$1 LIMIT 1`, submitted).Scan(&id)
		if err == nil {
			return nil
		}
		if err != pgx.ErrNoRows {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.users WHERE canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1)`, canonical).Scan(&exists)
		return err
	})
	return canonical, exists, err
}
