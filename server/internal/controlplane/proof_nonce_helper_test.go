package controlplane

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync/atomic"
)

var proofTestNonceCounter atomic.Int64

// proofTestNonce is a fresh well-formed one-use nonce: every machine and helper proof must carry one.
func proofTestNonce() string { return fmt.Sprintf("n0nce-test-%012d", proofTestNonceCounter.Add(1)) }

// resignHelperProof signs the same claims again with a fresh nonce, like a client retrying after a lost response.
func resignHelperProof(claims HelperProofClaims, key ed25519.PrivateKey) []byte {
	claims.Nonce = proofTestNonce()
	payload, _ := json.Marshal(claims)
	proof, _ := json.Marshal(helperProofEnvelope{Algorithm: "EdDSA", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))})
	return proof
}
