// Package mintwithdrawal takes mint signing keys out of service while the server runs (F21-08).
// A withdrawal is one row in paperboat.mint_withdrawn_keys plus a notification in the same
// transaction; every server process listens, reloads the set and hands it to its mint.Provider,
// which refuses the key at once and publishes it in withdrawn_key_ids for edges and hosts.
//
// Prior art: teleport lib/services/watcher.go (CA changes reach every auth server through a
// backend watch, not a restart) and lib/auth/rotate.go (a key leaves service by a state change
// the verifiers read).
package mintwithdrawal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	channel = "paperboat_mint_withdrawn"
	// maxKeys bounds the set: edges refuse a trust document naming more than 128 withdrawn keys.
	maxKeys = 128
)

// Provider is the part of mint.Provider the watcher drives.
type Provider interface {
	Withdraw(ids []string) error
}

// Withdraw records keyID as withdrawn and notifies every server process. activeID is the
// deployment's active signing key, which cannot be withdrawn: rotate to another key first.
func Withdraw(ctx context.Context, pool *pgxpool.Pool, keyID, activeID, actorUserID, reason string) error {
	keyID, actorUserID, reason = strings.TrimSpace(keyID), strings.TrimSpace(actorUserID), strings.TrimSpace(reason)
	switch {
	case keyID == "":
		return errors.New("withdraw mint key: key id is required")
	case keyID == strings.TrimSpace(activeID):
		return fmt.Errorf("withdraw mint key: %q is the active signing key; rotate to another key first", keyID)
	case actorUserID == "":
		return errors.New("withdraw mint key: actor user id is required")
	case reason == "":
		return errors.New("withdraw mint key: reason is required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("withdraw mint key: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.mint_withdrawn_keys (key_id, actor_user_id, reason)
		VALUES ($1, $2, $3) ON CONFLICT (key_id) DO NOTHING`, keyID, actorUserID, reason); err != nil {
		return fmt.Errorf("withdraw mint key: record: %w", err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_notify($1, '')`, channel); err != nil {
		return fmt.Errorf("withdraw mint key: notify: %w", err)
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("withdraw mint key: commit: %w", err)
	}
	return nil
}

// Watcher keeps a Provider's withdrawn set equal to the stored one.
type Watcher struct {
	pool, direct *pgxpool.Pool
	provider     Provider
}

// NewWatcher reads with pool and holds its LISTEN session on direct (db.DB.Direct).
func NewWatcher(pool, direct *pgxpool.Pool, provider Provider) *Watcher {
	return &Watcher{pool: pool, direct: direct, provider: provider}
}

// ListenFor runs one LISTEN session of at most d. The stored set is applied when the session
// starts (nothing withdrawn while no session was attached is missed) and on every notification.
func (w *Watcher) ListenFor(ctx context.Context, d time.Duration) error {
	session, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := w.listen(session)
	if session.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (w *Watcher) listen(ctx context.Context) error {
	conn, err := w.direct.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("mint withdrawal listener: acquire: %w", err)
	}
	defer conn.Release()
	if _, err = conn.Exec(ctx, "LISTEN "+channel); err != nil {
		_ = conn.Conn().Close(context.Background())
		return fmt.Errorf("mint withdrawal listener: listen: %w", err)
	}
	for {
		if err = w.Apply(ctx); err != nil {
			return err
		}
		if _, err = conn.Conn().WaitForNotification(ctx); err != nil {
			// A connection left mid-wait must not go back to the pool.
			_ = conn.Conn().Close(context.Background())
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("mint withdrawal listener: wait: %w", err)
		}
	}
}

// Apply loads the stored withdrawn keys and hands them to the provider.
func (w *Watcher) Apply(ctx context.Context) error {
	rows, err := w.pool.Query(ctx, `SELECT key_id FROM paperboat.mint_withdrawn_keys ORDER BY key_id LIMIT $1`, maxKeys+1)
	if err != nil {
		return fmt.Errorf("load withdrawn mint keys: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return fmt.Errorf("load withdrawn mint keys: %w", err)
		}
		ids = append(ids, id)
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("load withdrawn mint keys: %w", err)
	}
	if len(ids) > maxKeys {
		return fmt.Errorf("load withdrawn mint keys: more than %d stored; edges refuse a larger set", maxKeys)
	}
	if err = w.provider.Withdraw(ids); err != nil {
		return fmt.Errorf("apply withdrawn mint keys: %w", err)
	}
	return nil
}
