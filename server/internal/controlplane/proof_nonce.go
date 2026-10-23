package controlplane

import (
	"context"
	"net/http"
	"regexp"
	"time"
)

// PLAN F21-13 (Lane X #16): a signed machine or helper proof with a nonce is used once. The time
// window is checked first (a stale proof is clock_skew), then the nonce is reserved until the
// proof expires; a second use is a plain proof denial. Requests that HTTP itself may send again
// (GET/HEAD, or any request with an Idempotency-Key, which Go's transport re-sends after a
// dropped connection) are idempotent by design and are not reserved, so such a re-send is never
// mistaken for a replay. Proofs without a nonce (clients before the nonce) are accepted as before.

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

// reserveProofNonce records signer's nonce until expires; a nonce seen before is ErrHelperProof.
func (s *EnrollmentService) reserveProofNonce(ctx context.Context, signer, nonce, method string, expires time.Time) error {
	if nonce == "" || resendable(ctx, method) {
		return nil
	}
	if !proofNoncePattern.MatchString(nonce) {
		return ErrHelperProof
	}
	db := s.store.SQL()
	if _, err := db.ExecContext(ctx, `DELETE FROM paperboat.proof_nonces WHERE signer=$1 AND expires_at<now()`, signer); err != nil {
		return err
	}
	result, err := db.ExecContext(ctx, `INSERT INTO paperboat.proof_nonces(signer,nonce,expires_at) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, signer, nonce, expires)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return ErrHelperProof
	}
	return nil
}

// PruneProofNonces deletes reservations past their proof's expiry.
func (s *EnrollmentService) PruneProofNonces(ctx context.Context) error {
	_, err := s.store.SQL().ExecContext(ctx, `DELETE FROM paperboat.proof_nonces WHERE expires_at<now()`)
	return err
}
