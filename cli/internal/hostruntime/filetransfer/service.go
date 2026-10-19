package filetransfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

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

// BeginUpdate closes transfer mutation admission and waits for admitted
// work to finish its durable commit. Interrupted updates reopen admission.
func (s *Service) BeginUpdate(ctx context.Context, transactionID string) error {
	if ctx == nil || transactionID == "" || len(transactionID) > 128 {
		return &Error{Code: InvalidPath}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.slotMu.Lock()
	if s.updateTransaction != "" {
		if s.updateTransaction == transactionID && s.operations == 0 {
			s.slotMu.Unlock()
			return nil
		}
		s.slotMu.Unlock()
		return &Error{Code: ResourceLimit}
	}
	s.updateTransaction = transactionID
	s.updateGeneration++
	generation := s.updateGeneration
	done, pending := s.operationDone, s.operations != 0
	s.slotMu.Unlock()
	if pending {
		select {
		case <-done:
		case <-ctx.Done():
			s.slotMu.Lock()
			if s.updateGeneration == generation && s.updateTransaction == transactionID {
				s.updateTransaction = ""
			}
			s.slotMu.Unlock()
			return ctx.Err()
		}
	}
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	if err := ctx.Err(); err != nil {
		if s.updateGeneration == generation && s.updateTransaction == transactionID {
			s.updateTransaction = ""
		}
		return err
	}
	if s.updateGeneration != generation || s.updateTransaction != transactionID || s.operations != 0 {
		return &Error{Code: ResourceLimit}
	}
	return nil
}

// EndUpdate releases only the matching transaction's mutation fence.
func (s *Service) EndUpdate(transactionID string) error {
	if transactionID == "" || len(transactionID) > 128 {
		return &Error{Code: InvalidPath}
	}
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	if s.updateTransaction == "" {
		return nil
	}
	if s.updateTransaction != transactionID {
		return &Error{Code: ResourceLimit}
	}
	s.updateTransaction = ""
	return nil
}

func (s *Service) admitMutation(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	if s.updateTransaction != "" {
		return nil, &Error{Code: ResourceLimit}
	}
	if s.operations == 0 {
		s.operationDone = make(chan struct{})
	}
	s.operations++
	return s.releaseMutation, nil
}

func (s *Service) releaseMutation() {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	s.operations--
	if s.operations == 0 {
		close(s.operationDone)
	}
}

// Policy returns the current limits enforced by this receiver.
func (s *Service) Policy() Policy { return s.config.Policy.Current() }

func (s *Service) Create(ctx context.Context, request CreateRequest) ([]store.FileTransfer, error) {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseMutation()
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

func (s *Service) Append(ctx context.Context, id string, offset int64, body io.Reader) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, nil)
}

// AppendVerified commits at most one independently verified native chunk. The
// existing writer slots bound both buffering and storage work for all callers.
func (s *Service) AppendVerified(ctx context.Context, id string, offset int64, body io.Reader, digest [sha256.Size]byte) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, &digest)
}

func (s *Service) append(ctx context.Context, id string, offset int64, body io.Reader, digest *[sha256.Size]byte) (store.FileTransfer, error) {
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return store.FileTransfer{}, err
	}
	defer releaseMutation()
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
	releaseMutation, err := s.admitMutation(ctx)
	if err != nil {
		return store.FileTransfer{}, err
	}
	defer releaseMutation()
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

func (s *Service) cancel(ctx context.Context, id string) error {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	// Revocation can arrive while an update waits for an admitted writer. Take
	// cleanup ownership before that writer leaves the drain's operation set.
	// This admits no new transfer and keeps the fence until durable cancellation
	// and payload removal finish; calling public Cancel would reject that work.
	s.slotMu.Lock()
	ids := make([]string, 0)
	s.writes.Range(func(key, _ any) bool {
		if id, ok := key.(string); ok {
			ids = append(ids, id)
		}
		return true
	})
	if len(ids) == 0 {
		s.slotMu.Unlock()
		return nil
	}
	if s.operations == 0 {
		s.slotMu.Unlock()
		return &Error{Code: ResourceLimit}
	}
	s.operations++
	s.slotMu.Unlock()
	defer s.releaseMutation()
	var result error
	for _, id := range ids {
		result = errors.Join(result, s.cancel(ctx, id))
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
