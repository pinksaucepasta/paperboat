package usermachines

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// A machine proof's nonce is used once on the machine-control path, in the signer namespace every
// machine-proof verifier shares; a request HTTP may resend (Idempotency-Key, GET) is not reserved.
func TestMachineProofNonceIsUsedOnceOnPostgres(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	service := New(store, audit.NewWriter(store), Policy{}, nil)
	machine := fmt.Sprintf("machine_nonce_%d", time.Now().UnixNano())
	claims := MachineProofClaims{Nonce: "n0nce-" + fmt.Sprint(time.Now().UnixNano()), ExpiresAt: time.Now().Add(time.Minute)}
	if err := service.reserveMachineProofNonce(ctx, machine, claims, http.MethodPost); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	if err := service.reserveMachineProofNonce(ctx, machine, claims, http.MethodPost); ControlRejectionReason(err) != "proof_nonce_replayed" {
		t.Fatalf("second use = %v, want proof_nonce_replayed", err)
	}
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		if err := service.reserveMachineProofNonce(ctx, machine, MachineProofClaims{ExpiresAt: claims.ExpiresAt}, method); ControlRejectionReason(err) != "proof_nonce_replayed" {
			t.Fatalf("%s proof without a nonce = %v, want a refusal", method, err)
		}
	}
	if err := service.reserveMachineProofNonce(ctx, machine, MachineProofClaims{Nonce: "bad nonce!", ExpiresAt: claims.ExpiresAt}, http.MethodPost); ControlRejectionReason(err) != "proof_nonce_replayed" {
		t.Fatalf("malformed nonce = %v, want a refusal", err)
	}
}
