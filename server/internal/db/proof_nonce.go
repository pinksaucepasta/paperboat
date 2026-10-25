package db

import (
	"context"
	"net/http"
	"regexp"
	"time"
)

// PLAN F21-13 (Lane X #16): a signed machine or helper proof with a nonce is used once. The nonce is
// reserved until the proof expires; a second use is a replay. Requests that HTTP itself may send again
// (GET/HEAD, or any request with an Idempotency-Key, which Go's transport re-sends after a dropped
// connection) are idempotent by design and are not reserved, so such a re-send is never mistaken for a
// replay. Proofs without a nonce (clients before the nonce) are accepted as before. Every verifier of a
// machine or helper proof uses this one implementation (controlplane and the machine-control renewal).

type resendableKey struct{}

// WithResendableRequest marks a request that carries an Idempotency-Key.
func WithResendableRequest(ctx context.Context) context.Context {
	return context.WithValue(ctx, resendableKey{}, true)
}

func resendable(ctx context.Context, method string) bool {
	marked, _ := ctx.Value(resendableKey{}).(bool)
	return marked || method == http.MethodGet || method == http.MethodHead
}

var proofNoncePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// ReserveProofNonce records signer's nonce until expires. It reports false for a malformed nonce or one
// seen before (a replay); an empty nonce or a resendable request is not reserved and reports true.
func (d *DB) ReserveProofNonce(ctx context.Context, signer, nonce, method string, expires time.Time) (bool, error) {
	if nonce == "" || resendable(ctx, method) {
		return true, nil
	}
	if !proofNoncePattern.MatchString(nonce) {
		return false, nil
	}
	if _, err := d.SQL().ExecContext(ctx, `DELETE FROM paperboat.proof_nonces WHERE signer=$1 AND expires_at<now()`, signer); err != nil {
		return false, err
	}
	result, err := d.SQL().ExecContext(ctx, `INSERT INTO paperboat.proof_nonces(signer,nonce,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, signer, nonce, expires)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return err == nil && n == 1, err
}

// PruneProofNonces deletes reservations past their proof's expiry.
func (d *DB) PruneProofNonces(ctx context.Context) error {
	_, err := d.SQL().ExecContext(ctx, `DELETE FROM paperboat.proof_nonces WHERE expires_at<now()`)
	return err
}
