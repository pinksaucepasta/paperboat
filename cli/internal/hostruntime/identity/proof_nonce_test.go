package identity

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// payloadNonce reads the nonce a signed proof carries.
func payloadNonce(t *testing.T, proof []byte) string {
	t.Helper()
	var envelope struct {
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(proof, &envelope); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Nonce string `json:"nonce"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims.Nonce
}

// Every machine proof carries a nonce and no two proofs share one: the server uses a proof once (F21-13).
func TestMachineProofCarriesAFreshNonce(t *testing.T) {
	store, root, now := newControlCauseStore(t)
	saveControlCauseRegistration(t, store, root, now)
	first, err := store.MachineProof("operation-nonce-one", "POST", "/v1/runtime-observations", []byte("{}"), now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MachineProof("operation-nonce-one", "POST", "/v1/runtime-observations", []byte("{}"), now)
	if err != nil {
		t.Fatal(err)
	}
	a, b := payloadNonce(t, first), payloadNonce(t, second)
	if len(a) < 16 || a == b {
		t.Fatalf("nonces %q and %q: want two distinct values of at least 16 characters", a, b)
	}
}
