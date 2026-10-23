package apitokens

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

// PLAN F21-12 (Lane X #16): POST api-tokens with an Idempotency-Key. A client that lost the
// response retries with the same key and gets the same token and once-shown secret, instead of a
// 409 for the taken name and a secret nobody saw.

var (
	ErrInvalidIdempotencyKey = errors.New("Idempotency-Key must be 8-128 letters, digits, - or _")
	// ErrIdempotencyConflict: the key was already used for a different create request.
	ErrIdempotencyConflict = errors.New("this Idempotency-Key was already used for a different API token")
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// createRequestHash binds a key to the request it was first used with.
func createRequestHash(ws workspace.Scope, in CreateInput) []byte {
	raw, _ := json.Marshal(struct {
		Workspace string          `json:"workspace"`
		Name      string          `json:"name"`
		Days      int             `json:"days"`
		Scope     json.RawMessage `json:"scope"`
	}{ws.ID, in.Name, in.ExpiresInDays, in.Scope})
	sum := sha256.Sum256(raw)
	return sum[:]
}

// replayCreateTx returns the token and secret of an earlier create with the same key in the retry
// window ("" when there is none). It runs under the actor's row lock, so concurrent retries see it.
func (s *Service) replayCreateTx(ctx context.Context, tx *db.Tx, actor, key string, requestHash []byte) (Token, string, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM api_token_create_requests WHERE created_at <= now()-make_interval(secs=>$1)`, s.limits.RetrySeconds); err != nil {
		return Token{}, "", err
	}
	var hash, sealed []byte
	var id string
	err := tx.QueryRow(ctx, `SELECT request_hash,token_id,secret_ciphertext FROM api_token_create_requests WHERE account_id=$1 AND idempotency_key=$2`,
		actor, key).Scan(&hash, &id, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, "", nil
	}
	if err != nil {
		return Token{}, "", err
	}
	if string(hash) != string(requestHash) {
		return Token{}, "", ErrIdempotencyConflict
	}
	secret, err := secrets.Decrypt(s.key, sealed)
	if err != nil {
		return Token{}, "", err
	}
	token, err := getTx(ctx, tx, id, false)
	return token, secret, err
}

// rememberCreateTx keeps the new token's secret, encrypted, for the retry window.
func (s *Service) rememberCreateTx(ctx context.Context, tx *db.Tx, actor, key string, requestHash []byte, id, secret string) error {
	sealed, err := secrets.Encrypt(s.key, secret)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO api_token_create_requests(account_id,idempotency_key,request_hash,token_id,secret_ciphertext) VALUES($1,$2,$3,$4,$5)`,
		actor, key, requestHash, id, sealed)
	return err
}

// PruneCreateRequests deletes held secrets past the retry window; a periodic worker calls it so
// none outlives the window when no further creates happen.
func (s *Service) PruneCreateRequests(ctx context.Context) error {
	_, err := s.db.SQL().ExecContext(ctx, `DELETE FROM paperboat.api_token_create_requests WHERE created_at <= now()-make_interval(secs=>$1)`, s.limits.RetrySeconds)
	return err
}
