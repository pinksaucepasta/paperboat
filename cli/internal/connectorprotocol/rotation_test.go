package connectorprotocol

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

func rotationKey(seedByte byte) (ed25519.PrivateKey, string, string) {
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat(string([]byte{seedByte}), ed25519.SeedSize)))
	public := private.Public().(ed25519.PublicKey)
	thumbprint, _ := IdentityThumbprint(public)
	return private, "ed25519:" + thumbprint, thumbprint
}

func rotationProofFor(t *testing.T, challenge CredentialRotationChallenge, oldPrivate, newPrivate ed25519.PrivateKey, reference string, issuedAt time.Time) CredentialRotationProof {
	t.Helper()
	newPublic := newPrivate.Public().(ed25519.PublicKey)
	newKeyID, _ := IdentityKeyID(newPublic)
	newThumbprint, _ := IdentityThumbprint(newPublic)
	proof := CredentialRotationProof{
		AccountID: challenge.AccountID, TunnelID: challenge.TunnelID, OperationID: challenge.OperationID,
		ConnectorID: challenge.ConnectorID, HostID: challenge.HostID, SessionID: challenge.SessionID, ProcessGeneration: challenge.ProcessGeneration,
		TargetSetHash: challenge.TargetSetHash, OldCredentialGeneration: challenge.OldCredentialGeneration,
		NewCredentialGeneration: challenge.NewCredentialGeneration, OldIdentityKeyID: challenge.OldIdentityKeyID,
		OldIdentityKeyThumbprint: challenge.OldIdentityKeyThumbprint, NewIdentityKeyID: newKeyID,
		NewIdentityKeyThumbprint: newThumbprint, NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublic),
		NewCredentialReference: reference, ChallengeNonce: challenge.ChallengeNonce, IssuedAt: issuedAt, NewCredentialValidUntil: challenge.NewCredentialValidUntil,
	}
	proof, err := SignCredentialRotationProof(proof, func(payload []byte) []byte { return ed25519.Sign(oldPrivate, payload) }, func(payload []byte) []byte { return ed25519.Sign(newPrivate, payload) })
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestCredentialRotationFramesUseStrictWireValidation(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	oldPrivate, oldID, oldThumbprint := rotationKey(51)
	newPrivate, _, _ := rotationKey(52)
	target := RotationTarget{ConnectorID: "connector_1", HostID: "host_1", OldCredentialGeneration: 1, NewCredentialGeneration: 2}
	targetSetHash := "sha256:" + strings.Repeat("a", 64)
	challenge := CredentialRotationChallenge{AccountID: "acct_1", TunnelID: "tunnel_1", OperationID: "op_rotate_frame", ConnectorID: "connector_1", HostID: "host_1", SessionID: "sess_old", ProcessGeneration: 1, TargetSetHash: targetSetHash, Target: target, OldCredentialGeneration: 1, NewCredentialGeneration: 2, OldIdentityKeyID: oldID, OldIdentityKeyThumbprint: oldThumbprint, ChallengeNonce: "nonce-frame-rotation", IssuedAt: now, ExpiresAt: now.Add(time.Minute), OverlapUntil: now.Add(10 * time.Minute), NewCredentialValidUntil: now.Add(365 * 24 * time.Hour)}
	frame, err := NewFrame(MessageCredentialRotationChallenge, "req_rotation", challenge)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CredentialRotationChallenge
	if err := frame.DecodePayload(&decoded); err != nil || decoded.TargetSetHash != targetSetHash {
		t.Fatalf("decoded challenge=%+v err=%v", decoded, err)
	}
	proof := rotationProofFor(t, challenge, oldPrivate, newPrivate, "keychain://paperboat/rotation/connector_1", now.Add(time.Second))
	if _, err := NewFrame(MessageCredentialRotationProof, "req_rotation_proof", proof); err != nil {
		t.Fatal(err)
	}
	bad := proof
	bad.NewIdentityKeyThumbprint = strings.Repeat("a", 43)
	if _, err := NewFrame(MessageCredentialRotationProof, "req_rotation_bad", bad); err == nil {
		t.Fatal("mismatched rotation key accepted")
	}
}

func TestCredentialRotationReadyRejectsStaleReadyAt(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	newPrivate, newID, newThumbprint := rotationKey(73)
	newPublic := newPrivate.Public().(ed25519.PublicKey)
	ready := CredentialRotationReady{
		AccountID: "acct_1", TunnelID: "tunnel_1", OperationID: "op_rotate_ready_stale",
		ConnectorID: "connector_1", HostID: "host_1", SessionID: "sess_new",
		PreviousSessionID: "sess_old", ProcessGeneration: 2, TargetSetHash: "sha256:" + strings.Repeat("a", 64),
		OldCredentialGeneration: 1, NewCredentialGeneration: 2, NewIdentityKeyID: newID,
		NewIdentityKeyThumbprint: newThumbprint, NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublic),
		NewCredentialReference: "keychain://paperboat/rotation/stale-ready", NewCredentialValidUntil: now.Add(time.Hour),
		ConfigGeneration: 2, ConfigContentHash: "sha256:" + strings.Repeat("b", 64),
		EdgeReady: true, RouteReady: true, OriginReady: true, ReadyAt: now.Add(-MaxClockSkew - time.Second),
	}
	if err := ready.Validate(now); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("stale readiness accepted: %v", err)
	}
}
