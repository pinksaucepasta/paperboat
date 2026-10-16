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
