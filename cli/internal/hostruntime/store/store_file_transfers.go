package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store/storesqlc"
)

type FileTransfer struct {
	ID                   string    `json:"transfer_id"`
	BatchID              string    `json:"batch_id"`
	SourceMachineID      string    `json:"source_machine_id"`
	DestinationMachineID string    `json:"destination_machine_id"`
	InitiatingUserID     string    `json:"initiating_user_id"`
	SessionID            string    `json:"session_id,omitempty"`
	DeliveryClientID     string    `json:"-"`
	Basename             string    `json:"basename"`
	Size                 int64     `json:"size"`
	SHA256               string    `json:"sha256"`
	CommittedOffset      int64     `json:"committed_offset"`
	State                string    `json:"state"`
	ResultCode           string    `json:"result_code,omitempty"`
	ReceiptPath          string    `json:"receipt_path,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	ExpiresAt            time.Time `json:"expires_at"`
}

type FileTransferReceipt struct {
	ID         string
	ClientID   string
	ResultCode string
	Path       string
}

func (s *Store) CreateFileTransfers(ctx context.Context, transfers []FileTransfer) (resultErr error) {
	defer protectStoreError(&resultErr)
	return s.CreateFileTransfersWithinLimits(ctx, transfers, 0, 0)
}

func (s *Store) CreateFileTransfersWithinSpool(ctx context.Context, transfers []FileTransfer, maxSpoolBytes int64) (resultErr error) {
	defer protectStoreError(&resultErr)
	return s.CreateFileTransfersWithinLimits(ctx, transfers, maxSpoolBytes, 0)
}

func (s *Store) CreateFileTransfersWithinLimits(ctx context.Context, transfers []FileTransfer, maxSpoolBytes int64, maxActiveBatches int) (resultErr error) {
	defer protectStoreError(&resultErr)
	if len(transfers) < 1 || len(transfers) > 10 {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	if maxActiveBatches > 0 {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT batch_id) FROM file_transfers WHERE state IN ('created','uploading','pending')`).Scan(&active); err != nil {
			return err
		}
		if active >= maxActiveBatches {
			return ErrConflict
		}
	}
	if maxSpoolBytes > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size),0) FROM file_transfers WHERE state IN ('created','uploading','pending')`).Scan(&used); err != nil {
			return err
		}
		var incoming int64
		for _, transfer := range transfers {
			incoming += transfer.Size
		}
		if incoming < 0 || used > maxSpoolBytes-incoming {
			return ErrConflict
		}
	}
	for _, transfer := range transfers {
		if transfer.ID == "" || transfer.BatchID == "" || transfer.SourceMachineID == "" || transfer.DestinationMachineID == "" || transfer.InitiatingUserID == "" || transfer.Basename == "" || transfer.Size < 0 || transfer.CommittedOffset != 0 || transfer.CreatedAt.IsZero() || transfer.ExpiresAt.IsZero() {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO file_transfers(id,batch_id,source_machine_id,destination_machine_id,initiating_user_id,session_id,delivery_client_id,basename,size,sha256,committed_offset,state,result_code,receipt_path,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,0,'created',NULL,NULL,?,?)`, transfer.ID, transfer.BatchID, transfer.SourceMachineID, transfer.DestinationMachineID, transfer.InitiatingUserID, nullableString(transfer.SessionID), nullableString(transfer.DeliveryClientID), transfer.Basename, transfer.Size, transfer.SHA256, transfer.CreatedAt.UnixNano(), transfer.ExpiresAt.UnixNano()); err != nil {
			return classify(err)
		}
	}
	return tx.Commit()
}

func (s *Store) FileTransfer(ctx context.Context, id string) (transferResult FileTransfer, resultErr error) {
	defer protectStoreError(&resultErr)
	row, err := s.q.FileTransfer(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			return FileTransfer{}, ErrNotFound
		}
		return FileTransfer{}, err
	}
	return fileTransferFromSQLC(row), nil
}

func (s *Store) FileTransfersByBatch(ctx context.Context, batchID string) (transferResults []FileTransfer, resultErr error) {
	defer protectStoreError(&resultErr)
	if batchID == "" {
		return nil, ErrConflict
	}
	rows, err := s.q.FileTransfersByBatch(ctx, batchID)
	if err != nil {
		return nil, err
	}
	transfers := make([]FileTransfer, 0, len(rows))
	for _, row := range rows {
		transfers = append(transfers, fileTransferFromSQLC(row))
	}
	if len(transfers) == 0 {
		return nil, ErrNotFound
	}
	return transfers, nil
}

type FileTransferPage struct {
	Items      []FileTransfer                  `json:"items"`
	Pagination protocol.FileTransferPagination `json:"pagination"`
}

func (s *Store) FileTransfersForSource(ctx context.Context, sourceMachineID, userID, sessionID string, limit, offset int, q, state string) (pageResult FileTransferPage, resultErr error) {
	defer protectStoreError(&resultErr)
	if sourceMachineID == "" || userID == "" || limit < 1 || limit > 200 || offset < 0 || !protocol.ValidFileTransferHistoryFilters(q, state) {
		return FileTransferPage{}, ErrConflict
	}
	where := ` WHERE source_machine_id=? AND initiating_user_id=?`
	args := []any{sourceMachineID, userID}
	if sessionID != "" {
		where += ` AND session_id=?`
		args = append(args, sessionID)
	}
	if state != "" {
		where += ` AND state=?`
		args = append(args, state)
	}
	if q != "" {
		where += ` AND (instr(lower(id),lower(?))>0 OR instr(lower(basename),lower(?))>0)`
		args = append(args, q, q)
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return FileTransferPage{}, err
	}
	defer retainRollback(tx, &resultErr)
	page := FileTransferPage{Items: []FileTransfer{}, Pagination: protocol.FileTransferPagination{Limit: limit, Offset: offset}}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_transfers`+where, args...).Scan(&page.Pagination.Total); err != nil {
		return FileTransferPage{}, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := tx.QueryContext(ctx, fileTransferSelect+where+` ORDER BY created_at DESC,id DESC LIMIT ? OFFSET ?`, pageArgs...)
	if err != nil {
		return FileTransferPage{}, err
	}
	for rows.Next() {
		transfer, err := scanFileTransfer(rows)
		if err != nil {
			return FileTransferPage{}, storeFailure("runtime state query failed", err, rows.Close())
		}
		page.Items = append(page.Items, transfer)
	}
	err = storeFailure("runtime state query failed", rows.Err(), rows.Close())
	if err != nil {
		return FileTransferPage{}, err
	}
	if next := offset + len(page.Items); next < page.Pagination.Total {
		page.Pagination.NextOffset = &next
	}
	if err := tx.Commit(); err != nil {
		return FileTransferPage{}, err
	}
	return page, nil
}

func (s *Store) CommitFileTransferOffset(ctx context.Context, id string, expected, next int64) (resultErr error) {
	defer protectStoreError(&resultErr)
	if expected < 0 || next < expected {
		return ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET committed_offset=?,state='uploading' WHERE id=? AND committed_offset=? AND state IN ('created','uploading') AND ?<=size`, next, id, expected, next)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) CompleteFileTransfer(ctx context.Context, id, localMachineID string) (resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state=CASE destination_machine_id WHEN ? THEN 'published' ELSE 'pending' END,result_code=CASE destination_machine_id WHEN ? THEN 'published' ELSE 'pending' END WHERE id=? AND committed_offset=size AND state IN ('created','uploading')`, localMachineID, localMachineID, id)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed != 1 {
		transfer, getErr := s.FileTransfer(ctx, id)
		if getErr == nil && (transfer.State == "published" || transfer.State == "pending" || transfer.State == "delivered") {
			return nil
		}
		if getErr != nil && getErr != ErrNotFound {
			return getErr
		}
		return ErrConflict
	}
	return nil
}

func (s *Store) CompleteFileTransferBatch(ctx context.Context, batchID, localMachineID string) (resultErr error) {
	defer protectStoreError(&resultErr)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	var total, ready int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN committed_offset=size AND state IN ('created','uploading') THEN 1 ELSE 0 END),0) FROM file_transfers WHERE batch_id=?`, batchID).Scan(&total, &ready); err != nil {
		return err
	}
	if total < 1 || ready != total {
		var terminal int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_transfers WHERE batch_id=? AND state IN ('published','pending','delivered')`, batchID).Scan(&terminal); err != nil {
			return err
		}
		if terminal == total {
			return nil
		}
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state=CASE destination_machine_id WHEN ? THEN 'published' ELSE 'pending' END,result_code=CASE destination_machine_id WHEN ? THEN 'published' ELSE 'pending' END WHERE batch_id=? AND committed_offset=size AND state IN ('created','uploading')`, localMachineID, localMachineID, batchID)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if int(changed) != total {
		return ErrConflict
	}
	return tx.Commit()
}

// CancelFileTransferBatch commits cancellation only when every member is cancelable.
// The single guarded statement also arbitrates concurrent delivery receipts.
func (s *Store) CancelFileTransferBatch(ctx context.Context, batchID string) (resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state='canceled',result_code='canceled' WHERE batch_id=? AND state IN ('created','uploading','pending') AND NOT EXISTS (SELECT 1 FROM file_transfers AS member WHERE member.batch_id=? AND member.state NOT IN ('created','uploading','pending','canceled'))`, batchID, batchID)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed == 0 {
		var total, canceled int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN state='canceled' THEN 1 ELSE 0 END),0) FROM file_transfers WHERE batch_id=?`, batchID).Scan(&total, &canceled); err != nil {
			return err
		}
		if total > 0 && total == canceled {
			return nil
		}
		return ErrConflict
	}
	return nil
}

func (s *Store) PendingFileTransfers(ctx context.Context, clientID, sessionID string, now time.Time, limit int) (transferResults []FileTransfer, resultErr error) {
	defer protectStoreError(&resultErr)
	if clientID == "" || sessionID == "" || limit < 1 || limit > 10 {
		return nil, ErrConflict
	}
	rows, err := s.q.PendingFileTransfers(ctx, storesqlc.PendingFileTransfersParams{
		DeliveryClientID: nullableSQLString(clientID), SessionID: nullableSQLString(sessionID),
		ExpiresAt: now.UnixNano(), Limit: int64(limit),
	})
	if err != nil {
		return nil, err
	}
	transfers := make([]FileTransfer, 0, len(rows))
	for _, row := range rows {
		transfers = append(transfers, fileTransferFromSQLC(row))
	}
	return transfers, nil
}

func (s *Store) ExpiredFileTransfers(ctx context.Context, now time.Time) (transferResults []FileTransfer, resultErr error) {
	defer protectStoreError(&resultErr)
	rows, err := s.q.ExpiredFileTransfers(ctx, now.UnixNano())
	if err != nil {
		return nil, err
	}
	transfers := make([]FileTransfer, 0, len(rows))
	for _, row := range rows {
		transfers = append(transfers, fileTransferFromSQLC(row))
	}
	return transfers, nil
}

func (s *Store) ExpireFileTransfers(ctx context.Context, transfers []FileTransfer) (resultErr error) {
	defer protectStoreError(&resultErr)
	if len(transfers) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	for _, transfer := range transfers {
		state, code := "canceled", "canceled"
		if transfer.DeliveryClientID != "" {
			state, code = "failed", "delivery_timeout"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state=?,result_code=? WHERE id=? AND expires_at<=? AND state IN ('created','uploading','published','pending')`, state, code, transfer.ID, transfer.ExpiresAt.UnixNano()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ReceiptFileTransfer(ctx context.Context, id, clientID, resultCode, receiptPath string) (resultErr error) {
	defer protectStoreError(&resultErr)
	state := "failed"
	if resultCode == "stored" {
		state = "delivered"
	} else if resultCode == "" {
		return ErrConflict
	}
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state=?,result_code=?,receipt_path=? WHERE id=? AND delivery_client_id=? AND (state='pending' OR (state IN ('delivered','failed') AND COALESCE(result_code,'')=? AND COALESCE(receipt_path,'')=?))`, state, resultCode, nullableString(receiptPath), id, clientID, resultCode, receiptPath)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

func (s *Store) ReceiptFileTransferBatch(ctx context.Context, receipts []FileTransferReceipt) (resultErr error) {
	defer protectStoreError(&resultErr)
	if len(receipts) == 0 || len(receipts) > 10 {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer retainRollback(tx, &resultErr)
	for _, receipt := range receipts {
		state := "failed"
		if receipt.ResultCode == "stored" {
			state = "delivered"
		} else if receipt.ResultCode == "" || receipt.Path != "" {
			return ErrConflict
		}
		result, err := tx.ExecContext(ctx, `UPDATE file_transfers SET state=?,result_code=?,receipt_path=? WHERE id=? AND delivery_client_id=? AND (state='pending' OR (state IN ('delivered','failed') AND COALESCE(result_code,'')=? AND COALESCE(receipt_path,'')=?))`, state, receipt.ResultCode, nullableString(receipt.Path), receipt.ID, receipt.ClientID, receipt.ResultCode, receipt.Path)
		if err != nil {
			return err
		}
		changed, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return affectedErr
		}
		if changed != 1 {
			return ErrConflict
		}
	}
	return tx.Commit()
}

func (s *Store) CancelFileTransfer(ctx context.Context, id string) (resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state='canceled',result_code='canceled' WHERE id=? AND state IN ('created','uploading','pending')`, id)
	if err != nil {
		return err
	}
	changed, affectedErr := result.RowsAffected()
	if affectedErr != nil {
		return affectedErr
	}
	if changed == 0 {
		transfer, getErr := s.FileTransfer(ctx, id)
		if getErr == nil && transfer.State == "canceled" {
			return nil
		}
		if getErr != nil && getErr != ErrNotFound {
			return getErr
		}
		return ErrConflict
	}
	return nil
}

const fileTransferSelect = `SELECT id,batch_id,source_machine_id,destination_machine_id,initiating_user_id,COALESCE(session_id,''),COALESCE(delivery_client_id,''),basename,size,sha256,committed_offset,state,COALESCE(result_code,''),COALESCE(receipt_path,''),created_at,expires_at FROM file_transfers`

func scanFileTransfer(row scanner) (FileTransfer, error) {
	var transfer FileTransfer
	var created, expires int64
	err := row.Scan(&transfer.ID, &transfer.BatchID, &transfer.SourceMachineID, &transfer.DestinationMachineID, &transfer.InitiatingUserID, &transfer.SessionID, &transfer.DeliveryClientID, &transfer.Basename, &transfer.Size, &transfer.SHA256, &transfer.CommittedOffset, &transfer.State, &transfer.ResultCode, &transfer.ReceiptPath, &created, &expires)
	if err != nil {
		return FileTransfer{}, err
	}
	transfer.CreatedAt, transfer.ExpiresAt = time.Unix(0, created).UTC(), time.Unix(0, expires).UTC()
	return transfer, nil
}

func fileTransferFromSQLC(row storesqlc.FileTransfer) FileTransfer {
	return FileTransfer{
		ID: row.ID, BatchID: row.BatchID, SourceMachineID: row.SourceMachineID,
		DestinationMachineID: row.DestinationMachineID, InitiatingUserID: row.InitiatingUserID,
		SessionID: row.SessionID.String, DeliveryClientID: row.DeliveryClientID.String,
		Basename: row.Basename, Size: row.Size, SHA256: row.Sha256, CommittedOffset: row.CommittedOffset,
		State: row.State, ResultCode: row.ResultCode.String, ReceiptPath: row.ReceiptPath.String,
		CreatedAt: time.Unix(0, row.CreatedAt).UTC(), ExpiresAt: time.Unix(0, row.ExpiresAt).UTC(),
	}
}
