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

// Support support_fb7b442e (2026-11-05 12:08:16Z): two concurrent signed requests of one edge each
// deleted the signer's expired nonces and deadlocked (503 carrier_unavailable). Reserving a nonce
// touches only its own row, so a lock another transaction holds on any other row never blocks it.
func TestProofNonceReserveLocksOnlyItsOwnRowPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	s := NewEnrollmentService(store, nil, nil, "https://api.example.test", "proof-nonce-encryption-key")
	signer := fmt.Sprintf("helper:nonce_lock_%d", time.Now().UnixNano())
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.proof_nonces(signer,nonce,expires_at) VALUES($1,'expired-nonce-abcdefgh',now()-interval '1 second')`, signer); err != nil {
		t.Fatal(err)
	}
	holder, err := store.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback() }()
	if _, err := holder.ExecContext(ctx, `SELECT 1 FROM paperboat.proof_nonces WHERE signer=$1 FOR UPDATE`, signer); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := s.reserveProofNonce(bounded, signer, "fresh-nonce-abcdefghij", http.MethodPost, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("reserve while another row of the signer is locked: %v", err)
	}
	// Pruning skips a locked row instead of waiting for it, so concurrent prunes never deadlock either.
	if err := s.PruneProofNonces(bounded); err != nil {
		t.Fatalf("prune while a row is locked: %v", err)
	}
}
