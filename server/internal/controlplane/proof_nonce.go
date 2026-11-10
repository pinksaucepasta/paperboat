package controlplane

import (
	"context"
	"time"
)

// reserveProofNonce records signer's nonce until expires (db.ReserveProofNonce); a replay is ErrHelperProof and
// a missing nonce is the same error with its own reason.
func (s *EnrollmentService) reserveProofNonce(ctx context.Context, signer, nonce, method string, expires time.Time) error {
	if nonce == "" {
		return proofRejected("proof_nonce_missing")
	}
	ok, err := s.store.ReserveProofNonce(ctx, signer, nonce, method, expires)
	if err != nil {
		return err
	}
	if !ok {
		return ErrHelperProof
	}
	return nil
}

// PruneProofNonces deletes reservations past their proof's expiry.
func (s *EnrollmentService) PruneProofNonces(ctx context.Context) error {
	return s.store.PruneProofNonces(ctx)
}
