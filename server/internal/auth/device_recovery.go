package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

// recoverDeviceTokens recovers only the original still-active credential pair.
// The caller holds the device grant lock; the device code is the recovery secret.
func (s *DeviceService) recoverDeviceTokens(ctx context.Context, tx *db.Tx, grantID, userID string, now time.Time) (TokenSet, error) {
	var ciphertext []byte
	var sessionID string
	var accessExpiry, expiry time.Time
	err := tx.QueryRow(ctx, `SELECT response_ciphertext,cli_client_session_id,access_expires_at,expires_at FROM device_token_redemptions WHERE grant_id=$1`, grantID).Scan(&ciphertext, &sessionID, &accessExpiry, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSet{}, &DeviceError{Code: "invalid_grant"}
	}
	if err != nil {
		return TokenSet{}, err
	}
	if !now.Before(expiry) || accessExpiry.Sub(now) < time.Second {
		return TokenSet{}, &DeviceError{Code: "expired_token"}
	}
	var status string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR SHARE`, userID).Scan(&status)
	if err != nil {
		return TokenSet{}, err
	}
	if status != "active" {
		return TokenSet{}, &DeviceError{Code: "access_denied"}
	}
	if err := s.takeRate(ctx, "account:"+userID, s.cfg.AccountActionsPerMinute); err != nil {
		return TokenSet{}, err
	}
	plaintext, err := secrets.DecryptBytes([]byte(s.authorizationEncryptionKey), ciphertext)
	if err != nil {
		return TokenSet{}, err
	}
	defer clear(plaintext)
	var out TokenSet
	if err := json.Unmarshal(plaintext, &out); err != nil {
		return TokenSet{}, err
	}
	if out.CLIClientSessionID != sessionID {
		return TokenSet{}, errors.New("device recovery binding is invalid")
	}
	if _, err := tx.Queries().DeviceReplaySession(ctx, dbsqlc.DeviceReplaySessionParams{SessionID: sessionID, UserID: userID, AccessHashes: s.hashList(out.AccessToken), RefreshHashes: s.hashList(out.RefreshToken), NowAt: now}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TokenSet{}, &DeviceError{Code: "invalid_grant"}
		}
		return TokenSet{}, err
	}
	out.ExpiresIn = int(accessExpiry.Sub(now) / time.Second)
	return out, nil
}

// Cancel prevents approval or consumption, including a token response lost in transit.
func (s *DeviceService) Cancel(ctx context.Context, in DeviceTokenInput) error {
	if in.ClientID != s.cfg.ClientID {
		return &DeviceError{Code: "invalid_client"}
	}
	if in.DeviceCode == "" {
		return &DeviceError{Code: "invalid_grant"}
	}
	if err := s.takeRate(ctx, "network:"+s.hash(in.Network), s.cfg.NetworkRequestsPerMinute); err != nil {
		return err
	}
	if err := s.takeRate(ctx, "grant:"+s.hash(in.DeviceCode), s.cfg.GrantPollsPerMinute); err != nil {
		return err
	}
	var sid string
	err := s.inTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		grant, err := tx.Queries().GetDeviceGrantForPoll(ctx, s.hashList(in.DeviceCode))
		if errors.Is(err, sql.ErrNoRows) {
			return &DeviceError{Code: "invalid_grant"}
		}
		if err != nil {
			return err
		}
		if grant.ClientID != in.ClientID || !s.matchesHash(grant.DeviceCodeHash, in.DeviceCode) {
			return &DeviceError{Code: "invalid_grant"}
		}
		if err := tx.QueryRow(ctx, `SELECT coalesce(cli_client_session_id,'') FROM device_grants WHERE id=$1`, grant.ID).Scan(&sid); err != nil {
			return err
		}
		if sid != "" {
			if err := s.revokeTx(ctx, tx, sid, "login_cancelled"); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE device_grants SET state='denied',denied_at=coalesce(denied_at,$2),version=version+1 WHERE id=$1 AND state IN ('pending','approved')`, grant.ID, s.now()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM device_token_redemptions WHERE grant_id=$1`, grant.ID)
		return err
	})
	if err != nil {
		return err
	}
	if sid != "" && s.downstream != nil {
		return s.downstream.RevokeClientSessions(ctx, sid, "login_cancelled")
	}
	return nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
