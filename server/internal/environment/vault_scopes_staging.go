package environment

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func (s *Service) StagePersonalVaultScope(ctx context.Context, account, operation string, request VaultPersonalScopeStage) (VaultScopeDocument, error) {
	var out VaultScopeDocument
	if !validIdentifier(request.WorkspaceID) || !validIdentifier(operation) || !digestExpression.MatchString(request.ExpectedVaultDocumentID) || (request.MachineID != "" && !validIdentifier(request.MachineID)) {
		return out, ErrProtocolInvalid
	}
	raw, err := DecodeCanonicalBase64URL(request.Envelope, MaximumVaultScopeBytes)
	if err != nil {
		return out, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		current, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		if current.Head.DocumentID != request.ExpectedVaultDocumentID {
			return ErrPasswordVaultConflict
		}
		expected := current
		expected.Head.Generation++
		scope, err := parseVaultScope(raw, s.passwordVaultIssuer, expected)
		if err != nil {
			return err
		}
		if scope.Header.WorkspaceID != request.WorkspaceID || scope.Header.OwnerKind != "personal" || scope.Header.OwnerID != account || scope.Header.MachineID != request.MachineID {
			return ErrProtocolInvalid
		}
		old, err := readVaultScopeWorkspaceTx(ctx, tx, request.WorkspaceID, "personal", account, request.MachineID)
		if err != nil {
			return err
		}
		epoch, err := personalVaultEpochTx(ctx, tx, account)
		if err != nil {
			return err
		}
		if !scopeSuccessor(old, true, scope, epoch+1) {
			return ErrVersionConflict
		}
		var op, head string
		err = tx.QueryRow(ctx, `SELECT operation_id,expected_vault_document_id FROM environment_vault_personal_rotations WHERE account_id=$1`, account).Scan(&op, &head)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil && (op != operation || head != request.ExpectedVaultDocumentID) {
			return ErrOperationConflict
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_personal_rotations(account_id,operation_id,expected_vault_document_id) VALUES($1,$2,$3)`, account, operation, request.ExpectedVaultDocumentID); err != nil {
				return err
			}
		}
		if len(request.Records.Envelopes) > MaximumVaultRecordBatch || len(request.Records.DocumentIDs) > MaximumVaultRecordBatch || len(request.Records.Envelopes) != len(request.Records.DocumentIDs) || request.Records.ExpectedSequence > MaxBrowserInteger {
			return ErrProtocolInvalid
		}
		seenIDs := map[string]bool{}
		for _, id := range request.Records.DocumentIDs {
			if !digestExpression.MatchString(id) || seenIDs[id] {
				return ErrProtocolInvalid
			}
			seenIDs[id] = true
		}
		liveBytes := 0
		seenRecords := map[string]bool{}
		for _, encoded := range request.Records.Envelopes {
			raw, err := DecodeCanonicalBase64URL(encoded, MaximumVaultRecordBytes)
			if err != nil {
				return err
			}
			record, err := parseVaultRecord(raw, s.passwordVaultIssuer, expected)
			if err != nil {
				return err
			}
			liveBytes += record.CiphertextBytes
			if liveBytes > MaximumVaultRecordLiveBytes {
				return ErrLimitExceeded
			}
			if seenRecords[record.Claims.RecordID] {
				return ErrProtocolInvalid
			}
			seenRecords[record.Claims.RecordID] = true
			c := record.Claims
			if c.WorkspaceID != request.WorkspaceID || c.OwnerKind != "personal" || c.OwnerID != account || c.MachineID != request.MachineID || c.KeyEpoch != epoch+1 || c.Revision != 1 || c.Deleted {
				return ErrProtocolInvalid
			}
		}
		replacement, err := json.Marshal(request.Records)
		if err != nil {
			return err
		}
		var id string
		var oldReplacement []byte
		err = tx.QueryRow(ctx, `SELECT document_id,record_replacement FROM environment_vault_personal_rotation_scopes WHERE account_id=$1 AND workspace_id=$2 AND machine_id=$3`, account, request.WorkspaceID, request.MachineID).Scan(&id, &oldReplacement)
		out = VaultScopeDocument{WorkspaceID: request.WorkspaceID, MachineID: request.MachineID, DocumentID: scope.State.DocumentID}
		if err == nil {
			var previous VaultRecordReplacement
			if err := json.Unmarshal(oldReplacement, &previous); err != nil {
				return err
			}
			canonical, err := json.Marshal(previous)
			if err != nil {
				return err
			}
			if id != out.DocumentID || !bytes.Equal(canonical, replacement) {
				return ErrOperationConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// Every staged row corresponds to a currently existing bounded account scope.
		_, err = tx.Exec(ctx, `INSERT INTO environment_vault_personal_rotation_scopes(account_id,operation_id,workspace_id,machine_id,document_id,envelope,record_replacement) VALUES($1,$2,$3,$4,$5,$6,$7)`, account, operation, request.WorkspaceID, request.MachineID, out.DocumentID, raw, replacement)
		return err
	})
	return out, err
}
func (s *Service) AbortPersonalVaultRotation(ctx context.Context, account, operation string) error {
	if !validIdentifier(operation) {
		return ErrProtocolInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM environment_vault_personal_rotations WHERE account_id=$1 AND operation_id=$2`, account, operation)
		return err
	})
}
