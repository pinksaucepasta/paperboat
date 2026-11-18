package filetransfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/cli/internal/diskspace"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/cli/internal/transfername"
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
	if config.Platform == "" {
		config.Platform = runtime.GOOS
	}
	if config.EnsurePublishRoot == nil {
		config.EnsurePublishRoot = privateDirectory
	}
	if !filepath.IsAbs(config.Root) || config.LocalMachineID == "" || config.Store == nil {
		return nil, &Error{Code: InvalidPath}
	}
	if err := os.MkdirAll(config.Root, 0o700); err != nil {
		return nil, storageFailure(err)
	}
	if err := os.Chmod(config.Root, 0o700); err != nil {
		return nil, storageFailure(err)
	}
	return &Service{config: config}, nil
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
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
		return nil, &Error{Code: UpdateInProgress}
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
	maxFile, maxBatch := policy.Bounds()
	spool := policy.MaxPendingSpoolBytes
	switch {
	case request.TransferMode == "unlimited":
		maxFile, maxBatch, spool = math.MaxInt64, math.MaxInt64, math.MaxInt64
	case request.TransferMode == "automatic" && request.MaxBytes > 0:
		bound := min(request.MaxBytes, AutomaticTransferLimit)
		maxFile, maxBatch, spool = bound, bound, max(spool, bound)
	}
	// Only the receiver knows which names it can store; a relayed batch is checked for the destination by the sender.
	platform := "other"
	if request.DestinationMachineID == s.config.LocalMachineID {
		platform = s.config.Platform
	}
	for i, file := range request.Files {
		if !validBasename(platform, file.Basename) || file.Directory && request.DestinationMachineID != s.config.LocalMachineID {
			return nil, &Error{Code: InvalidPath}
		}
		if file.Size < 0 || file.Size > maxFile {
			return nil, &Error{Code: InvalidSize}
		}
		if !validDigest(file.SHA256) {
			return nil, &Error{Code: DigestMismatch}
		}
		total += file.Size
		if total > maxBatch {
			return nil, &Error{Code: BatchLimit}
		}
		id, err := s.newID()
		if err != nil {
			return nil, storageFailure(err)
		}
		// Outgoing: the delivery deadline. Incoming: an idle upload lease, renewed by progress and replaced
		// by the retention period when the batch is published, so an abandoned upload frees its slot.
		expiresAt := now.Add(time.Duration(policy.DeliveryTimeoutSeconds) * time.Second)
		transfers[i] = store.FileTransfer{ID: id, BatchID: request.BatchID, SourceMachineID: request.SourceMachineID, DestinationMachineID: request.DestinationMachineID, InitiatingUserID: request.InitiatingUserID, SessionID: request.SessionID, DeliveryClientID: request.DeliveryClientID, Basename: file.Basename, Size: file.Size, SHA256: file.SHA256, State: "created", CreatedAt: now, ExpiresAt: expiresAt}
	}
	if request.DestinationMachineID == s.config.LocalMachineID {
		if err := s.requireRoom(uint64(total)); err != nil {
			return nil, err
		}
		if err := s.requireFilesFit(request.Files); err != nil {
			return nil, err
		}
	}
	if err := s.config.Store.CreateFileTransfersWithinLimits(ctx, transfers, spool, policy.MaxConcurrentTransfers); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &Error{Code: ResourceLimit, Cause: err}
		}
		return nil, storageFailure(err)
	}
	for i, transfer := range transfers {
		s.cancelSignal(transfer.ID)
		spool := []string{s.partialPath(transfer.ID)}
		if request.Files[i].Directory {
			spool = append(spool, s.directoryMarker(transfer.ID))
		}
		for _, path := range spool {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				_ = s.cancelBatch(context.Background(), transfers)
				return nil, storageFailure(err)
			}
			if err := file.Close(); err != nil {
				_ = s.cancelBatch(context.Background(), transfers)
				return nil, storageFailure(err)
			}
		}
	}
	if err := syncDir(s.config.Root); err != nil {
		_ = s.cancelBatch(context.Background(), transfers)
		return nil, storageFailure(err)
	}
	return transfers, nil
}

func (s *Service) Append(ctx context.Context, id string, offset int64, body io.Reader) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, nil, 0)
}

// AppendSegment streams at most protocol.FileTransferSegmentBytes of one transfer from offset: the receiver
// writes sequentially, fsyncs and commits every protocol.FileTransferCheckpointBytes (a cut keeps everything
// before it), and the sender needs no acknowledgement in between. The whole file is verified once, at Complete.
func (s *Service) AppendSegment(ctx context.Context, id string, offset int64, body io.Reader) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, nil, protocol.FileTransferSegmentBytes)
}

// AppendVerified commits at most one independently verified native chunk. The
// existing writer slots bound both buffering and storage work for all callers.
func (s *Service) AppendVerified(ctx context.Context, id string, offset int64, body io.Reader, digest [sha256.Size]byte) (store.FileTransfer, error) {
	return s.append(ctx, id, offset, body, &digest, 0)
}

// append writes body at offset. A limit above zero caps the bytes one call takes. Data is synced and committed
// at checkpoints, at the end, and when the body ends early (everything received so far is kept for the resume).
func (s *Service) append(ctx context.Context, id string, offset int64, body io.Reader, digest *[sha256.Size]byte, limit int64) (store.FileTransfer, error) {
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
	file, err := openSpoolFile(s.partialPath(id), true)
	if err != nil {
		return transfer, storageFailure(err)
	}
	defer file.Close()
	if err := file.Truncate(offset); err != nil {
		return transfer, storageFailure(err)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return transfer, storageFailure(err)
	}
	remaining := transfer.Size - offset
	if limit > 0 {
		remaining = min(remaining, limit)
	}
	canceled := s.cancelSignal(id)
	reader := &contextReader{ctx: ctx, reader: body, canceled: canceled}
	// The lease covers the idle time between commits, so every commit renews it. A commit is a short local write
	// of bytes already synced: a caller that left meanwhile (its context is canceled) must not lose them.
	commitCtx := context.WithoutCancel(ctx)
	commit := func(upTo int64) error {
		if err := file.Sync(); err != nil {
			return storageFailure(err)
		}
		var lease time.Time
		if transfer.DestinationMachineID == s.config.LocalMachineID {
			lease = s.config.Now().Add(time.Duration(s.config.Policy.Current().DeliveryTimeoutSeconds) * time.Second)
		}
		if err := s.config.Store.CommitFileTransferOffset(commitCtx, id, transfer.CommittedOffset, upTo, lease); err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
				return &Error{Code: OffsetConflict, Cause: err}
			}
			return storageFailure(err)
		}
		transfer.CommittedOffset = upTo
		transfer.State = "uploading"
		return nil
	}
	var written int64
	stepBytes, lastCommit := min(checkpointBytes, copyStepBytes), time.Now()
	for written < remaining+1 {
		step := min(stepBytes, remaining+1-written)
		n, err := io.Copy(file, io.LimitReader(reader, step))
		written += n
		if err != nil {
			// Bytes that arrived before the cut stay: the sender's next HEAD reports them. A cancellation or a failing disk keeps nothing, and the first
			// error is the one returned.
			if written > 0 {
				select {
				case <-canceled:
				default:
					_ = commit(offset + min(written, remaining))
				}
			}
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
		if written > remaining {
			_ = file.Truncate(transfer.CommittedOffset)
			return transfer, &Error{Code: InvalidSize}
		}
		if n < step {
			break
		}
		select {
		case <-canceled:
			_ = file.Truncate(transfer.CommittedOffset)
			return transfer, &Error{Code: Canceled}
		default:
		}
		// A checkpoint is due by bytes or by time, so a slow link still renews the lease and yields its slot to an
		// update within a bounded time.
		if offset+written-transfer.CommittedOffset < checkpointBytes && time.Since(lastCommit) < checkpointInterval {
			continue
		}
		if err := commit(offset + written); err != nil {
			return transfer, err
		}
		lastCommit = time.Now()
		if s.updating() {
			return transfer, &Error{Code: UpdateInProgress}
		}
	}
	select {
	case <-canceled:
		_ = file.Truncate(transfer.CommittedOffset)
		return transfer, &Error{Code: Canceled}
	default:
	}
	if written > remaining {
		_ = file.Truncate(transfer.CommittedOffset)
		return transfer, &Error{Code: InvalidSize}
	}
	if err := commit(offset + written); err != nil {
		return transfer, err
	}
	return transfer, nil
}

// updating reports that an update fence is closing admission: a long stream stops at its next checkpoint.
func (s *Service) updating() bool {
	s.slotMu.Lock()
	defer s.slotMu.Unlock()
	return s.updateTransaction != ""
}

// checkpointBytes is how much a streamed upload writes between fsync + offset commits. A var only so tests can shorten it.
var checkpointBytes int64 = protocol.FileTransferCheckpointBytes

// copyStepBytes is how much is copied between checks for a due checkpoint, cancellation or update. A var only so tests can shorten it.
var copyStepBytes int64 = 64 << 10

// checkpointInterval is the longest a streamed upload goes without a commit, however slow the link.
var checkpointInterval = 15 * time.Second

// slotWait bounds how long a write waits for a free transfer slot; a caller whose context never ends (a stalled
// HTTP client) gets ResourceLimit instead of waiting forever.
var slotWait = 30 * time.Second

func (s *Service) acquire(ctx context.Context) error {
	deadline := time.Now().Add(slotWait)
	for {
		s.slotMu.Lock()
		limit := s.config.Policy.Current().MaxConcurrentTransfers
		if s.active < limit {
			s.active++
			s.slotMu.Unlock()
			return nil
		}
		s.slotMu.Unlock()
		if !time.Now().Before(deadline) {
			return &Error{Code: ResourceLimit, Cause: errors.New("no file transfer slot became free")}
		}
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
	for index, transfer := range transfers {
		if transfer.State != "failed" || transfer.DestinationMachineID != s.config.LocalMachineID {
			continue
		}
		// The Inbox copy failed earlier but the spool still holds the upload: publish it again.
		if _, statErr := os.Lstat(s.contentPath(transfer.ID)); statErr == nil {
			if err := s.config.Store.ReopenFileTransferPublication(ctx, transfer.ID, s.config.LocalMachineID); err != nil {
				return requested, storageFailure(err)
			}
			transfers[index].State = "published"
		}
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
	// The verified files stay open until the commit is checked: each renamed path must still be the file whose
	// bytes were hashed, so a swap between the hash and the rename fails the batch instead of publishing other bytes.
	verified := make(map[string]*os.File, len(transfers))
	defer func() {
		for _, file := range verified {
			_ = file.Close()
		}
	}()
	for _, transfer := range transfers {
		if transfer.CommittedOffset != transfer.Size || transfer.State != "created" && transfer.State != "uploading" {
			return requested, &Error{Code: InvalidSize}
		}
		contentPath := s.partialPath(transfer.ID)
		if _, statErr := os.Lstat(contentPath); errors.Is(statErr, os.ErrNotExist) {
			contentPath = s.contentPath(transfer.ID)
		}
		file, openErr := openSpoolFile(contentPath, false)
		if openErr != nil {
			return requested, storageFailure(openErr)
		}
		verified[transfer.ID] = file
		hash := sha256.New()
		written, copyErr := io.Copy(hash, &contextReader{ctx: ctx, reader: file, canceled: s.cancelSignal(transfer.ID)})
		if copyErr != nil {
			return requested, classifyIO(copyErr)
		}
		if written != transfer.Size || hex.EncodeToString(hash.Sum(nil)) != transfer.SHA256 {
			_ = s.cancelBatchLocked(context.Background(), transfers)
			return requested, &Error{Code: DigestMismatch}
		}
		if s.isDirectory(transfer.ID) {
			if folderErr := s.checkFolder(contentPath); folderErr != nil {
				_ = s.cancelBatchLocked(context.Background(), transfers)
				return requested, folderErr
			}
		}
	}
	var renamed []store.FileTransfer
	rollback := func() {
		for index := len(renamed) - 1; index >= 0; index-- {
			//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=batch-commit-rollback
			_ = os.Rename(s.contentPath(renamed[index].ID), s.partialPath(renamed[index].ID))
		}
	}
	for _, transfer := range transfers {
		if _, statErr := os.Lstat(s.contentPath(transfer.ID)); statErr != nil {
			//paperboat:allow-source-policy atomic-replacement owner=file-transfer reason=verified-content-commit
			if renameErr := os.Rename(s.partialPath(transfer.ID), s.contentPath(transfer.ID)); renameErr != nil {
				rollback()
				return requested, storageFailure(renameErr)
			}
			renamed = append(renamed, transfer)
		}
		held, heldErr := verified[transfer.ID].Stat()
		committed, committedErr := os.Lstat(s.contentPath(transfer.ID))
		if heldErr != nil || committedErr != nil || !os.SameFile(held, committed) {
			rollback()
			_ = s.cancelBatchLocked(context.Background(), transfers)
			return requested, &Error{Code: DigestMismatch, Cause: errors.Join(heldErr, committedErr)}
		}
	}
	if err := syncDir(s.config.Root); err != nil {
		return requested, storageFailure(err)
	}
	retainUntil := s.config.Now().Add(time.Duration(s.config.Policy.Current().RetentionSeconds) * time.Second)
	if err := s.config.Store.CompleteFileTransferBatch(ctx, requested.BatchID, s.config.LocalMachineID, retainUntil); err != nil {
		return requested, storageFailure(err)
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
			return storageFailure(err)
		}
	}
	for _, transfer := range transfers {
		s.signalCancel(transfer.ID)
	}
	if err := s.waitForWrites(ctx, transfers); err != nil {
		return errors.Join(result, storageFailure(err))
	}
	for _, transfer := range transfers {
		result = errors.Join(result, s.removeSpoolFiles(transfer))
		s.cancels.Delete(transfer.ID)
	}
	return errors.Join(result, syncDir(s.config.Root))
}
func (s *Service) partialPath(id string) string { return filepath.Join(s.config.Root, id+".part") }
func (s *Service) contentPath(id string) string { return filepath.Join(s.config.Root, id+".content") }
func (s *Service) publishedPath(transfer store.FileTransfer) string {
	return filepath.Join(s.config.Root, transfer.ID+"-"+transfer.Basename)
}

// spoolFiles are the files a transfer owns in the spool: its parts, its published marker and, when the transfer was
// published into the spool itself (no Inbox), the published copy under the name it was pinned to (a collision pins
// 'name (2)', not the reconstructed name).
func (s *Service) spoolFiles(transfer store.FileTransfer) []string {
	files := []string{s.partialPath(transfer.ID), s.contentPath(transfer.ID), s.markerPath(transfer.ID), s.directoryMarker(transfer.ID), s.publishedPath(transfer)}
	if transfer.ReceiptPath != "" && filepath.Dir(transfer.ReceiptPath) == s.config.Root {
		files = append(files, transfer.ReceiptPath)
	}
	return files
}

// removeSpoolFiles removes what a transfer owns in the spool. The published copy of a folder transfer (kept in the spool
// when there is no Inbox) is a tree; everything else is a file, and a directory in a file's place is an error, not removed.
func (s *Service) removeSpoolFiles(transfer store.FileTransfer) error {
	directory := s.isDirectory(transfer.ID)
	var result error
	for _, path := range s.spoolFiles(transfer) {
		remove := os.Remove
		if directory && (path == s.publishedPath(transfer) || path == transfer.ReceiptPath) {
			remove = os.RemoveAll
		}
		if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
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
func validBasename(platform, value string) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= 255 && value != "." && value != ".." &&
		!strings.ContainsAny(value, "/\\\x00") && transfername.Refusal(platform, value) == ""
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

// storageFailure is a failed write: a full disk is its own permanent code, anything else is a
// storage problem the sender may retry.
func storageFailure(err error) *Error {
	if diskFull(err) {
		return &Error{Code: InsufficientStorage, Cause: err}
	}
	return &Error{Code: StorageUnavailable, Cause: err}
}

func classifyIO(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return storageFailure(err)
}

// requireFilesFit refuses a batch holding a file the Inbox's file system cannot store (FAT32 stops at 4 GiB),
// before a byte is sent: otherwise the copy runs to the limit and then fails on every retry.
func (s *Service) requireFilesFit(files []File) error {
	if s.config.PublishRoot == nil {
		return nil
	}
	root, err := s.config.PublishRoot()
	if err != nil {
		return nil
	}
	maxFile := s.config.MaxFile
	if maxFile == nil {
		maxFile = diskspace.MaxFile
	}
	limit, name, err := maxFile(root)
	if err != nil || limit == 0 {
		return nil // an unreadable disk is reported by the write itself
	}
	for _, file := range files {
		if uint64(file.Size) > limit {
			return &Error{Code: FileTooLarge, FileBytes: uint64(file.Size), FileLimit: limit, FileSystem: name}
		}
	}
	return nil
}

// requireRoom refuses a batch the receiving disk cannot hold before a single byte is sent, so the
// sender learns at once instead of failing mid-upload. It checks the spool the bytes land in and the
// Inbox they are published to, which may be on another disk.
func (s *Service) requireRoom(needed uint64) error {
	roots := []string{s.config.Root}
	if s.config.PublishRoot != nil {
		if root, err := s.config.PublishRoot(); err == nil {
			roots = append(roots, root)
		}
	}
	freeSpace := s.config.FreeSpace
	if freeSpace == nil {
		freeSpace = diskspace.Available
	}
	for _, root := range roots {
		free, err := freeSpace(root)
		if err != nil {
			continue // an unreadable disk is reported by the write itself
		}
		if free < needed {
			return &Error{Code: InsufficientStorage, NeededBytes: needed, FreeBytes: free}
		}
	}
	return nil
}
