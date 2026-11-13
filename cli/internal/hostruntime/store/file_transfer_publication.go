package store

import "context"

// SetFileTransferPublicationPath reserves the receiver's durable destination
// before creating its hard link. The compare-and-set also fences restarts and
// collision retries; sender-provided delivery receipts use a different state.
func (s *Store) SetFileTransferPublicationPath(ctx context.Context, id, localMachineID, previous, path string) (resultErr error) {
	defer protectStoreError(&resultErr)
	result, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET receipt_path=? WHERE id=? AND destination_machine_id=? AND state='published' AND COALESCE(receipt_path,'')=?`, path, id, localMachineID, previous)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrConflict
	}
	return nil
}

// FailFileTransferPublication records that the receiver could not copy a completed upload into its Inbox. The spool
// content stays so a repeated complete can publish it again; the result code says why.
func (s *Store) FailFileTransferPublication(ctx context.Context, id, localMachineID, resultCode string) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state='failed',result_code=? WHERE id=? AND destination_machine_id=? AND state='published'`, resultCode, id, localMachineID)
	return err
}

// ReopenFileTransferPublication makes a transfer whose Inbox copy failed publishable again.
func (s *Store) ReopenFileTransferPublication(ctx context.Context, id, localMachineID string) (resultErr error) {
	defer protectStoreError(&resultErr)
	_, err := s.db.ExecContext(ctx, `UPDATE file_transfers SET state='published',result_code='published' WHERE id=? AND destination_machine_id=? AND state='failed' AND delivery_client_id IS NULL`, id, localMachineID)
	return err
}
