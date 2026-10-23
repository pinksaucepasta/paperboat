package environment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

const MaximumVaultLayerObservations = 128

type VaultLayerObservation struct {
	WorkspaceID        string    `json:"workspace_id"`
	OwnerKind          string    `json:"owner_kind"`
	OwnerID            string    `json:"owner_id"`
	SourceMachineID    string    `json:"source_machine_id"`
	DeliveryGeneration uint64    `json:"delivery_generation"`
	DocumentID         string    `json:"document_id"`
	FenceGeneration    uint64    `json:"fence_generation"`
	HostRecipientKeyID string    `json:"host_recipient_key_id"`
	ObservationSeq     uint64    `json:"observation_seq"`
	State              string    `json:"state"`
	ErrorCode          *string   `json:"error_code"`
	ObservedAt         time.Time `json:"observed_at"`
}
type VaultLayerObservationsPut struct {
	OperationID  string                  `json:"operation_id"`
	Observations []VaultLayerObservation `json:"observations"`
}
type VaultLayerObservationsAck struct {
	OperationID string `json:"operation_id"`
	Accepted    bool   `json:"accepted"`
}

func (o VaultLayerObservation) coordinate() VaultLayerCoordinate {
	return VaultLayerCoordinate{o.WorkspaceID, o.OwnerKind, o.OwnerID, o.SourceMachineID}
}
func (o VaultLayerObservation) Valid() bool {
	return validLayerCoordinate(o.coordinate()) && validCounter(o.DeliveryGeneration) && digestExpression.MatchString(o.DocumentID) && validCounter(o.FenceGeneration) && stringsVaultKeyID(o.HostRecipientKeyID) && validCounter(o.ObservationSeq) && !o.ObservedAt.IsZero() && ((o.State == "applied" && o.ErrorCode == nil) || (o.State == "failed" && o.ErrorCode != nil && *o.ErrorCode == "environment_projection_invalid"))
}
func layerObservationMatches(o VaultLayerObservation, d VaultLayerDelivery) bool {
	public, err := DecodeCanonicalBase64URL(d.Recipient.HostPublic, 32)
	return err == nil && len(public) == 32 && d.State == "ready" && o.coordinate() == d.Source.VaultLayerCoordinate && o.DeliveryGeneration == d.Recipient.DeliveryGeneration && o.DocumentID == d.Recipient.DocumentID && o.FenceGeneration == d.Recipient.FenceGeneration && o.HostRecipientKeyID == vaultHostKeyID(public)
}

// RecordVaultLayerObservations records metadata only. Authenticate the current recipient before replaying
// a receipt; an old successful operation cannot restore a revoked host's access.
func (s *Service) RecordVaultLayerObservations(ctx context.Context, account, machine string, in VaultLayerObservationsPut) (VaultLayerObservationsAck, error) {
	out := VaultLayerObservationsAck{OperationID: in.OperationID, Accepted: true}
	if !validIdentifier(in.OperationID) || len(in.Observations) == 0 || len(in.Observations) > MaximumVaultLayerObservations {
		return out, ErrObservationInvalid
	}
	seen := make(map[VaultLayerCoordinate]bool, len(in.Observations))
	for _, o := range in.Observations {
		if !o.Valid() || seen[o.coordinate()] {
			return out, ErrObservationInvalid
		}
		seen[o.coordinate()] = true
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if _, err := authorizeVaultHostTx(ctx, tx, account, machine); err != nil {
			return err
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, in.OperationID, "host-layer-observations:"+machine, in, &out)
		if err != nil || replay {
			return err
		}
		for _, o := range in.Observations {
			c := o.coordinate()
			delivery, err := readVaultLayerTx(ctx, tx, machine, c)
			if err != nil {
				return err
			}
			// Current recipient membership and entitlement are rechecked for shared
			// layers, including when the ciphertext survives a membership revocation.
			if c.OwnerKind == "team" || c.WorkspaceID != "personal" {
				if err := requireVaultWorkspaceMemberTx(ctx, tx, account, c.WorkspaceID); err != nil {
					return err
				}
			}
			if c.OwnerKind == "personal" && c.OwnerID != account {
				return ErrKeyAuthorizationRequired
			}
			var sequence uint64
			var previous []byte
			err = tx.QueryRow(ctx, `SELECT observation_seq,observation_digest FROM environment_vault_layers WHERE machine_id=$1 AND workspace_id=$2 AND owner_kind=$3 AND owner_id=$4 AND source_machine_id=$5`, machine, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID).Scan(&sequence, &previous)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrVersionConflict
			}
			if err != nil {
				return err
			}
			raw, err := json.Marshal(o)
			if err != nil {
				return err
			}
			if len(raw) > 4096 {
				return ErrObservationInvalid
			}
			sum := sha256.Sum256(raw)
			if o.ObservationSeq < sequence {
				continue
			}
			if o.ObservationSeq == sequence {
				if !bytes.Equal(previous, sum[:]) {
					return ErrOperationConflict
				}
				continue
			}
			if !layerObservationMatches(o, delivery) {
				return ErrVersionConflict
			}
			if _, err := tx.Exec(ctx, `UPDATE environment_vault_layers SET observation_seq=$6,observation_digest=$7,observation=$8 WHERE machine_id=$1 AND workspace_id=$2 AND owner_kind=$3 AND owner_id=$4 AND source_machine_id=$5`, machine, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, o.ObservationSeq, sum[:], raw); err != nil {
				return err
			}
		}
		return saveVaultOperationTx(ctx, tx, account, in.OperationID, digest, out)
	})
	return out, err
}

func layerObservationTx(ctx context.Context, tx *db.Tx, d *VaultLayerDelivery) error {
	var raw []byte
	c := d.Source.VaultLayerCoordinate
	err := tx.QueryRow(ctx, `SELECT observation FROM environment_vault_layers WHERE machine_id=$1 AND workspace_id=$2 AND owner_kind=$3 AND owner_id=$4 AND source_machine_id=$5`, d.Recipient.MachineID, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > 4096 {
		return ErrObservationInvalid
	}
	var o VaultLayerObservation
	if json.Unmarshal(raw, &o) != nil || !o.Valid() {
		return ErrObservationInvalid
	}
	d.Observation = &o
	d.Applied = o.State == "applied" && layerObservationMatches(o, *d)
	return nil
}
