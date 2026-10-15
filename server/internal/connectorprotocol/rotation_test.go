package connectorprotocol

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

func rotationKey(seedByte byte) (ed25519.PrivateKey, string, string) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string([]byte{seedByte}), ed25519.SeedSize)))
	public := private.Public().(ed25519.PublicKey)
	thumbprint, _ := wire.IdentityThumbprint(public)
	return private, "ed25519:" + thumbprint, thumbprint
}

func rotationProofFor(t *testing.T, challenge wire.CredentialRotationChallenge, oldPrivate, newPrivate ed25519.PrivateKey, reference string, issuedAt time.Time) wire.CredentialRotationProof {
	t.Helper()
	newPublic := newPrivate.Public().(ed25519.PublicKey)
	newKeyID, _ := wire.IdentityKeyID(newPublic)
	newThumbprint, _ := wire.IdentityThumbprint(newPublic)
	proof := wire.CredentialRotationProof{
		AccountID: challenge.AccountID, TunnelID: challenge.TunnelID, OperationID: challenge.OperationID,
		ConnectorID: challenge.ConnectorID, HostID: challenge.HostID, SessionID: challenge.SessionID, ProcessGeneration: challenge.ProcessGeneration,
		TargetSetHash: challenge.TargetSetHash, OldCredentialGeneration: challenge.OldCredentialGeneration,
		NewCredentialGeneration: challenge.NewCredentialGeneration, OldIdentityKeyID: challenge.OldIdentityKeyID,
		OldIdentityKeyThumbprint: challenge.OldIdentityKeyThumbprint, NewIdentityKeyID: newKeyID,
		NewIdentityKeyThumbprint: newThumbprint, NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublic),
		NewCredentialReference: reference, ChallengeNonce: challenge.ChallengeNonce, IssuedAt: issuedAt, NewCredentialValidUntil: challenge.NewCredentialValidUntil,
	}
	proof, err := wire.SignCredentialRotationProof(proof, func(payload []byte) []byte { return ed25519.Sign(oldPrivate, payload) }, func(payload []byte) []byte { return ed25519.Sign(newPrivate, payload) })
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func rotationVerifier(keys map[string]ed25519.PrivateKey) wire.RotationOldProofVerifier {
	return func(_ context.Context, proof wire.CredentialRotationProof, payload, signature []byte) error {
		private, ok := keys[proof.OldIdentityKeyID]
		if !ok || !ed25519.Verify(private.Public().(ed25519.PublicKey), payload, signature) {
			return wire.ErrAuthenticationFailed
		}
		return nil
	}
}
