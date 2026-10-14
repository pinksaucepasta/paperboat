package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type storeErrorSpy struct{}

func (storeErrorSpy) Error() string { panic("private cause must not be formatted") }

func TestStoreCausePrivacyRollbackAndFreshRecovery(t *testing.T) {
	fail := false
	state, _ := openStore(t, func(point string) error {
		if fail && point == "append_before_commit" {
			return storeErrorSpy{}
		}
		return nil
	})
	defer state.Close()
	if err := state.CreateSession(t.Context(), testSession()); err != nil {
		t.Fatal(err)
	}
	fail = true
	_, _, err := state.AppendOutput(t.Context(), "ses_1", 1, 0, []byte("PRIVATE_TERMINAL_PAYLOAD"), 64)
	if !errors.Is(err, storeErrorSpy{}) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("private rollback cause lost or formatted")
	}
	_, latest, err := state.OutputBounds(t.Context(), "ses_1")
	if err != nil || latest != 0 {
		t.Fatal("failed append mutated cursor")
	}
	fail = false
	if _, _, err := state.AppendOutput(t.Context(), "ses_1", 1, 0, []byte("ok"), 64); err != nil {
		t.Fatal("fresh append did not recover")
	}
	// Persisted parser failures retain typed decoder metadata without row values.
	if _, err := state.db.Exec(`UPDATE sessions SET command_args=? WHERE id='ses_1'`, []byte(`["PRIVATE_ARGUMENT",}`)); err != nil {
		t.Fatal(err)
	}
	_, err = state.Sessions(t.Context())
	var syntax *json.SyntaxError
	if !errors.Is(err, ErrCorrupt) || !errors.As(err, &syntax) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("decoder cause/privacy lost")
	}
	if _, err := state.db.Exec(`UPDATE sessions SET command_args=? WHERE id='ses_1'`, []byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := state.Sessions(t.Context()); err != nil {
		t.Fatal("fresh session read failed")
	}
	// A real SQLite native constraint remains discoverable but never displays
	// terminal/env/path content through the public store error.
	err = state.CreateSession(t.Context(), testSession())
	var native *sqlite.Error
	if !errors.As(err, &native) || native.Code()&255 != 19 || strings.Contains(err.Error(), "SQLITE") {
		t.Fatal("original constraint cause lost")
	}
	if errors.Is(classifyDBError(errors.Join(err, syscall.EIO)), ErrCorrupt) {
		t.Fatal("mixed native error authorized corruption")
	}
}

func TestStoreSQLiteBusyCancellationRetainsCauseAndRecovers(t *testing.T) {
	state, _ := openStore(t, nil)
	defer state.Close()
	now := time.Now().UTC()
	operation := OperationResult{OperationID: "op_busy_0001", RequestHash: []byte("hash"), Result: []byte(`{"ok":true}`), CompletedAt: now, ExpiresAt: now.Add(time.Hour)}
	if _, _, err := state.ReserveOperation(t.Context(), operation.OperationID, operation.RequestHash, operation.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	tx, err := state.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE operation_results SET error_code='lock' WHERE operation_id=?`, operation.OperationID); err != nil {
		t.Fatal(err)
	}
	conn, err := state.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), "PRAGMA busy_timeout=1"); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	// Fixture reduces only the free connection's native timeout to exercise the
	// unchanged application's20x50ms retry policy without waiting5s per attempt.
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err = state.CompleteOperation(ctx, operation)
	cancel()
	var native *sqlite.Error
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &native) || native.Code()&255 != 5 {
		t.Fatal("busy cancellation lost original native error")
	}
	if retryableSQLiteFailure(err) || retryableSQLiteFailure(errors.Join(native, syscall.EIO)) {
		t.Fatal("mixed cause authorized retry")
	}
	if !retryableSQLiteFailure(fmt.Errorf("wrapped: %w", native)) {
		t.Fatal("unary native busy proof lost")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := state.CompleteOperation(t.Context(), operation); err != nil {
		t.Fatal("completion did not recover after lock release")
	}
	if err := state.CompleteOperation(t.Context(), operation); err != nil {
		t.Fatal("completed operation idempotency changed")
	}
	var result []byte
	if err := state.db.QueryRow(`SELECT result FROM operation_results WHERE operation_id=?`, operation.OperationID).Scan(&result); err != nil || string(result) != string(operation.Result) {
		t.Fatal("operation result changed")
	}
}

func TestStoreSchemaOperationalFailureIsNotCorruption(t *testing.T) {
	state, _ := openStore(t, nil)
	defer state.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := validateDatabaseSchema(ctx, state.db)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrCorrupt) {
		t.Fatal("canceled schema probe mislabeled corrupt")
	}
	if err := validateDatabaseSchema(t.Context(), state.db); err != nil {
		t.Fatal("schema probe did not recover")
	}
	if _, err := state.db.Exec(`CREATE TABLE PRIVATE_SCHEMA_NAME(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	err = validateDatabaseSchema(t.Context(), state.db)
	if !errors.Is(err, ErrCorrupt) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("schema drift leaked private metadata")
	}
	// Standard transaction completion is the sole quiet rollback outcome.
	tx, err := state.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var resultErr error
	retainRollback(tx, &resultErr)
	if resultErr != nil {
		t.Fatal("committed transaction produced false cleanup fault")
	}
	if _, err := state.db.Exec(`DROP TABLE PRIVATE_SCHEMA_NAME`); err != nil {
		t.Fatal(err)
	}
	if err := validateDatabaseSchema(t.Context(), state.db); err != nil {
		t.Fatal(err)
	}
}

func TestStoreCompletionFallbackRetainsRealReadFailureAndRecovery(t *testing.T) {
	state, _ := openStore(t, nil)
	defer state.Close()
	now := time.Now().UTC()
	transfer := FileTransfer{ID: "ft_fallback", BatchID: "fb_fallback", SourceMachineID: "source", DestinationMachineID: "destination", InitiatingUserID: "user", Basename: "data", Size: 4, SHA256: strings.Repeat("a", 64), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := state.CreateFileTransfers(t.Context(), []FileTransfer{transfer}); err != nil {
		t.Fatal(err)
	}
	// Simulate a live schema/read fault with real SQLite objects. The no-op view
	// update yields zero affected rows, then its malformed timestamp fails the
	// authoritative idempotency lookup. It must never turn into a refusal.
	if _, err := state.db.Exec(`ALTER TABLE file_transfers RENAME TO saved_transfers`); err != nil {
		t.Fatal(err)
	}
	view := strings.Replace(fileTransferSelect, "created_at,expires_at", "'PRIVATE_TIMESTAMP' AS created_at,expires_at", 1)
	view = strings.Replace(view, "FROM file_transfers", "FROM saved_transfers", 1)
	if _, err := state.db.Exec(`CREATE VIEW file_transfers AS ` + view); err != nil {
		t.Fatal(err)
	}
	if _, err := state.db.Exec(`CREATE TRIGGER ignored_update INSTEAD OF UPDATE ON file_transfers BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []func() error{func() error { return state.CompleteFileTransfer(t.Context(), transfer.ID, "destination") }, func() error { return state.CancelFileTransfer(t.Context(), transfer.ID) }} {
		err := operation()
		if err == nil || errors.Is(err, ErrConflict) || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("read failure became conflict or exposed private timestamp")
		}
	}
	if _, err := state.db.Exec(`DROP VIEW file_transfers; ALTER TABLE saved_transfers RENAME TO file_transfers`); err != nil {
		t.Fatal(err)
	}
	if err := state.CommitFileTransferOffset(t.Context(), transfer.ID, 0, 4); err != nil {
		t.Fatal(err)
	}
	if err := state.CompleteFileTransfer(t.Context(), transfer.ID, "destination"); err != nil {
		t.Fatal("fresh completion did not recover")
	}
	if err := state.CompleteFileTransfer(t.Context(), transfer.ID, "destination"); err != nil {
		t.Fatal("idempotent completion changed")
	}
}
