package store

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"time"
)

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

// ForgetOperation removes an operation's record so the same ID can run again.
func (s *Store) ForgetOperation(ctx context.Context, operationID string) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, err := s.db.ExecContext(ctx, `DELETE FROM operation_results WHERE operation_id=?`, operationID)
	return err
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
