package connectorprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
)

type connectorVector struct {
	Case               string           `json:"case"`
	Valid              bool             `json:"valid"`
	Type               wire.MessageType `json:"type"`
	RequestID          string           `json:"request_id"`
	Payload            json.RawMessage  `json:"payload"`
	ExpectedGeneration uint64           `json:"expected_generation"`
	ExpectedCanonical  string           `json:"expected_canonical_payload"`
	ExpectedCode       wire.Code        `json:"expected_code"`
	Semantic           string           `json:"semantic,omitempty"`
}

func validateConnectorVectorSemantic(vector connectorVector, seenRotationNonces map[string]struct{}) error {
	switch vector.Semantic {
	case "stale":
		var challenge wire.CredentialRotationChallenge
		if err := json.Unmarshal(vector.Payload, &challenge); err != nil {
			return wire.ErrMalformedFrame
		}
		return challenge.Validate(time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC))
	case "replay":
		var challenge wire.CredentialRotationChallenge
		if err := json.Unmarshal(vector.Payload, &challenge); err != nil {
			return wire.ErrMalformedFrame
		}
		if _, seen := seenRotationNonces[challenge.ChallengeNonce]; seen {
			return wire.CodeError(wire.ErrCredentialRotationRejected, wire.ReasonCredentialRotation, false, errors.New("fixture replay detected"))
		}
		return nil
	case "wrong_session":
		var ack wire.CredentialRotationAck
		if err := json.Unmarshal(vector.Payload, &ack); err != nil {
			return wire.ErrMalformedFrame
		}
		if ack.SessionID != "sess_1" {
			return wire.CodeError(wire.ErrIdentityMismatch, wire.ReasonAuthentication, false, errors.New("fixture session is not bound"))
		}
		return nil
	case "capability_missing_rotation":
		var hello wire.Hello
		if err := json.Unmarshal(vector.Payload, &hello); err != nil {
			return wire.ErrMalformedFrame
		}
		if wire.HasCapability(hello.Capabilities, wire.CapabilityCredentialRotation) {
			return nil
		}
		return wire.CodeError(wire.ErrCapabilityMissing, wire.ReasonCapabilityMissing, false, errors.New("credential rotation capability is absent"))
	default:
		return wire.ErrInvalidInput
	}
}

func checkSnapshotVector(t *testing.T, vector connectorVector) {
	t.Helper()
	if vector.Type != wire.MessageSnapshot {
		return
	}
	var snapshot wire.Snapshot
	if err := json.Unmarshal(vector.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot vector validate: %v", err)
	}
	if vector.ExpectedGeneration != snapshot.Generation || vector.ExpectedCanonical != string(snapshot.Payload) {
		t.Fatalf("snapshot vector generation=%d canonical=%q", snapshot.Generation, snapshot.Payload)
	}
	if !bytes.Equal(snapshot.Payload, []byte(vector.ExpectedCanonical)) {
		t.Fatal("shared canonical snapshot bytes changed")
	}
}
