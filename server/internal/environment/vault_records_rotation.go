package environment

import (
	"context"
	"crypto/rand"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// RebuildVaultRecordsTx is called inside the already-authorized scope/vault
// rotation transaction, after writing its new anchor. The caller must validate
// total-loss authority using the existing owner/reset gate. The previous exact
// live document inventory fences preserving rotations against concurrent edits.
// No operation may use this helper as an ordinary record-write shortcut.
func (s *Service) RebuildVaultRecordsTx(ctx context.Context, tx *db.Tx, c VaultLayerCoordinate, expectedSequence uint64, oldDocumentIDs []string, epoch uint64, writer PasswordVaultMetadata, envelopes []string, totalLoss bool) (VaultRecordsPage, error) {
	out := VaultRecordsPage{Records: []VaultRecordState{}}
	if !validLayerCoordinate(c) || !validCounter(epoch) || len(envelopes) > MaximumVaultRecordBatch || len(oldDocumentIDs) > 128 {
		return out, ErrProtocolInvalid
	}
	anchor, err := readVaultScopeWorkspaceTx(ctx, tx, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
	if err != nil {
		return out, err
	}
	if anchor.KeyEpoch != epoch {
		return out, ErrVersionConflict
	}
	var oldEpoch, oldSequence uint64
	err = tx.QueryRow(ctx, `SELECT key_epoch,sequence FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4 FOR UPDATE`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID).Scan(&oldEpoch, &oldSequence)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if oldSequence != expectedSequence || exists && (epoch < oldEpoch || epoch == oldEpoch && oldSequence > 0) {
		return out, ErrVersionConflict
	}
	if !totalLoss && len(envelopes) != len(oldDocumentIDs) {
		return out, ErrProtocolInvalid
	}
	if totalLoss && len(envelopes) != 0 {
		return out, ErrProtocolInvalid
	}
	if !totalLoss && exists {
		expected := map[string]bool{}
		for _, id := range oldDocumentIDs {
			if expected[id] {
				return out, ErrProtocolInvalid
			}
			expected[id] = true
		}
		rows, err := tx.Query(ctx, `SELECT document_id FROM(SELECT DISTINCT ON(record_id) document_id,deleted FROM environment_vault_record_versions WHERE `+recordScopePredicate+` ORDER BY record_id,revision DESC) current WHERE NOT deleted`, recordArgs(c, oldEpoch)...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return out, err
			}
			if !expected[id] {
				rows.Close()
				return out, ErrVersionConflict
			}
			delete(expected, id)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return out, err
		}
		if len(expected) != 0 {
			return out, ErrVersionConflict
		}
	} else if !totalLoss && len(oldDocumentIDs) > 0 {
		return out, ErrVersionConflict
	}
	parsed := make([]parsedVaultRecord, 0, len(envelopes))
	seen := map[string]bool{}
	liveBytes := 0
	for _, encoded := range envelopes {
		raw, err := DecodeCanonicalBase64URL(encoded, MaximumVaultRecordBytes)
		if err != nil {
			return out, err
		}
		record, err := parseVaultRecord(raw, s.passwordVaultIssuer, writer)
		if err != nil {
			return out, err
		}
		claims := record.Claims
		if (VaultLayerCoordinate{WorkspaceID: claims.WorkspaceID, OwnerKind: claims.OwnerKind, OwnerID: claims.OwnerID, MachineID: claims.MachineID}) != c || claims.KeyEpoch != epoch || claims.Revision != 1 || claims.Deleted || seen[claims.RecordID] {
			return out, ErrProtocolInvalid
		}
		seen[claims.RecordID] = true
		liveBytes += record.CiphertextBytes
		if liveBytes > MaximumVaultRecordLiveBytes {
			return out, ErrLimitExceeded
		}
		parsed = append(parsed, record)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return out, err
	}
	// Delete/recreate the head cascades every old epoch version and cursor key,
	// together with replacing the anchor in the caller's atomic transaction.
	if _, err := tx.Exec(ctx, `DELETE FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID); err != nil {
		return out, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_record_heads(workspace_id,owner_kind,owner_id,machine_id,key_epoch,sequence,cursor_key) VALUES($1,$2,$3,$4,$5,1,$6)`, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, epoch, key); err != nil {
		return out, err
	}
	for _, record := range parsed {
		state := recordState(c, record, 1, writer.WriterPublicKey[:])
		args := append(recordArgs(c, epoch), state.RecordID, state.Revision, state.Deleted, uint64(1), state.DocumentID, record.Raw, record.CiphertextBytes, writer.WriterPublicKey[:])
		if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_record_versions(workspace_id,owner_kind,owner_id,machine_id,key_epoch,record_id,revision,deleted,sequence,document_id,envelope,ciphertext_bytes,writer_public) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, args...); err != nil {
			return out, err
		}
		out.Records = append(out.Records, state)
	}
	out.Sequence = 1
	return out, nil
}
