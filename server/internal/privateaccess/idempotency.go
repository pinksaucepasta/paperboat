package privateaccess

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// IdempotencyStore binds an idempotency key to one request fingerprint until it expires, across every server
// instance (F21-54). Bind reports false when the key is already bound to a different, unexpired fingerprint.
type IdempotencyStore interface {
	Bind(ctx context.Context, accountID, sessionID, key, fingerprint string, expires time.Time) (bool, error)
}

type sqlIdempotencyStore struct{ db *db.DB }

func NewSQLIdempotencyStore(database *db.DB) (IdempotencyStore, error) {
	if database == nil || database.Pool() == nil {
		return nil, fmt.Errorf("%w: database is required", ErrInvalid)
	}
	return sqlIdempotencyStore{db: database}, nil
}

func (s sqlIdempotencyStore) Bind(ctx context.Context, accountID, sessionID, key, fingerprint string, expires time.Time) (bool, error) {
	var bound string
	// A new or expired key takes this fingerprint; a live key keeps the one it has.
	err := s.db.Pool().QueryRow(ctx, `WITH up AS (
  INSERT INTO paperboat.private_access_idempotency (account_id, session_id, idempotency_key, fingerprint, expires_at)
  VALUES ($1,$2,$3,$4,$5)
  ON CONFLICT (account_id, session_id, idempotency_key) DO UPDATE SET fingerprint=excluded.fingerprint, expires_at=excluded.expires_at
  WHERE private_access_idempotency.expires_at <= now()
  RETURNING fingerprint)
SELECT fingerprint FROM up
UNION ALL SELECT fingerprint FROM paperboat.private_access_idempotency
  WHERE account_id=$1 AND session_id=$2 AND idempotency_key=$3 AND NOT EXISTS (SELECT 1 FROM up)
LIMIT 1`, accountID, sessionID, key, fingerprint, expires).Scan(&bound)
	if err != nil {
		return false, err
	}
	return bound == fingerprint, nil
}

// PurgeIdempotency deletes expired bindings in bounded batches; run it on the worker leader.
func PurgeIdempotency(ctx context.Context, database *db.DB) error {
	_, err := database.Pool().Exec(ctx, `DELETE FROM paperboat.private_access_idempotency WHERE ctid IN (
  SELECT ctid FROM paperboat.private_access_idempotency WHERE expires_at < now() LIMIT 5000)`)
	return err
}
