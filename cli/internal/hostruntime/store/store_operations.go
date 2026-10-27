package store

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"time"
)

func (s *Store) PutInputDecision(ctx context.Context, decision InputDecision) (insertedResult bool, resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO input_decisions(session_id,client_id,attachment_id,generation,input_id,request_hash,status,bytes_written,error_code,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, decision.SessionID, decision.ClientID, decision.AttachmentID, decision.Generation, decision.InputID, decision.Hash, decision.Status, decision.BytesWritten, nullableString(decision.ErrorCode), decision.CreatedAt.UnixNano())
	if err != nil {
		return false, err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return false, affectedErr
	}
	if changed == 1 {
		return true, nil
	}
	var hash []byte
	if err := s.db.QueryRowContext(ctx, `SELECT request_hash FROM input_decisions WHERE session_id=? AND client_id=? AND attachment_id=? AND generation=? AND input_id=?`, decision.SessionID, decision.ClientID, decision.AttachmentID, decision.Generation, decision.InputID).Scan(&hash); err != nil {
		return false, err
	}
	if !bytes.Equal(hash, decision.Hash) {
		return false, ErrConflict
	}
	return false, nil
}

func (s *Store) InputDecisions(ctx context.Context, sessionID string) (decisionResults []InputDecision, resultErr error) {
	defer protectStoreError(&resultErr)
	rows, err := s.q.ListInputDecisions(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	decisions := make([]InputDecision, 0, len(rows))
	for _, row := range rows {
		decisions = append(decisions, InputDecision{
			SessionID: row.SessionID, ClientID: row.ClientID, AttachmentID: row.AttachmentID,
			Generation: uint64(row.Generation), InputID: row.InputID, Hash: row.RequestHash,
			Status: row.Status, BytesWritten: int(row.BytesWritten), ErrorCode: row.ErrorCode,
			CreatedAt: time.Unix(0, row.CreatedAt).UTC(),
		})
	}
	return decisions, nil
}

// UpdateInputDecisions records several input outcomes in one transaction (one fsync for a burst of typing). A row
// already pruned from the journal window is skipped.
func (s *Store) UpdateInputDecisions(ctx context.Context, decisions []InputDecision) (resultErr error) {
	defer protectStoreError(&resultErr)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	for _, decision := range decisions {
		if _, err := tx.ExecContext(ctx, `UPDATE input_decisions SET status=?,bytes_written=?,error_code=? WHERE session_id=? AND client_id=? AND attachment_id=? AND generation=? AND input_id=? AND request_hash=?`, decision.Status, decision.BytesWritten, nullableString(decision.ErrorCode), decision.SessionID, decision.ClientID, decision.AttachmentID, decision.Generation, decision.InputID, decision.Hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) PutOperation(ctx context.Context, operation OperationResult) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, _, err := s.ReserveOperation(ctx, operation.OperationID, operation.RequestHash, operation.ExpiresAt)
	if err != nil {
		return err
	}
	return s.CompleteOperation(ctx, operation)
}

func (s *Store) ReserveOperation(ctx context.Context, operationID string, requestHash []byte, expiresAt time.Time) (operationResult OperationResult, insertedResult bool, resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `INSERT INTO operation_results(operation_id,request_hash,state,result,error_code,completed_at,expires_at) VALUES(?,?,'pending',NULL,NULL,NULL,?) ON CONFLICT(operation_id) DO NOTHING`, operationID, requestHash, expiresAt.UnixNano())
	if err != nil {
		return OperationResult{}, false, err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return OperationResult{}, false, affectedErr
	}
	if changed == 1 {
		return OperationResult{OperationID: operationID, RequestHash: append([]byte(nil), requestHash...), State: "pending", ExpiresAt: expiresAt}, true, nil
	}
	existing, err := s.operation(ctx, operationID)
	if err != nil {
		return OperationResult{}, false, err
	}
	if !bytes.Equal(existing.RequestHash, requestHash) {
		return OperationResult{}, false, ErrConflict
	}
	return existing, false, nil
}

func (s *Store) CompleteOperation(ctx context.Context, operation OperationResult) (resultErr error) {
	defer protectStoreError(&resultErr)
	if len(operation.Result) > MaxOperationResultBytes {
		return ErrResultTooLarge
	}
	const attempts = 20
	for attempt := 0; attempt < attempts; attempt++ {
		result, err := s.db.ExecContext(ctx, `UPDATE operation_results SET state='completed',result=?,error_code=?,completed_at=?,expires_at=? WHERE operation_id=? AND request_hash=? AND state='pending'`, operation.Result, nullableString(operation.ErrorCode), operation.CompletedAt.UnixNano(), operation.ExpiresAt.UnixNano(), operation.OperationID, operation.RequestHash)
		if err != nil {
			if !retryableSQLiteFailure(err) {
				return err
			}
			if attempt+1 < attempts {
				select {
				case <-ctx.Done():
					return storeFailure("runtime state completion interrupted", err, ctx.Err())
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}
			return err
		}
		changed, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		if changed == 1 {
			return nil
		}
		existing, err := s.operation(ctx, operation.OperationID)
		if err != nil {
			return err
		}
		if !bytes.Equal(existing.RequestHash, operation.RequestHash) {
			return ErrConflict
		}
		if existing.State == "completed" {
			if bytes.Equal(existing.Result, operation.Result) && existing.ErrorCode == operation.ErrorCode {
				return nil
			}
			return ErrConflict
		}
		if attempt+1 < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return ErrConflict
}

func (s *Store) DeleteExpiredOperations(ctx context.Context, now time.Time) (resultErr error) {
	defer protectStoreError(&resultErr)
	return s.q.DeleteExpiredOperations(ctx, now.UnixNano())
}

func (s *Store) Operations(ctx context.Context, now time.Time, limit int) (operationResults []OperationResult, resultErr error) {
	defer protectStoreError(&resultErr)
	if limit < 1 {
		return nil, ErrConflict
	}
	if err := s.q.DeleteExpiredOperations(ctx, now.UnixNano()); err != nil {
		return nil, err
	}
	// Older helpers persisted terminal replay inside attach outcomes. Discard
	// those oversized payloads without releasing the operation ID for reuse:
	// pending makes a retry uncertain instead of rerunning a mutation.
	if err := s.q.ResetOversizedOperations(ctx, MaxOperationResultBytes); err != nil {
		return nil, err
	}
	rows, err := s.q.ListOperations(ctx, int64(limit))
	if err != nil {
		return nil, err
	}
	operations := make([]OperationResult, 0, len(rows))
	for _, row := range rows {
		operations = append(operations, operationFromSQLC(row.OperationID, row.RequestHash, row.State, row.Result, row.ErrorCode, row.CompletedAt, row.ExpiresAt))
	}
	return operations, nil
}

func (s *Store) OperationsWithPrefix(ctx context.Context, prefix string, now time.Time, limit int) (operationResults []OperationResult, resultErr error) {
	defer protectStoreError(&resultErr)
	return s.operationsByPrefix(ctx, prefix, false, now, limit)
}

func (s *Store) OperationsExcludingPrefix(ctx context.Context, prefix string, now time.Time, limit int) (operationResults []OperationResult, resultErr error) {
	defer protectStoreError(&resultErr)
	return s.operationsByPrefix(ctx, prefix, true, now, limit)
}

func (s *Store) operationsByPrefix(ctx context.Context, prefix string, exclude bool, now time.Time, limit int) (operationResults []OperationResult, resultErr error) {
	defer protectStoreError(&resultErr)
	if prefix == "" || limit < 1 || limit > 1<<20 {
		return nil, ErrConflict
	}
	if err := s.DeleteExpiredOperations(ctx, now); err != nil {
		return nil, err
	}
	comparison := "LIKE"
	if exclude {
		comparison = "NOT LIKE"
	}
	query := `SELECT operation_id,request_hash,state,result,COALESCE(error_code,''),completed_at,expires_at FROM operation_results WHERE operation_id ` + comparison + ` ? ESCAPE '\' ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END,completed_at ASC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, escapeLike(prefix)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			resultErr = storeFailure("runtime state query cleanup failed", resultErr, closeErr)
		}
	}()
	operations := make([]OperationResult, 0)
	for rows.Next() {
		var operationID, state, errorCode string
		var requestHash, result []byte
		var completed sql.NullInt64
		var expires int64
		if err := rows.Scan(&operationID, &requestHash, &state, &result, &errorCode, &completed, &expires); err != nil {
			return nil, err
		}
		operations = append(operations, operationFromSQLC(operationID, requestHash, state, result, errorCode, completed, expires))
	}
	return operations, rows.Err()
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `%`, `\%`)
	return strings.ReplaceAll(value, `_`, `\_`)
}

func (s *Store) operation(ctx context.Context, operationID string) (operationResult OperationResult, resultErr error) {
	defer protectStoreError(&resultErr)
	row, err := s.q.GetOperation(ctx, operationID)
	if err != nil {
		return OperationResult{}, err
	}
	return operationFromSQLC(row.OperationID, row.RequestHash, row.State, row.Result, row.ErrorCode, row.CompletedAt, row.ExpiresAt), nil
}

// PruneInputDecisions deletes one client's sequenced input decisions of a generation up to and including
// sequence upTo. Unsequenced (named) decisions are kept.
func (s *Store) PruneInputDecisions(ctx context.Context, sessionID, clientID, attachmentID string, generation, upTo uint64) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, err := s.db.ExecContext(ctx, `DELETE FROM input_decisions WHERE session_id=? AND client_id=? AND attachment_id=? AND generation=? AND input_id NOT GLOB '*[^0-9]*' AND input_id<>'' AND CAST(input_id AS INTEGER)<=?`, sessionID, clientID, attachmentID, generation, upTo)
	return err
}

// DeleteInputDecisionsOutsideGeneration deletes a session's input decisions of every other generation:
// input of an older process generation is refused as stale.
func (s *Store) DeleteInputDecisionsOutsideGeneration(ctx context.Context, sessionID string, generation uint64) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, err := s.db.ExecContext(ctx, `DELETE FROM input_decisions WHERE session_id=? AND generation<>?`, sessionID, generation)
	return err
}
