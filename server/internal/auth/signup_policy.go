package auth

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/emailpolicy"
)

func signupEmail(ctx context.Context, tx *db.Tx, profile WorkOSProfile) (sql.NullString, error) {
	var existing bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.users WHERE workos_subject=$1)`, profile.Subject).Scan(&existing); err != nil {
		return sql.NullString{}, err
	}
	if existing {
		return sql.NullString{}, nil
	}
	canonical, err := emailpolicy.Normalize(ctx, tx, profile.Email)
	if err != nil {
		return sql.NullString{}, err
	}
	var duplicate bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.users WHERE canonical_email=$1 OR paperboat.canonical_email_address(primary_email)=$1)`, canonical).Scan(&duplicate); err != nil {
		return sql.NullString{}, err
	}
	if duplicate {
		return sql.NullString{}, emailpolicy.ErrDuplicate
	}
	return sql.NullString{String: canonical, Valid: true}, nil
}

func signupPolicyError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "users_canonical_email_unique" {
		return emailpolicy.ErrDuplicate
	}
	return err
}
