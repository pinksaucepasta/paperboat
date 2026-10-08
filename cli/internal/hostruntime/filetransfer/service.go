package filetransfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

const (
	MaxFileBytes  = int64(50 << 20)
	MaxBatchBytes = int64(500 << 20)
	MaxBatchFiles = 10
	Retention     = 7 * 24 * time.Hour

	// cancellationDrainTimeout bounds a DELETE while an HTTP upload is
	// unwinding. We must not remove a partial file until its active writer has
	// closed it: Windows rejects removal of an open file.
	cancellationDrainTimeout = 5 * time.Second
)

type Policy struct {
	Revision               string `json:"revision"`
	MaxFileBytes           int64  `json:"max_file_bytes"`
	MaxBatchFiles          int    `json:"max_batch_files"`
	MaxBatchBytes          int64  `json:"max_batch_bytes"`
	MaxConcurrentTransfers int    `json:"max_concurrent_transfers"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	DeliveryTimeoutSeconds int64  `json:"delivery_timeout_seconds"`
	MaxPendingSpoolBytes   int64  `json:"max_pending_spool_bytes"`
}

var DefaultPolicy = Policy{Revision: "file-transfer-v1", MaxFileBytes: MaxFileBytes, MaxBatchFiles: MaxBatchFiles, MaxBatchBytes: MaxBatchBytes, MaxConcurrentTransfers: 2, RetentionSeconds: int64(Retention / time.Second), DeliveryTimeoutSeconds: 600, MaxPendingSpoolBytes: 1 << 30}

type PolicyStore struct {
	mu     sync.RWMutex
	policy Policy
}

func NewPolicyStore(policy Policy) *PolicyStore {
	if !policy.Valid() {
		policy = DefaultPolicy
	}
	return &PolicyStore{policy: policy}
}
func (s *PolicyStore) Current() Policy { s.mu.RLock(); defer s.mu.RUnlock(); return s.policy }
func (s *PolicyStore) Update(policy Policy) error {
	if !policy.Valid() {
		return &Error{Code: InvalidSize}
	}
	s.mu.Lock()
	s.policy = policy
	s.mu.Unlock()
	return nil
}
func (p Policy) Valid() bool {
	return p.Revision != "" && p.MaxFileBytes > 0 && p.MaxFileBytes <= MaxFileBytes && p.MaxBatchFiles > 0 && p.MaxBatchFiles <= MaxBatchFiles && p.MaxBatchBytes >= p.MaxFileBytes && p.MaxBatchBytes <= MaxBatchBytes && p.MaxConcurrentTransfers > 0 && p.MaxConcurrentTransfers <= 2 && p.RetentionSeconds > 0 && p.DeliveryTimeoutSeconds > 0 && p.MaxPendingSpoolBytes >= p.MaxBatchBytes
}

type Code string

const (
	InvalidPath        Code = "invalid_path"
	InvalidSize        Code = "invalid_size"
	BatchLimit         Code = "batch_limit"
	OffsetConflict     Code = "offset_conflict"
	StateConflict      Code = "state_conflict"
	DigestMismatch     Code = "digest_mismatch"
	StorageUnavailable Code = "storage_unavailable"
	ResourceLimit      Code = "resource_limit"
	Canceled           Code = "canceled"
	DeliveryTimeout    Code = "delivery_timeout"
)

type Error struct {
	Code  Code
	Cause error
}

func (e *Error) Error() string {
	return string(e.Code)
}
func (e *Error) Unwrap() error { return e.Cause }

func lookupFailure(err error) error {
	if onlyContextTermination(err) {
		return err
	}
	code := StorageUnavailable
	if errors.Is(err, store.ErrNotFound) {
		code = InvalidPath
	}
	return &Error{Code: code, Cause: err}
}

type cleanupFailure struct{ cause error }

func (*cleanupFailure) Error() string           { return "file transfer retention cleanup failed" }
func (e *cleanupFailure) Unwrap() error         { return e.cause }
func (*cleanupFailure) DiagnosticStage() string { return "lifecycle" }
func (*cleanupFailure) DiagnosticCode() string  { return "file_transfer_failed" }

func classifyCleanupFailure(err error) error {
	if err == nil || onlyContextTermination(err) {
		return err
	}
	return &cleanupFailure{cause: err}
}

func onlyContextTermination(err error) bool {
	if err == nil {
		return false
	}
	remaining := []error{err}
	seen := make(map[error]struct{})
	leaves := 0
	for visited := 0; len(remaining) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := remaining[0]
		remaining = remaining[1:]
		if current == nil {
			return false
		}
		typeOf := reflect.TypeOf(current)
		if typeOf.Comparable() {
			if _, ok := seen[current]; ok {
				return false
			}
			seen[current] = struct{}{}
		}
		value := reflect.ValueOf(current)
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children) > 16-visited-1 || len(remaining)+len(children) > 16-visited-1 {
				return false
			}
			remaining = append(remaining, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(remaining)+1 > 16-visited-1 {
				return false
			}
			remaining = append(remaining, child)
		default:
			leaves++
			if current != context.Canceled && current != context.DeadlineExceeded {
				return false
			}
		}
	}
	return leaves > 0
}

type File struct {
	Basename string `json:"basename"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}
type CreateRequest struct {
	BatchID              string
	SourceMachineID      string
	DestinationMachineID string
	InitiatingUserID     string
	SessionID            string
	DeliveryClientID     string
	Files                []File
}

type Config struct {
	Root                     string
	PublishRoot              string
	LocalMachineID           string
	Store                    *store.Store
	Now                      func() time.Time
	Random                   io.Reader
	Policy                   *PolicyStore
	CancellationDrainTimeout time.Duration
}

type Service struct {
	config  Config
	slotMu  sync.Mutex
	active  int
	locks   sync.Map
	cancels sync.Map
	writes  sync.Map
}

type transferCancellation struct {
	once sync.Once
	done chan struct{}
}

type transferWrite struct{ done chan struct{} }

func New(config Config) (*Service, error) {
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Policy == nil {
		config.Policy = NewPolicyStore(DefaultPolicy)
	}
	if !filepath.IsAbs(config.Root) || config.LocalMachineID == "" || config.Store == nil {
		return nil, &Error{Code: InvalidPath}
	}
	if err := os.MkdirAll(config.Root, 0o700); err != nil {
		return nil, &Error{Code: StorageUnavailable, Cause: err}
	}
	if err := os.Chmod(config.Root, 0o700); err != nil {
		return nil, &Error{Code: StorageUnavailable, Cause: err}
	}
	return &Service{config: config}, nil
}

// ActiveCount reports current admitted content-write operations. Retained
// transfer files and completed uploads are not active operations.
func (s *Service) ActiveCount() uint64 {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	return uint64(s.active)
}

// Policy returns the current limits enforced by this receiver.
func (s *Service) Policy() Policy { return s.config.Policy.Current() }

func (s *Service) Create(ctx context.Context, request CreateRequest) ([]store.FileTransfer, error) {
	policy := s.config.Policy.Current()
	if request.BatchID == "" || request.SourceMachineID == "" || request.DestinationMachineID == "" || request.InitiatingUserID == "" || request.SourceMachineID == request.DestinationMachineID || request.DestinationMachineID != s.config.LocalMachineID && (request.SessionID == "" || request.DeliveryClientID == "") {
		return nil, &Error{Code: InvalidPath}
	}
	if len(request.Files) < 1 || len(request.Files) > policy.MaxBatchFiles {
		return nil, &Error{Code: BatchLimit}
	}
	now := s.config.Now()
	transfers := make([]store.FileTransfer, len(request.Files))
	var total int64
	for i, file := range request.Files {
		if !validBasename(file.Basename) {
			return nil, &Error{Code: InvalidPath}
		}
		if file.Size < 0 || file.Size > policy.MaxFileBytes {
			return nil, &Error{Code: InvalidSize}
		}
		if !validDigest(file.SHA256) {
			return nil, &Error{Code: DigestMismatch}
		}
		total += file.Size
		if total > policy.MaxBatchBytes {
			return nil, &Error{Code: BatchLimit}
		}
		id, err := s.newID()
		if err != nil {
			return nil, &Error{Code: StorageUnavailable, Cause: err}
		}
		expiresAt := now.Add(time.Duration(policy.RetentionSeconds) * time.Second)
		if request.DestinationMachineID != s.config.LocalMachineID {
			expiresAt = now.Add(time.Duration(policy.DeliveryTimeoutSeconds) * time.Second)
		}
		transfers[i] = store.FileTransfer{ID: id, BatchID: request.BatchID, SourceMachineID: request.SourceMachineID, DestinationMachineID: request.DestinationMachineID, InitiatingUserID: request.InitiatingUserID, SessionID: request.SessionID, DeliveryClientID: request.DeliveryClientID, Basename: file.Basename, Size: file.Size, SHA256: file.SHA256, State: "created", CreatedAt: now, ExpiresAt: expiresAt}
	}
	if err := s.config.Store.CreateFileTransfersWithinLimits(ctx, transfers, policy.MaxPendingSpoolBytes, policy.MaxConcurrentTransfers); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &Error{Code: ResourceLimit, Cause: err}
		}
		return nil, &Error{Code: StorageUnavailable, Cause: err}
	}
	for _, transfer := range transfers {
		s.cancelSignal(transfer.ID)
		file, err := os.OpenFile(s.partialPath(transfer.ID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = s.cancelBatch(context.Background(), transfers)
			return nil, &Error{Code: StorageUnavailable, Cause: err}
		}
		if err := file.Close(); err != nil {
			_ = s.cancelBatch(context.Background(), transfers)
			return nil, &Error{Code: StorageUnavailable, Cause: err}
		}
	}
	if err := syncDir(s.config.Root); err != nil {
		_ = s.cancelBatch(context.Background(), transfers)
		return nil, &Error{Code: StorageUnavailable, Cause: err}
	}
	return transfers, nil
}

// CleanupExpired removes transfer records and content after their independent retention deadline.
func (s *Service) CleanupExpired(ctx context.Context) error {
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

func (s *Service) Append(ctx context.Context, id string, offset int64, body io.Reader) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, nil)
}

// AppendVerified commits at most one independently verified native chunk. The
// existing writer slots bound both buffering and storage work for all callers.
func (s *Service) AppendVerified(ctx context.Context, id string, offset int64, body io.Reader, digest [sha256.Size]byte) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, &digest)
}

func (s *Service) append(ctx context.Context, id string, offset int64, body io.Reader, digest *[sha256.Size]byte) (store.FileTransfer, error) {
	if err := s.acquire(ctx); err != nil {
		return store.FileTransfer{}, err
	}
	defer s.release()
	lock := s.lock(id)
	lock.Lock()
	defer lock.Unlock()
	finish := s.beginWrite(id)
	defer finish()
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return store.FileTransfer{}, lookupFailure(err)
	}
	if transfer.State == "canceled" {
		return store.FileTransfer{}, &Error{Code: Canceled}
	}
	if offset != transfer.CommittedOffset {
		return transfer, &Error{Code: OffsetConflict}
	}
	if digest != nil {
		chunk, readErr := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: body, canceled: s.cancelSignal(id)}, protocol.FileTransferChunkBytes+1))
		if readErr != nil {
			if ctx.Err() != nil {
				return transfer, ctx.Err()
			}
			select {
			case <-s.cancelSignal(id):
				return transfer, &Error{Code: Canceled}
			default:
			}
			return transfer, classifyIO(readErr)
		}
		if len(chunk) > protocol.FileTransferChunkBytes {
			return transfer, &Error{Code: InvalidSize}
		}
		if sha256.Sum256(chunk) != *digest {
			return transfer, &Error{Code: DigestMismatch}
		}
		body = bytes.NewReader(chunk)
	}
	file, err := os.OpenFile(s.partialPath(id), os.O_WRONLY, 0)
	if err != nil {
		return transfer, &Error{Code: StorageUnavailable, Cause: err}
	}
	defer file.Close()
	if err := file.Truncate(offset); err != nil {
		return transfer, &Error{Code: StorageUnavailable, Cause: err}
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return transfer, &Error{Code: StorageUnavailable, Cause: err}
	}
	remaining := transfer.Size - offset
	canceled := s.cancelSignal(id)
	written, err := io.Copy(file, io.LimitReader(&contextReader{ctx: ctx, reader: body, canceled: canceled}, remaining+1))
	if err != nil {
		if ctx.Err() != nil {
			return transfer, ctx.Err()
		}
		select {
		case <-canceled:
			return transfer, &Error{Code: Canceled}
		default:
		}
		return transfer, classifyIO(err)
	}
	select {
	case <-canceled:
		_ = file.Truncate(offset)
		return transfer, &Error{Code: Canceled}
	default:
	}
	if written > remaining {
		_ = file.Truncate(offset)
		return transfer, &Error{Code: InvalidSize}
	}
	if err := file.Sync(); err != nil {
		return transfer, &Error{Code: StorageUnavailable, Cause: err}
	}
	if err := s.config.Store.CommitFileTransferOffset(ctx, id, offset, offset+written); err != nil {
		return transfer, &Error{Code: OffsetConflict, Cause: err}
	}
	transfer.CommittedOffset += written
	transfer.State = "uploading"
	return transfer, nil
}

func (s *Service) acquire(ctx context.Context) error {
	for {
		s.slotMu.Lock()
		limit := s.config.Policy.Current().MaxConcurrentTransfers
		if s.active < limit {
			s.active++
			s.slotMu.Unlock()
			return nil
		}
		s.slotMu.Unlock()
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (s *Service) release() { s.slotMu.Lock(); s.active--; s.slotMu.Unlock() }

func (s *Service) Complete(ctx context.Context, id string) (store.FileTransfer, error) {
	requested, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return store.FileTransfer{}, lookupFailure(err)
	}
	lock := s.lock("batch:" + requested.BatchID)
	lock.Lock()
	defer lock.Unlock()
	transfers, err := s.config.Store.FileTransfersByBatch(ctx, requested.BatchID)
	if err != nil {
		return store.FileTransfer{}, lookupFailure(err)
	}
	allTerminal := true
	for _, transfer := range transfers {
		if transfer.State != "published" && transfer.State != "pending" && transfer.State != "delivered" {
			allTerminal = false
		}
	}
	if allTerminal {
		transfer, getErr := s.config.Store.FileTransfer(ctx, id)
		if getErr != nil {
			return store.FileTransfer{}, lookupFailure(getErr)
		}
		return transfer, nil
	}
	for _, transfer := range transfers {
		if transfer.CommittedOffset != transfer.Size || transfer.State != "created" && transfer.State != "uploading" {
			return requested, &Error{Code: InvalidSize}
		}
		contentPath := s.partialPath(transfer.ID)
		if _, statErr := os.Stat(contentPath); errors.Is(statErr, os.ErrNotExist) {
			contentPath = s.contentPath(transfer.ID)
		}
		file, openErr := os.Open(contentPath)
		if openErr != nil {
			return requested, &Error{Code: StorageUnavailable, Cause: openErr}
		}
		hash := sha256.New()
		written, copyErr := io.Copy(hash, &contextReader{ctx: ctx, reader: file, canceled: s.cancelSignal(transfer.ID)})
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return requested, classifyIO(errors.Join(copyErr, closeErr))
		}
		if written != transfer.Size || hex.EncodeToString(hash.Sum(nil)) != transfer.SHA256 {
			_ = s.cancelBatchLocked(context.Background(), transfers)
			return requested, &Error{Code: DigestMismatch}
		}
	}
	var renamed []store.FileTransfer
	for _, transfer := range transfers {
		if _, statErr := os.Stat(s.contentPath(transfer.ID)); statErr == nil {
			continue
		}
		//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=verified-content-commit
		if renameErr := os.Rename(s.partialPath(transfer.ID), s.contentPath(transfer.ID)); renameErr != nil {
			for index := len(renamed) - 1; index >= 0; index-- {
				//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=batch-commit-rollback
				_ = os.Rename(s.contentPath(renamed[index].ID), s.partialPath(renamed[index].ID))
			}
			return requested, &Error{Code: StorageUnavailable, Cause: renameErr}
		}
		renamed = append(renamed, transfer)
	}
	if err := syncDir(s.config.Root); err != nil {
		return requested, &Error{Code: StorageUnavailable, Cause: err}
	}
	if err := s.config.Store.CompleteFileTransferBatch(ctx, requested.BatchID, s.config.LocalMachineID); err != nil {
		return requested, &Error{Code: StorageUnavailable, Cause: err}
	}
	for _, transfer := range transfers {
		if transfer.DestinationMachineID == s.config.LocalMachineID {
			s.cancels.Delete(transfer.ID)
		}
	}
	return s.config.Store.FileTransfer(ctx, id)
}

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

func (s *Service) PublishedPath(ctx context.Context, id string) (string, error) {
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return "", lookupFailure(err)
	}
	if transfer.DestinationMachineID != s.config.LocalMachineID || transfer.State != "published" {
		return "", &Error{Code: InvalidPath}
	}
	contentPath := s.contentPath(id)
	if s.config.PublishRoot != "" {
		return s.publishToRoot(contentPath, transfer)
	}
	path := s.publishedPath(transfer)
	if err := os.Link(contentPath, path); err != nil && !errors.Is(err, os.ErrExist) {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	contentInfo, contentErr := os.Stat(contentPath)
	info, err := os.Lstat(path)
	if contentErr != nil || err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !os.SameFile(contentInfo, info) {
		return "", &Error{Code: StorageUnavailable, Cause: errors.Join(contentErr, err)}
	}
	if err := syncDir(s.config.Root); err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	return path, nil
}

// ExistingPublishedPath resolves the already-published destination without
// creating or repairing any filesystem entry. Status requests use this path so
// a read-only inspection can report the exact collision-resolved Inbox name.
func (s *Service) ExistingPublishedPath(ctx context.Context, id string) (string, error) {
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return "", lookupFailure(err)
	}
	if transfer.DestinationMachineID != s.config.LocalMachineID || transfer.State != "published" {
		return "", &Error{Code: InvalidPath}
	}
	contentInfo, err := os.Stat(s.contentPath(id))
	if err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	if s.config.PublishRoot == "" {
		path := s.publishedPath(transfer)
		matches, matchErr := publishedFileMatches(contentInfo, path)
		if matchErr != nil {
			return "", &Error{Code: StorageUnavailable, Cause: matchErr}
		}
		if !matches {
			return "", &Error{Code: InvalidPath}
		}
		return path, nil
	}
	root := filepath.Clean(s.config.PublishRoot)
	if !filepath.IsAbs(root) || root != s.config.PublishRoot {
		return "", &Error{Code: StorageUnavailable, Cause: errors.New("publish root is invalid")}
	}
	for index := 1; index <= 10000; index++ {
		path := filepath.Join(root, publishedName(transfer.Basename, index))
		matches, matchErr := publishedFileMatches(contentInfo, path)
		if matchErr != nil {
			return "", &Error{Code: StorageUnavailable, Cause: matchErr}
		}
		if matches {
			return path, nil
		}
	}
	return "", &Error{Code: InvalidPath}
}

func publishedFileMatches(contentInfo os.FileInfo, path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(contentInfo, info), nil
}

func (s *Service) publishToRoot(contentPath string, transfer store.FileTransfer) (string, error) {
	root := filepath.Clean(s.config.PublishRoot)
	if !filepath.IsAbs(root) || root != s.config.PublishRoot {
		return "", &Error{Code: StorageUnavailable, Cause: errors.New("publish root is invalid")}
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	contentInfo, err := os.Stat(contentPath)
	if err != nil {
		return "", &Error{Code: StorageUnavailable, Cause: err}
	}
	for index := 1; index <= 10000; index++ {
		path := filepath.Join(root, publishedName(transfer.Basename, index))
		info, statErr := os.Lstat(path)
		if statErr == nil {
			if info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && os.SameFile(contentInfo, info) {
				return path, nil
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", &Error{Code: StorageUnavailable, Cause: statErr}
		}
		if err := os.Link(contentPath, path); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		if err := syncDir(root); err != nil {
			return "", &Error{Code: StorageUnavailable, Cause: err}
		}
		return path, nil
	}
	return "", &Error{Code: StorageUnavailable, Cause: errors.New("inbox name collision limit reached")}
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
	transfer, err := s.config.Store.FileTransfer(ctx, id)
	if err != nil {
		return err
	}
	lock := s.lock("batch:" + transfer.BatchID)
	lock.Lock()
	defer lock.Unlock()
	transfers, err := s.config.Store.FileTransfersByBatch(ctx, transfer.BatchID)
	if err != nil {
		return err
	}
	return s.cancelBatchLocked(ctx, transfers)
}

// CancelActive stops every transfer currently writing on this receiver. New
// admission is closed by the capability gate before this cleanup begins.
func (s *Service) CancelActive(ctx context.Context) error {
	ids := make([]string, 0)
	s.writes.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	var result error
	for _, id := range ids {
		result = errors.Join(result, s.Cancel(ctx, id))
	}
	return result
}

func (s *Service) cancelBatch(ctx context.Context, transfers []store.FileTransfer) error {
	if len(transfers) == 0 {
		return nil
	}
	lock := s.lock("batch:" + transfers[0].BatchID)
	lock.Lock()
	defer lock.Unlock()
	return s.cancelBatchLocked(ctx, transfers)
}

func (s *Service) cancelBatchLocked(ctx context.Context, transfers []store.FileTransfer) error {
	var result error
	if len(transfers) > 0 {
		if err := s.config.Store.CancelFileTransferBatch(ctx, transfers[0].BatchID); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return &Error{Code: StateConflict, Cause: err}
			}
			return &Error{Code: StorageUnavailable, Cause: err}
		}
	}
	for _, transfer := range transfers {
		s.signalCancel(transfer.ID)
	}
	if err := s.waitForWrites(ctx, transfers); err != nil {
		return errors.Join(result, &Error{Code: StorageUnavailable, Cause: err})
	}
	for _, transfer := range transfers {
		for _, path := range []string{s.partialPath(transfer.ID), s.contentPath(transfer.ID), s.publishedPath(transfer)} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				result = errors.Join(result, err)
			}
		}
		s.cancels.Delete(transfer.ID)
	}
	return errors.Join(result, syncDir(s.config.Root))
}
func (s *Service) partialPath(id string) string { return filepath.Join(s.config.Root, id+".part") }
func (s *Service) contentPath(id string) string { return filepath.Join(s.config.Root, id+".content") }
func (s *Service) publishedPath(transfer store.FileTransfer) string {
	return filepath.Join(s.config.Root, transfer.ID+"-"+transfer.Basename)
}
func (s *Service) lock(id string) *sync.Mutex {
	value, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}
func (s *Service) cancelSignal(id string) <-chan struct{} {
	value, _ := s.cancels.LoadOrStore(id, &transferCancellation{done: make(chan struct{})})
	return value.(*transferCancellation).done
}
func (s *Service) signalCancel(id string) {
	value, _ := s.cancels.LoadOrStore(id, &transferCancellation{done: make(chan struct{})})
	cancel := value.(*transferCancellation)
	cancel.once.Do(func() { close(cancel.done) })
}
func (s *Service) CancellationSignal(id string) <-chan struct{} { return s.cancelSignal(id) }

func (s *Service) beginWrite(id string) func() {
	write := &transferWrite{done: make(chan struct{})}
	s.writes.Store(id, write)
	return func() {
		close(write.done)
		s.writes.CompareAndDelete(id, write)
	}
}

func (s *Service) waitForWrites(ctx context.Context, transfers []store.FileTransfer) error {
	timeout := s.config.CancellationDrainTimeout
	if timeout <= 0 {
		timeout = cancellationDrainTimeout
	}
	drain, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for _, transfer := range transfers {
		value, ok := s.writes.Load(transfer.ID)
		if !ok {
			continue
		}
		write := value.(*transferWrite)
		select {
		case <-write.done:
		case <-drain.Done():
			return drain.Err()
		}
	}
	return nil
}
func (s *Service) newID() (string, error) {
	id, err := uuid.NewRandomFromReader(s.config.Random)
	if err != nil {
		return "", err
	}
	return "transfer_" + id.String(), nil
}
func validDigest(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32
}
func validBasename(value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= 255 && value == filepath.Base(value) && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00")
}

type contextReader struct {
	ctx      context.Context
	reader   io.Reader
	canceled <-chan struct{}
}

func (r *contextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.canceled:
		return 0, context.Canceled
	default:
		return r.reader.Read(p)
	}
}
func classifyIO(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &Error{Code: StorageUnavailable, Cause: err}
}
func (e *Error) Format(state fmt.State, verb rune) { _, _ = fmt.Fprint(state, e.Error()) }
