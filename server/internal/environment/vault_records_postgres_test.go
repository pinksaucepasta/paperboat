package environment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func vaultRecordFixture(t *testing.T, c VaultLayerCoordinate, account, id string, revision, epoch uint64, deleted bool, writerGeneration ...uint64) string {
	t.Helper()
	claims := vaultRecordClaims{Domain: "paperboat.environment.record", Version: 1, Issuer: "https://control.example", WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: epoch, RecordID: id, Revision: revision, Deleted: deleted, WriterAccount: account, WriterVaultGeneration: 1, Nonce: bytes.Repeat([]byte{7}, 12)}
	if len(writerGeneration) == 1 {
		claims.WriterVaultGeneration = writerGeneration[0]
	}
	header, err := EncodeCanonical(claims)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := bytes.Repeat([]byte{19}, 32)
	msg, _ := EncodeCanonical([]any{"paperboat.environment.record-signature", uint64(1), header, ciphertext})
	raw, err := EncodeCanonical(vaultRecordEnvelope{Claims: header, Ciphertext: ciphertext, Signature: ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32)), msg)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}
func TestPostgresVaultRecordsCASAtomicSnapshotAndDeletion(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	account := "recordowner_" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	c := VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: account}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.environment_vault_operations WHERE account_id=$1`, account); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.environment_vault_scopes WHERE owner_id=$1`, account); err != nil {
			t.Error(err)
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.users WHERE id=$1`, account); err != nil {
			t.Error(err)
		}
	}()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, account); err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, nil, "https://control.example")
	vault := passwordVaultFixture(t, "https://control.example", account, 1, make([]byte, 32))
	if _, err := service.PutPasswordVault(ctx, account, vault); err != nil {
		t.Fatal(err)
	}
	anchorRaw := vaultScopeFixture(t, fixtureScopeHeader(account, "personal", account, 1, 1, 1, make([]byte, 32)), 11)
	if _, err := service.PutVaultScope(ctx, account, "personal", "personal", account, "", VaultScopePut{OperationID: "record_anchor_" + account, Envelope: b64Vault(anchorRaw)}); err != nil {
		t.Fatal(err)
	}
	var fence uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.environment_vault_layer_fences WHERE owner_id=$1`, account).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	ids := []string{fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3)}
	put := func(op string, expected []uint64, revisions []uint64, deleted []bool) (VaultRecordsPage, error) {
		in := VaultRecordsPut{OperationID: op + account}
		for i, revision := range revisions {
			in.Records = append(in.Records, VaultRecordMutation{ExpectedRevision: expected[i], Envelope: vaultRecordFixture(t, c, account, ids[i], revision, 1, deleted[i])})
		}
		return service.PutVaultRecords(ctx, account, c, in)
	}
	first, err := put("record_first_", []uint64{0, 0}, []uint64{1, 1}, []bool{false, false})
	if err != nil || first.Sequence != 1 || len(first.Records) != 2 {
		t.Fatalf("firstbatch=%+v err=%v", first, err)
	}
	if replay, err := put("record_first_", []uint64{0, 0}, []uint64{1, 1}, []bool{false, false}); err != nil || replay.Sequence != first.Sequence {
		t.Fatal("lost-ACK exact replay failed", err)
	}
	frozen, err := service.ListVaultRecords(ctx, account, c, VaultRecordsQuery{Limit: 1})
	if err != nil || frozen.NextCursor == "" {
		t.Fatal("snapshot continuation missing", err)
	}
	// Updating one record must not conflict with an independent record's CAS.
	if _, err := put("record_update_", []uint64{1}, []uint64{2}, []bool{false}); err != nil {
		t.Fatal(err)
	}
	secondRecord := VaultRecordsPut{OperationID: "record_other_" + account, Records: []VaultRecordMutation{{ExpectedRevision: 1, Envelope: vaultRecordFixture(t, c, account, ids[1], 2, 1, false)}}}
	if _, err := service.PutVaultRecords(ctx, account, c, secondRecord); err != nil {
		t.Fatal("different-record edit conflicted", err)
	}
	oldPage, err := service.ListVaultRecords(ctx, account, c, VaultRecordsQuery{Limit: 1, Cursor: frozen.NextCursor})
	if err != nil || len(oldPage.Records) != 1 || oldPage.Records[0].Revision != 1 || oldPage.Sequence != 1 {
		t.Fatal("snapshot mixed concurrent edits", err)
	}
	if _, err := put("record_rollback_", []uint64{2, 0}, []uint64{3, 1}, []bool{false, false}); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("stale second CAS did not reject entire batch", err)
	}
	current, err := service.GetVaultRecord(ctx, account, c, ids[0])
	if err != nil || current.Revision != 2 {
		t.Fatal("failed batch partly committed", err)
	}
	if _, err := service.ListVaultRecords(ctx, "foreign", c, VaultRecordsQuery{Limit: 1}); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatal("foreign private read accepted", err)
	}
	if _, err := service.ListVaultRecords(ctx, account, c, VaultRecordsQuery{Limit: 1, Cursor: frozen.NextCursor + "x"}); err == nil {
		t.Fatal("tampered cursor accepted")
	}
	deletion := VaultRecordsPut{OperationID: "record_delete_" + account, Records: []VaultRecordMutation{{ExpectedRevision: 2, Envelope: vaultRecordFixture(t, c, account, ids[0], 3, 1, true)}}}
	deleted, err := service.PutVaultRecords(ctx, account, c, deletion)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := service.ListVaultRecords(ctx, account, c, VaultRecordsQuery{Limit: 200, AfterSequence: first.Sequence})
	if err != nil || len(delta.Records) != 2 || delta.Sequence != deleted.Sequence || !delta.Records[0].Deleted {
		t.Fatal("delta omitted deletion or independent edit", err)
	}
	var afterFence uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM paperboat.environment_vault_layer_fences WHERE owner_id=$1`, account).Scan(&afterFence); err != nil || afterFence != fence {
		t.Fatal("ordinary record edit revoked key grant", err)
	}
	service.SetClock(func() time.Time { return time.Now().UTC().Add(11 * time.Minute) })
	if _, err := service.ListVaultRecords(ctx, account, c, VaultRecordsQuery{Limit: 1, Cursor: frozen.NextCursor}); !errors.Is(err, ErrVaultRecordCursorExpired) {
		t.Fatal("expired snapshot cursor accepted", err)
	}
	// Anchor replacement and record rotation are one transaction: stale sequence
	// or incomplete preservation must leave the old epoch entirely intact.
	live, err := service.GetVaultRecord(ctx, account, c, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	rotate := func(expected, epoch uint64, documents, envelopes []string, loss bool) error {
		return store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			if err := lockVaultScopes(ctx, tx); err != nil {
				return err
			}
			writer, err := service.vaultMetadataTx(ctx, tx, account)
			if err != nil {
				return err
			}
			old, err := readVaultScopeWorkspaceTx(ctx, tx, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID)
			if err != nil {
				return err
			}
			id, err := hex.DecodeString(strings.TrimPrefix(old.DocumentID, "sha256:"))
			if err != nil {
				return err
			}
			next, err := parseVaultScope(vaultScopeFixture(t, fixtureScopeHeader(account, "personal", account, 1, epoch, old.Revision+1, id[:]), 11), service.passwordVaultIssuer, writer)
			if err != nil {
				return err
			}
			if err := writeVaultScopeTx(ctx, tx, next, writer.WriterPublicKey[:]); err != nil {
				return err
			}
			_, err = service.RebuildVaultRecordsTx(ctx, tx, c, expected, documents, epoch, writer, envelopes, loss)
			return err
		})
	}
	newID := fmt.Sprintf("%064x", 4)
	newEnvelope := vaultRecordFixture(t, c, account, newID, 1, 2, false)
	if err := rotate(deleted.Sequence-1, 2, []string{live.DocumentID}, []string{newEnvelope}, false); !errors.Is(err, ErrVersionConflict) {
		t.Fatal("stale rotation accepted", err)
	}
	if err := rotate(deleted.Sequence, 2, []string{live.DocumentID}, nil, false); !errors.Is(err, ErrProtocolInvalid) {
		t.Fatal("preserving rotation lost a record", err)
	}
	if _, err := service.GetVaultRecord(ctx, account, c, ids[1]); err != nil {
		t.Fatal("failed rotation modified old epoch", err)
	}
	if err := rotate(deleted.Sequence, 2, []string{live.DocumentID}, []string{newEnvelope}, false); err != nil {
		t.Fatal(err)
	}
	var rotatedEpoch uint64
	var rotatedCount int
	if err := store.SQL().QueryRowContext(ctx, `SELECT key_epoch,(SELECT count(*) FROM paperboat.environment_vault_record_versions WHERE owner_id=$1) FROM paperboat.environment_vault_record_heads WHERE owner_id=$1`, account).Scan(&rotatedEpoch, &rotatedCount); err != nil || rotatedEpoch != 2 || rotatedCount != 1 {
		t.Fatal("rotation retained old records or tombstones", err)
	}
	if err := rotate(1, 3, nil, nil, true); err != nil {
		t.Fatal("total loss reset failed", err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.environment_vault_record_versions WHERE owner_id=$1`, account).Scan(&rotatedCount); err != nil || rotatedCount != 0 {
		t.Fatal("total loss reset retained records", err)
	}

}
