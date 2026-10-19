package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func (s *Service) Get(ctx context.Context, id string) (store.FileTransfer, error) {
	return s.config.Store.FileTransfer(ctx, id)
}

func (s *Service) Batch(ctx context.Context, batchID string) ([]store.FileTransfer, error) {
	return s.config.Store.FileTransfersByBatch(ctx, batchID)
}

func (s *Service) List(ctx context.Context, sourceMachineID, userID, sessionID string, limit, offset int, q, state string) (store.FileTransferPage, error) {
	return s.config.Store.FileTransfersForSource(ctx, sourceMachineID, userID, sessionID, limit, offset, q, state)
}

func (s *Service) Pending(ctx context.Context, clientID, sessionID string, limit int) ([]store.FileTransfer, error) {
	return s.config.Store.PendingFileTransfers(ctx, clientID, sessionID, s.config.Now(), limit)
}

func (s *Service) Receipt(ctx context.Context, id, clientID, resultCode, receiptPath string) error {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return err
	}
	defer releaseMutation()
	if err := s.config.Store.ReceiptFileTransfer(ctx, id, clientID, resultCode, receiptPath); err != nil {
		return err
	}
	if err := os.Remove(s.contentPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: StorageUnavailable, Cause: err}
	}
	s.cancels.Delete(id)
	return syncDir(s.config.Root)
}

func (s *Service) ReceiptBatch(ctx context.Context, receipts []store.FileTransferReceipt) error {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return err
	}
	defer releaseMutation()
	if err := s.config.Store.ReceiptFileTransferBatch(ctx, receipts); err != nil {
		return err
	}
	for _, receipt := range receipts {
		if err := os.Remove(s.contentPath(receipt.ID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return &Error{Code: StorageUnavailable, Cause: err}
		}
		s.cancels.Delete(receipt.ID)
	}
	return syncDir(s.config.Root)
}

func (s *Service) OpenContent(ctx context.Context, id string) (*os.File, store.FileTransfer, error) {
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return nil, transfer, lookupFailure(err)
	}
	if transfer.State != "published" && transfer.State != "pending" && transfer.State != "delivered" {
		return nil, transfer, &Error{Code: InvalidPath}
	}
	file, err := os.Open(s.contentPath(id))
	if err != nil {
		return nil, transfer, &Error{Code: StorageUnavailable, Cause: err}
	}
	return file, transfer, nil
}

func publishedName(basename string, index int) string {
	if index <= 1 {
		return basename
	}
	extension := filepath.Ext(basename)
	stem := strings.TrimSuffix(basename, extension)
	return fmt.Sprintf("%s (%d)%s", stem, index, extension)
}

func (s *Service) Cancel(ctx context.Context, id string) error {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return err
	}
	defer releaseMutation()
	return s.cancel(ctx, id)
}
