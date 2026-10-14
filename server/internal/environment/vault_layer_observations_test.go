package environment

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func layerObservationFixture() (VaultLayerObservation, VaultLayerDelivery) {
	public := bytes.Repeat([]byte{19}, 32)
	coordinate := VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "owner"}
	document := "sha256:" + strings.Repeat("a", 64)
	o := VaultLayerObservation{WorkspaceID: coordinate.WorkspaceID, OwnerKind: coordinate.OwnerKind, OwnerID: coordinate.OwnerID, DeliveryGeneration: 2, DocumentID: document, FenceGeneration: 3, HostRecipientKeyID: vaultHostKeyID(public), ObservationSeq: 4, State: "applied", ObservedAt: time.Now().UTC()}
	d := VaultLayerDelivery{Recipient: VaultLayerRecipient{HostPublic: b64VaultPublic(public), DeliveryGeneration: 2, DocumentID: document, FenceGeneration: 3}, Source: VaultLayerSource{VaultLayerCoordinate: coordinate}, State: "ready"}
	return o, d
}
func TestLayerObservationRequiresExactCurrentBinding(t *testing.T) {
	o, d := layerObservationFixture()
	if !o.Valid() || !layerObservationMatches(o, d) {
		t.Fatal("current applied report rejected")
	}
	for _, which := range []string{"workspace", "owner", "source_machine", "delivery", "document", "fence", "recipient", "pending"} {
		t.Run(which, func(t *testing.T) {
			changed := d
			switch which {
			case "workspace":
				changed.Source.WorkspaceID = "team"
			case "owner":
				changed.Source.OwnerID = "foreign"
			case "source_machine":
				changed.Source.MachineID = "foreign"
			case "delivery":
				changed.Recipient.DeliveryGeneration++
			case "document":
				changed.Recipient.DocumentID = "sha256:" + strings.Repeat("b", 64)
			case "fence":
				changed.Recipient.FenceGeneration++
			case "recipient":
				changed.Recipient.HostPublic = b64VaultPublic(bytes.Repeat([]byte{20}, 32))
			case "pending":
				changed.State = "pending"
			}
			if layerObservationMatches(o, changed) {
				t.Fatal("stale or foreign report marked current")
			}
		})
	}
}
func TestLayerObservationRejectsUnboundedOrUntrustedFailureState(t *testing.T) {
	o, _ := layerObservationFixture()
	code := "environment_projection_invalid"
	o.State = "failed"
	o.ErrorCode = &code
	if !o.Valid() {
		t.Fatal("typed failure rejected")
	}
	for _, which := range []string{"state", "error", "applied_error", "zero_sequence", "overflow", "document", "timestamp", "coordinate"} {
		t.Run(which, func(t *testing.T) {
			changed := o
			switch which {
			case "state":
				changed.State = "revoked"
			case "error":
				unknown := "provider secret error"
				changed.ErrorCode = &unknown
			case "applied_error":
				changed.State = "applied"
			case "zero_sequence":
				changed.ObservationSeq = 0
			case "overflow":
				changed.DeliveryGeneration = MaxBrowserInteger + 1
			case "document":
				changed.DocumentID = "arbitrary"
			case "timestamp":
				changed.ObservedAt = time.Time{}
			case "coordinate":
				changed.OwnerKind = "team"
				changed.WorkspaceID = "personal"
			}
			if changed.Valid() {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestLayerObservationBatchBoundsBeforeStorage(t *testing.T) {
	observation, _ := layerObservationFixture()
	service := &Service{}
	for _, which := range []string{"empty", "duplicates", "too_many", "bad_state", "bad_operation"} {
		t.Run(which, func(t *testing.T) {
			in := VaultLayerObservationsPut{OperationID: "operation", Observations: []VaultLayerObservation{observation}}
			switch which {
			case "empty":
				in.Observations = nil
			case "duplicates":
				in.Observations = append(in.Observations, observation)
			case "too_many":
				in.Observations = make([]VaultLayerObservation, MaximumVaultLayerObservations+1)
			case "bad_state":
				in.Observations[0].State = "unknown"
			case "bad_operation":
				in.OperationID = ""
			}
			if _, err := service.RecordVaultLayerObservations(context.Background(), "owner", "host", in); !errors.Is(err, ErrObservationInvalid) {
				t.Fatalf("invalid batch reached storage: %v", err)
			}
		})
	}
}
