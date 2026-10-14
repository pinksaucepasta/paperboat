package auth

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
)

func validRefreshAttemptID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == strings.ToLower(value)
}

type refreshRedemption struct {
	TokenHash, AttemptHash string
	Tokens                 TokenSet
}

func (s *DeviceService) storeRefreshTokens(ctx context.Context, tx *db.Tx, tokenHash, attempt string, tokens TokenSet, now, expiry time.Time) error {
	attemptHash := s.hash("refresh-attempt:" + attempt)
	body, err := json.Marshal(refreshRedemption{TokenHash: tokenHash, AttemptHash: attemptHash, Tokens: tokens})
	if err != nil {
		return err
	}
	defer clear(body)
	ciphertext, err := secrets.EncryptBytes([]byte(s.authorizationEncryptionKey), body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO client_refresh_redemptions(token_hash,attempt_hash,cli_client_session_id,response_ciphertext,access_expires_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, tokenHash, attemptHash, tokens.CLIClientSessionID, ciphertext, now.Add(s.cfg.AccessTokenLifetime), expiry)
	return err
}

// The caller holds the original refresh token and session locks. Recovery never
// rotates again, extends expiry or resurrects a revoked account/session/token.
func (s *DeviceService) recoverRefreshTokens(ctx context.Context, tx *db.Tx, tokenHash, sessionID, attempt string) (TokenSet, bool, error) {
	var ciphertext []byte
	var attemptHash, storedSession string
	var accessExpiry, expiry time.Time
	err := tx.QueryRow(ctx, `SELECT attempt_hash,cli_client_session_id,response_ciphertext,access_expires_at,expires_at FROM client_refresh_redemptions WHERE token_hash=$1`, tokenHash).Scan(&attemptHash, &storedSession, &ciphertext, &accessExpiry, &expiry)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenSet{}, false, nil
	}
	if err != nil {
		return TokenSet{}, false, err
	}
	if !s.matchesHash(attemptHash, "refresh-attempt:"+attempt) {
		return TokenSet{}, false, nil
	}
	now := s.now()
	if storedSession != sessionID || !now.Before(expiry) {
		return TokenSet{}, true, ErrUnauthenticated
	}
	body, err := secrets.DecryptBytes([]byte(s.authorizationEncryptionKey), ciphertext)
	if err != nil {
		return TokenSet{}, true, err
	}
	defer clear(body)
	var redemption refreshRedemption
	if err := json.Unmarshal(body, &redemption); err != nil {
		return TokenSet{}, true, err
	}
	out := redemption.Tokens
	if redemption.TokenHash != tokenHash || redemption.AttemptHash != attemptHash || out.CLIClientSessionID != sessionID {
		return TokenSet{}, true, errors.New("refresh recovery binding is invalid")
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cli_client_sessions cs JOIN users u ON u.id=cs.user_id JOIN cli_refresh_tokens rt ON rt.cli_client_session_id=cs.id WHERE cs.id=$1 AND cs.state='active' AND u.status='active' AND rt.token_hash=ANY(string_to_array($2,' ')) AND rt.state='active' AND rt.expires_at>$3)`, sessionID, s.hashList(out.RefreshToken), now).Scan(&active)
	if err != nil {
		return TokenSet{}, true, err
	}
	if !active {
		return TokenSet{}, true, ErrUnauthenticated
	}
	out.ExpiresIn = max(0, int(accessExpiry.Sub(now)/time.Second))
	return out, true, nil
}
