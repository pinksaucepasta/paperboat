package controlplane

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"net/http"
	"testing"
	"time"
)

// F21-13 (Lane X #16): a proof nonce is used once until the proof expires; requests HTTP may send
// again (GET/HEAD, Idempotency-Key) are not reserved, and proofs without a nonce pass as before.
func TestProofNonceIsUsedOnce(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	s := NewEnrollmentService(store, nil, nil, "https://api.example.test", "proof-nonce-encryption-key")
	signer := fmt.Sprintf("machine:nonce_test_%d", time.Now().UnixNano())
	expires := time.Now().Add(time.Minute)
	nonce := "n0nce-abcdefghijklmnop"
	if err := s.reserveProofNonce(ctx, signer, nonce, http.MethodPost, expires); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if err := s.reserveProofNonce(ctx, signer, nonce, http.MethodPost, expires); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("replay: %v", err)
	}
	if err := s.reserveProofNonce(ctx, "machine:other", nonce, http.MethodPost, expires); err != nil {
		t.Fatalf("same nonce, other signer: %v", err)
	}
	for name, try := range map[string]func() error{
		"GET": func() error { return s.reserveProofNonce(ctx, signer, nonce, http.MethodGet, expires) },
		"Idempotency-Key": func() error {
			return s.reserveProofNonce(db.WithResendableRequest(ctx), signer, nonce, http.MethodPost, expires)
		},
		"no nonce": func() error { return s.reserveProofNonce(ctx, signer, "", http.MethodPost, expires) },
	} {
		if err := try(); err != nil {
			t.Fatalf("%s re-send refused: %v", name, err)
		}
	}
	if err := s.reserveProofNonce(ctx, signer, "short", http.MethodPost, expires); !errors.Is(err, ErrHelperProof) {
		t.Fatalf("malformed nonce: %v", err)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.proof_nonces SET expires_at=now()-interval '1 second' WHERE signer=$1`, signer); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneProofNonces(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.proof_nonces WHERE signer=$1`, signer).Scan(&left); err != nil || left != 0 {
		t.Fatalf("pruned: %d %v", left, err)
	}
}
