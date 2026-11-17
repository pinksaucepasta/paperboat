package environment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/vaultprotocol"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type VaultLayerObservation = vaultprotocol.VaultLayerObservation

const MaximumVaultLayerObservations = 128

type VaultLayerObservationsPut struct {
	OperationID  string                  `json:"operation_id"`
	Observations []VaultLayerObservation `json:"observations"`
}
type VaultLayerObservationsAck struct {
	OperationID string `json:"operation_id"`
	Accepted    bool   `json:"accepted"`
}

func validLayerObservation(o VaultLayerObservation) bool {
	return validLayerCoordinate(o.Coordinate()) && validCounter(o.DeliveryGeneration) && digestExpression.MatchString(o.DocumentID) && validCounter(o.FenceGeneration) && stringsVaultKeyID(o.HostRecipientKeyID) && validCounter(o.ObservationSeq) && !o.ObservedAt.IsZero() && ((o.State == "applied" && o.ErrorCode == nil) || (o.State == "failed" && o.ErrorCode != nil && *o.ErrorCode == "environment_projection_invalid"))
}
func layerObservationMatches(o VaultLayerObservation, d VaultLayerDelivery) bool {
	public, err := DecodeCanonicalBase64URL(d.Recipient.HostPublic, 32)
	return err == nil && len(public) == 32 && d.State == "ready" && o.Coordinate() == d.Source.VaultLayerCoordinate && o.DeliveryGeneration == d.Recipient.DeliveryGeneration && o.DocumentID == d.Recipient.DocumentID && o.FenceGeneration == d.Recipient.FenceGeneration && o.HostRecipientKeyID == vaultHostKeyID(public)
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
		if !validLayerObservation(o) || seen[o.Coordinate()] {
			return out, ErrObservationInvalid
		}
		seen[o.Coordinate()] = true
	}
	err := s.db.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		// Observations update only this host's layer rows: they wait for the writers of each team they name and
		// for the host's own account, never for other hosts' observations.
		var teams []string
		for c := range seen {
			if c.WorkspaceID != "personal" && !slices.Contains(teams, c.WorkspaceID) {
				teams = append(teams, c.WorkspaceID)
			}
		}
		if err := db.LockTeamsFor(ctx, tx, false, teams...); err != nil {
			return err
		}
		if err := lockVaultAccounts(ctx, tx, true, account); err != nil {
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
			c := o.Coordinate()
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
	if json.Unmarshal(raw, &o) != nil || !validLayerObservation(o) {
		return ErrObservationInvalid
	}
	d.Observation = &o
	d.Applied = o.State == "applied" && layerObservationMatches(o, *d)
	return nil
}
