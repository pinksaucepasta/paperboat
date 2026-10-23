package auth

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// ClientTokenPolicy is shared by browser approval and installation bootstrap.
// Both approval methods issue ordinary CLI sessions with the same credential policy.
type ClientTokenPolicy struct {
	HashKey                         []byte
	AccessLifetime, RefreshLifetime time.Duration
}

func (s *DeviceService) tokenPolicy() ClientTokenPolicy {
	return ClientTokenPolicy{HashKey: s.keys[0], AccessLifetime: s.cfg.AccessTokenLifetime, RefreshLifetime: s.cfg.RefreshTokenLifetime}
}

// CreateClientSessionTx is the sole writer for newly authorized CLI sessions.
// Resource approval belongs to the caller; account eligibility and tokens belong here.
func CreateClientSessionTx(ctx context.Context, tx *db.Tx, session dbsqlc.CreateClientSessionParams, access, refresh string, policy ClientTokenPolicy) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, session.UserID).Scan(&status); err != nil {
		return err
	}
	if status != "active" {
		return &DeviceError{Code: "access_denied"}
	}
	if err := tx.Queries().CreateClientSession(ctx, session); err != nil {
		return err
	}
	return IssueClientTokensTx(ctx, tx, session.ID, access, refresh, session.CreatedAt, policy)
}

// IssueClientTokensTx persists one credential pair for an already-authorized session.
// Rotation or recovery callers hold the corresponding session lock and retire old tokens.
func IssueClientTokensTx(ctx context.Context, tx *db.Tx, sessionID, access, refresh string, now time.Time, policy ClientTokenPolicy) error {
	if len(policy.HashKey) == 0 || policy.AccessLifetime <= 0 || policy.RefreshLifetime <= 0 || access == "" || refresh == "" {
		return errors.New("CLI credential policy is not configured")
	}
	if err := tx.Queries().CreateClientAccessToken(ctx, dbsqlc.CreateClientAccessTokenParams{TokenHash: hashWithKey(policy.HashKey, access), CLIClientSessionID: sessionID, ExpiresAt: now.Add(policy.AccessLifetime), CreatedAt: now}); err != nil {
		return err
	}
	return tx.Queries().CreateClientRefreshToken(ctx, dbsqlc.CreateClientRefreshTokenParams{TokenHash: hashWithKey(policy.HashKey, refresh), CLIClientSessionID: sessionID, ExpiresAt: now.Add(policy.RefreshLifetime), CreatedAt: now})
}
