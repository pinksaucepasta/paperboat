package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
)

var ErrWorkOSTemporarilyUnavailable = errors.New("workos session could not be refreshed")

// refreshWorkOSSession serializes refresh-token rotation across server processes. A
// session row is locked until the replacement token is durable, so a parallel
// request cannot reuse an already-rotated token or resurrect a revoked session.
func (s *Service) refreshWorkOSSession(ctx context.Context, sessionHash, subject, sessionID string) error {
	refresher, ok := s.verifier.(interface {
		RefreshSession(context.Context, string) (WorkOSProfile, error)
	})
	if !ok {
		return ErrWorkOSTemporarilyUnavailable
	}
	tx, err := s.db.SQL().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	var userID string
	var ciphertext []byte
	var expiresAt sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.workos_refresh_token_ciphertext, s.workos_access_expires_at
		FROM paperboat.sessions s JOIN paperboat.users u ON u.id=s.user_id
		WHERE s.session_hash=$1 AND s.workos_session_id=$2 AND coalesce(s.workos_subject,u.workos_subject)=$3
		AND u.status='active' AND s.revoked_at IS NULL AND s.expires_at>now() FOR UPDATE OF s`, sessionHash, sessionID, subject).Scan(&id, &userID, &ciphertext, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUnauthenticated
	}
	if err != nil {
		return err
	}
	if len(ciphertext) == 0 || !expiresAt.Valid {
		return ErrUnauthenticated
	}
	if s.now().Add(10 * time.Second).Before(expiresAt.Time) {
		return tx.Commit()
	}
	refreshToken, err := secrets.Decrypt(s.providerEncryptionKey, ciphertext)
	if err != nil {
		return fmt.Errorf("decrypt workos refresh credential: %w", err)
	}
	providerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var profile WorkOSProfile
	for attempt := 0; attempt < 2; attempt++ {
		profile, err = refresher.RefreshSession(providerCtx, refreshToken)
		if err == nil || errors.Is(err, ErrWorkOSInvalidSession) || attempt == 1 {
			break
		}
		select {
		case <-providerCtx.Done():
			return errors.Join(ErrWorkOSTemporarilyUnavailable, providerCtx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
	if errors.Is(err, ErrWorkOSInvalidSession) {
		if _, revokeErr := tx.ExecContext(ctx, `UPDATE paperboat.sessions SET revoked_at=now(), workos_refresh_token_ciphertext=NULL, updated_at=now(), version=version+1 WHERE id=$1`, id); revokeErr != nil {
			return revokeErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		return ErrUnauthenticated
	}
	if err != nil {
		return errors.Join(ErrWorkOSTemporarilyUnavailable, err, providerCtx.Err())
	}
	if profile.Subject != subject || profile.SessionID != sessionID || strings.TrimSpace(profile.Email) == "" || profile.Impersonator ||
		profile.RefreshToken == "" || !s.now().Before(profile.AccessExpiresAt) {
		if _, revokeErr := tx.ExecContext(ctx, `UPDATE paperboat.sessions SET revoked_at=now(), workos_refresh_token_ciphertext=NULL, updated_at=now(), version=version+1 WHERE id=$1`, id); revokeErr != nil {
			return revokeErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		return ErrUnauthenticated
	}
	replacement, err := secrets.Encrypt(s.providerEncryptionKey, profile.RefreshToken)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE paperboat.sessions SET workos_refresh_token_ciphertext=$1,
		workos_access_expires_at=$2, updated_at=now(), version=version+1 WHERE id=$3`, replacement, profile.AccessExpiresAt, id); err != nil {
		return err
	}
	// The verified email is an authorization input for shared browser resources.
	// Refresh it atomically with provider credentials before this request continues.
	if _, err = tx.ExecContext(ctx, `UPDATE paperboat.users SET canonical_email=CASE WHEN canonical_email IS NULL THEN NULL ELSE paperboat.canonical_email_address($1) END, primary_email=paperboat.canonical_email_address($1), email_verified=$2,
		updated_at=now(), version=version+1 WHERE id=$3`, strings.ToLower(strings.TrimSpace(profile.Email)), profile.EmailVerified, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE paperboat.user_identities SET email=$1, updated_at=now()
		WHERE user_id=$2 AND provider='workos' AND provider_subject=$3`, strings.ToLower(strings.TrimSpace(profile.Email)), userID, subject); err != nil {
		return err
	}
	return tx.Commit()
}
