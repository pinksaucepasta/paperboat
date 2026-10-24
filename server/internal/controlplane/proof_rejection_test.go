package controlplane

import (
	"errors"
	"testing"
)

// A rejected machine proof carries a fixed reason code and is still ErrHelperProof for every caller.
func TestProofRejectionKeepsItsReasonAndIsAHelperProofError(t *testing.T) {
	err := error(proofRejected("installation_generation_moved"))
	if !errors.Is(err, ErrHelperProof) || ProofRejectionReason(err) != "installation_generation_moved" {
		t.Fatalf("rejection=%v reason=%q", err, ProofRejectionReason(err))
	}
	if ProofRejectionReason(ErrHelperProof) != "proof_rejected" || ProofRejectionReason(errors.New("db down")) != "" {
		t.Fatal("bare sentinel or unrelated error mis-reported")
	}
}
