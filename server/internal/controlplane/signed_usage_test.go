package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestVerifySignedUsageEnvelope(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Unix(1, 0).UTC()
	request := edgeUsageRequest{OperationID: "op_signed_01", Node: "node_1", Epoch: "epoch_1", Environment: "env_1", Route: "route_1", Revision: 2, Direction: "egress", Bytes: 42, Start: start, End: start.Add(time.Second)}
	document := signedUsageDocument{OperationID: request.OperationID, Key: signedUsageKey{Node: request.Node, Epoch: request.Epoch, Environment: request.Environment, Route: request.Route, Direction: request.Direction, Revision: request.Revision}, Bytes: request.Bytes, Start: request.Start, End: request.End}
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	envelope := signedUsageEnvelope{Algorithm: "EdDSA", KeyID: "usage-key-1", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))}
	if err := verifySignedUsageEnvelope(publicKey, envelope, request); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}

	tamperedAuthority := request
	tamperedAuthority.AuthorityID = "grant:other-team"
	if err := verifySignedUsageEnvelope(publicKey, envelope, tamperedAuthority); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("unsigned authority accepted: %v", err)
	}
	tampered := request
	tampered.Bytes++
	if err := verifySignedUsageEnvelope(publicKey, envelope, tampered); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("relabeled report error = %v", err)
	}
	envelope.Signature = base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	if err := verifySignedUsageEnvelope(publicKey, envelope, request); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("tampered signature error = %v", err)
	}
}

func TestVerifySignedUsageEnvelopeRejectsUnknownFields(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"operation_id":"op","key":{},"bytes":0,"interval_start":"2026-01-01T00:00:00Z","interval_end":"2026-01-01T00:00:00Z","unknown":true}`)
	envelope := signedUsageEnvelope{Algorithm: "EdDSA", KeyID: "key", Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))}
	if err := verifySignedUsageEnvelope(publicKey, envelope, edgeUsageRequest{}); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("unknown field error = %v", err)
	}
}

// A verification key lookup that failed (the server was at deadline_exceeded when the prod edge got its usage 400 at
// 2026-11-16 04:33:22Z) is a retryable 503, never the permanent 400 the edge dead-letters; only a missing key refuses.
func TestUsageVerificationKeyLookupFailureIsRetryable(t *testing.T) {
	key := make([]byte, ed25519.PublicKeySize)
	for _, tc := range []struct {
		name   string
		key    []byte
		err    error
		status int
	}{
		{"deadline", nil, context.DeadlineExceeded, http.StatusServiceUnavailable},
		{"no key", nil, pgx.ErrNoRows, http.StatusBadRequest},
		{"short key", key[:5], nil, http.StatusBadRequest},
	} {
		err := verificationKeyLookupError(tc.key, tc.err)
		if status, _, _, _ := usageErrorResponse(err); status != tc.status {
			t.Errorf("%s: status %d (%v), want %d", tc.name, status, err, tc.status)
		}
	}
	if err := verificationKeyLookupError(key, nil); err != nil {
		t.Fatalf("active key refused: %v", err)
	}
}
