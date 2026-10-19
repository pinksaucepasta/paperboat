package filetransfer

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

// CleanupExpired removes transfer records and content after their independent retention deadline.
func (s *Service) CleanupExpired(ctx context.Context) error {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		var refusal *Error
		if errors.As(err, &refusal) && refusal.Code == ResourceLimit {
			return nil
		}
		return err
	}
	defer releaseMutation()
	now := s.config.Now()
	transfers, err := s.config.Store.ExpiredFileTransfers(ctx, now)
	if err != nil {
		return classifyCleanupFailure(err)
	}
	var result error
	expired := make([]store.FileTransfer, 0, len(transfers))
	for _, transfer := range transfers {
		batchLock := s.lock("batch:" + transfer.BatchID)
		batchLock.Lock()
		s.signalCancel(transfer.ID)
		if err := s.waitForWrites(ctx, []store.FileTransfer{transfer}); err != nil {
			result = errors.Join(result, err)
			batchLock.Unlock()
			continue
		}
		transferLock := s.lock(transfer.ID)
		transferLock.Lock()
		current, getErr := s.config.Store.FileTransfer(ctx, transfer.ID)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				transferLock.Unlock()
				batchLock.Unlock()
				continue
			}
			result = errors.Join(result, getErr)
			transferLock.Unlock()
			batchLock.Unlock()
			continue
		}
		if current.ExpiresAt.After(now) || !expirationState(current.State) {
			s.cancels.Delete(current.ID)
			transferLock.Unlock()
			batchLock.Unlock()
			continue
		}
		if err := s.waitForWrites(ctx, []store.FileTransfer{current}); err != nil {
			result = errors.Join(result, err)
			transferLock.Unlock()
			batchLock.Unlock()
			continue
		}
		var removeErr error
		for _, path := range []string{s.partialPath(current.ID), s.contentPath(current.ID), s.publishedPath(current)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				removeErr = errors.Join(removeErr, err)
			}
		}
		if removeErr != nil {
			result = errors.Join(result, removeErr)
			transferLock.Unlock()
			batchLock.Unlock()
			continue
		}
		expired = append(expired, current)
		transferLock.Unlock()
		batchLock.Unlock()
	}
	if len(expired) > 0 {
		if err := syncDir(s.config.Root); err != nil {
			result = errors.Join(result, err)
		} else if err := s.config.Store.ExpireFileTransfers(ctx, expired); err != nil {
			result = errors.Join(result, err)
		} else {
			for _, transfer := range expired {
				s.cancels.Delete(transfer.ID)
			}
		}
	}
	return classifyCleanupFailure(result)
}

func expirationState(state string) bool {
	switch state {
	case "created", "uploading", "published", "pending":
		return true
	default:
		return false
	}
}

type CleanupWorker struct {
	Service  *Service
	Interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
}

func (w *CleanupWorker) Start(parent context.Context) error {
	if w == nil || w.Service == nil || parent == nil {
		return &Error{Code: InvalidPath}
	}
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	ctx, cancel := context.WithCancel(parent)
	w.cancel, w.done = cancel, make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		failureActive := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				err := w.Service.CleanupExpired(ctx)
				if err == nil {
					failureActive = false
					continue
				}
				if onlyContextTermination(err) {
					continue
				}
				if !failureActive {
					errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "transfer", "lifecycle", "file_transfer_failed", err)
					failureActive = true
				}
			}
		}
	}()
	return nil
}

func (w *CleanupWorker) Shutdown(ctx context.Context) error {
	if w.cancel == nil {
		return nil
	}
	w.cancel()
	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
