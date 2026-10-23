package controlplane

import (
	"context"
	"time"
)

const (
	// configSecretGrace keeps an expired row (without its ciphertext) long enough for a late retry of
	// the same operation to be answered as a replay rather than issuing a new credential.
	configSecretGrace = time.Hour
	// configSecretPendingRetention bounds repository-access operations that never issued a token.
	configSecretPendingRetention = 24 * time.Hour
	configSecretPruneBatch       = 500
)

// pruneExpiredConfigSecrets clears the encrypted config-sync credential and repository-access token of
// every row past its expiry, and deletes rows an hour later (audit D2: expired ciphertexts were kept
// forever). Replays already refuse expired rows before decrypting, and provider revocation only acts
// on unexpired tokens, so nothing reads an expired ciphertext. Bounded batches; rows locked by another
// instance are skipped. Prior art: Headscale hard-deletes expired OAuth tokens
// (hscontrol/db/oauth.go:282).
func (s *ConfigRepositoryAccessService) pruneExpiredConfigSecrets(ctx context.Context) error {
	now := s.clock().UTC()
	statements := []string{
		`UPDATE paperboat.control_config_credentials SET credential_ciphertext=NULL WHERE jti IN (
 SELECT jti FROM paperboat.control_config_credentials WHERE credential_ciphertext IS NOT NULL AND expires_at<=$1::timestamptz
 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM paperboat.control_config_credentials WHERE jti IN (
 SELECT jti FROM paperboat.control_config_credentials WHERE expires_at<=$1::timestamptz-$3::interval
 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`,
		`UPDATE paperboat.control_config_repository_access_operations SET access_ciphertext=NULL, updated_at=$1 WHERE operation_id IN (
 SELECT operation_id FROM paperboat.control_config_repository_access_operations WHERE access_ciphertext IS NOT NULL AND expires_at<=$1::timestamptz
 ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)`,
		`DELETE FROM paperboat.control_config_repository_access_operations WHERE operation_id IN (
 SELECT operation_id FROM paperboat.control_config_repository_access_operations
 WHERE (expires_at IS NOT NULL AND expires_at<=$1::timestamptz-$3::interval) OR (expires_at IS NULL AND created_at<=$1::timestamptz-$4::interval)
 LIMIT $2 FOR UPDATE SKIP LOCKED)`,
	}
	grace, pending := configSecretGrace.String(), configSecretPendingRetention.String()
	for i, statement := range statements {
		args := []any{now, configSecretPruneBatch}
		switch i {
		case 1:
			args = append(args, grace)
		case 3:
			args = append(args, grace, pending)
		}
		if _, err := s.store.SQL().ExecContext(ctx, statement, args...); err != nil {
			return err
		}
	}
	return nil
}
