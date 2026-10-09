package diagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaximumRecordBytes   = 64 << 10
	DefaultMaximumBytes  = 50 << 20
	DefaultRetention     = 7 * 24 * time.Hour
	defaultSegmentBytes  = 1 << 20
	defaultQueueCapacity = 256
	diskOperationTimeout = 2 * time.Second
)

type DiskConfig struct {
	Directory string
	// OwnerSID selects the Windows user allowed to read and write diagnostic
	// state. It is optional on Windows, where the current user SID is used.
	// A supplied SID must identify the current user. Unix ignores this field.
	OwnerSID string
	// OwnerUID selects the Unix owner. It is ignored on Windows and must be
	// non-negative on Unix.
	OwnerUID      int
	MaximumBytes  int64
	Retention     time.Duration
	SegmentBytes  int64
	QueueCapacity int
	Clock         func() time.Time
}

type DiskStats struct {
	PersistenceAvailable bool
	DroppedRecords       uint64
	DroppedBytes         uint64
	PersistedBytes       uint64
	FailedRecords        uint64
}

type diskRecord struct {
	encoded []byte
	barrier chan struct{}
}

type DiskRing struct {
	config DiskConfig
	owner  diagnosticOwner
	queue  chan diskRecord
	done   chan struct{}

	mu     sync.Mutex
	closed bool
	err    error

	droppedRecords       atomic.Uint64
	droppedBytes         atomic.Uint64
	persistedBytes       atomic.Uint64
	failedRecords        atomic.Uint64
	persistenceAvailable atomic.Bool
	workerContext        context.Context
	cancelWorker         context.CancelFunc
}

func NewDiskRing(config DiskConfig) (*DiskRing, error) {
	if !filepath.IsAbs(config.Directory) || filepath.Clean(config.Directory) != config.Directory {
		return nil, ErrInvalid
	}
	owner, err := resolveDiagnosticOwner(config)
	if err != nil {
		return nil, err
	}
	if config.MaximumBytes == 0 {
		config.MaximumBytes = DefaultMaximumBytes
	}
	if config.Retention == 0 {
		config.Retention = DefaultRetention
	}
	if config.SegmentBytes == 0 {
		config.SegmentBytes = defaultSegmentBytes
	}
	if config.QueueCapacity == 0 {
		config.QueueCapacity = defaultQueueCapacity
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if config.MaximumBytes < MaximumRecordBytes || config.MaximumBytes > DefaultMaximumBytes || config.Retention <= 0 || config.Retention > DefaultRetention || config.SegmentBytes < MaximumRecordBytes || config.SegmentBytes > config.MaximumBytes || config.QueueCapacity < 1 || config.QueueCapacity > 4096 {
		return nil, ErrInvalid
	}
	if err := ensureDiagnosticDirectory(config.Directory, owner); err != nil {
		return nil, err
	}
	ring := &DiskRing{config: config, owner: owner, queue: make(chan diskRecord, config.QueueCapacity), done: make(chan struct{})}
	ring.persistenceAvailable.Store(true)
	ring.workerContext, ring.cancelWorker = context.WithCancel(context.Background())
	if err := ring.recover(); err != nil {
		ring.cancelWorker()
		return nil, err
	}
	go ring.run()
	return ring, nil
}

func (r *DiskRing) Record(event Event) error {
	if r == nil || event.Validate() != nil {
		return ErrInvalid
	}
	encoded, err := json.Marshal(event)
	if err != nil || len(encoded)+1 > MaximumRecordBytes {
		return ErrInvalid
	}
	encoded = append(encoded, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return os.ErrClosed
	}
	select {
	case r.queue <- diskRecord{encoded: encoded}:
		return nil
	default:
		r.droppedRecords.Add(1)
		r.droppedBytes.Add(uint64(len(encoded)))
		return nil
	}
}

func (r *DiskRing) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), diskOperationTimeout)
	defer cancel()
	return r.CloseContext(ctx)
}

func (r *DiskRing) CloseContext(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		return ErrInvalid
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	select {
	case <-r.done:
	case <-ctx.Done():
		r.cancelWorker()
		return ctx.Err()
	}
	r.cancelWorker()
	r.mu.Lock()
	err := r.err
	r.mu.Unlock()
	return err
}

func (r *DiskRing) Flush(ctx context.Context) error {
	if r == nil || ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	barrier := make(chan struct{})
	retry := time.NewTicker(time.Millisecond)
	defer retry.Stop()
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return os.ErrClosed
		}
		select {
		case r.queue <- diskRecord{barrier: barrier}:
			r.mu.Unlock()
			select {
			case <-barrier:
				r.mu.Lock()
				err := r.err
				r.mu.Unlock()
				return err
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			r.mu.Unlock()
		}
		// A saturated queue must not hold the producer/close mutex while disk I/O
		// stalls. Retrying the durability barrier is bounded by the caller deadline.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
		}
	}
}

func (r *DiskRing) Stats() DiskStats {
	if r == nil {
		return DiskStats{}
	}
	return DiskStats{PersistenceAvailable: r.persistenceAvailable.Load(), DroppedRecords: r.droppedRecords.Load(), DroppedBytes: r.droppedBytes.Load(), PersistedBytes: r.persistedBytes.Load(), FailedRecords: r.failedRecords.Load()}
}

func (r *DiskRing) run() {
	defer close(r.done)
	for record := range r.queue {
		if record.barrier != nil {
			close(record.barrier)
			continue
		}
		if err := r.persist(record.encoded); err != nil {
			r.persistenceAvailable.Store(false)
			r.mu.Lock()
			// Retain the latest cause and count every loss. Joining every failed
			// write would retain an unbounded error tree during a disk outage.
			r.err = err
			r.mu.Unlock()
			r.failedRecords.Add(1)
			r.droppedRecords.Add(1)
			r.droppedBytes.Add(uint64(len(record.encoded)))
		} else {
			r.persistenceAvailable.Store(true)
		}
	}
}

func (r *DiskRing) persist(encoded []byte) (result error) {
	ctx, cancel := context.WithTimeout(r.workerContext, diskOperationTimeout)
	defer cancel()
	unlock, err := acquireDiskLock(ctx, filepath.Join(r.config.Directory, "writer.lock"), r.owner)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	segments, total, err := r.segments()
	if err != nil {
		return err
	}
	// Enforce age retention during normal operation, not only at startup.
	cutoff := r.config.Clock().UTC().Add(-r.config.Retention)
	kept := segments[:0]
	for _, segment := range segments {
		if segment.expired(cutoff) {
			if err := removeDiagnosticFile(segment.path, r.owner); err != nil {
				return err
			}
			total -= segment.size
			continue
		}
		kept = append(kept, segment)
	}
	segments = kept
	for total+int64(len(encoded)) > r.config.MaximumBytes && len(segments) > 0 {
		if err := removeDiagnosticFile(segments[0].path, r.owner); err != nil {
			return err
		}
		total -= segments[0].size
		segments = segments[1:]
	}
	if int64(len(encoded)) > r.config.MaximumBytes-total {
		return errors.New("diagnostic ring capacity exhausted")
	}
	path := ""
	if len(segments) > 0 {
		latest := segments[len(segments)-1]
		// A lightly used segment must not retain its oldest records indefinitely
		// merely because new appends keep refreshing its modification time.
		maximumAge := min(r.config.Retention, 24*time.Hour)
		if latest.size+int64(len(encoded)) <= r.config.SegmentBytes && !latest.createdAt.IsZero() && latest.createdAt.After(r.config.Clock().UTC().Add(-maximumAge)) {
			path = latest.path
		}
	}
	if path == "" {
		path, err = r.createSegment()
		if err != nil {
			return err
		}
	}
	file, err := openDiagnosticAppend(path, r.owner)
	if err != nil {
		return err
	}
	before, statErr := file.Stat()
	if statErr != nil {
		return errors.Join(statErr, file.Close())
	}
	written, writeErr := file.Write(encoded)
	if written != len(encoded) && writeErr == nil {
		writeErr = io.ErrShortWrite
	}
	syncErr := file.Sync()
	var rollbackErr error
	if writeErr != nil || syncErr != nil {
		if truncateErr := file.Truncate(before.Size()); truncateErr != nil {
			rollbackErr = truncateErr
		} else {
			rollbackErr = file.Sync()
		}
	}
	closeErr := file.Close()
	if rollbackErr != nil {
		// A failed rollback is rare, but do not leave a partial tail for the next
		// writer if the file can still be repaired under the lock we already hold.
		rollbackErr = errors.Join(rollbackErr, recoverSegment(path, r.owner))
	}
	if writeErr != nil || syncErr != nil || rollbackErr != nil || closeErr != nil {
		return errors.Join(writeErr, syncErr, rollbackErr, closeErr)
	}
	r.persistedBytes.Add(uint64(len(encoded)))
	return nil
}

type segmentInfo struct {
	path      string
	size      int64
	modTime   time.Time
	createdAt time.Time
}

func (s segmentInfo) expired(cutoff time.Time) bool {
	return s.modTime.Before(cutoff) || !s.createdAt.IsZero() && s.createdAt.Before(cutoff)
}

func segmentCreatedAt(name string) time.Time {
	stamp := strings.TrimSuffix(strings.TrimPrefix(name, "events-"), ".ndjson")
	index := strings.LastIndexByte(stamp, '-')
	if index < 0 {
		return time.Time{}
	}
	nanoseconds, err := strconv.ParseInt(stamp[:index], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(0, nanoseconds).UTC()
}

func (r *DiskRing) segments() ([]segmentInfo, int64, error) {
	entries, err := os.ReadDir(r.config.Directory)
	if err != nil {
		return nil, 0, err
	}
	result := make([]segmentInfo, 0, len(entries))
	var total int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "events-") || filepath.Ext(entry.Name()) != ".ndjson" {
			continue
		}
		path := filepath.Join(r.config.Directory, entry.Name())
		info, err := verifiedDiagnosticFile(path, r.owner)
		if err != nil {
			return nil, 0, err
		}
		result = append(result, segmentInfo{path: path, size: info.Size(), modTime: info.ModTime(), createdAt: segmentCreatedAt(entry.Name())})
		total += info.Size()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].path < result[j].path })
	return result, total, nil
}

func (r *DiskRing) createSegment() (string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		name := fmt.Sprintf("events-%020d-%02d.ndjson", r.config.Clock().UTC().UnixNano(), attempt)
		path := filepath.Join(r.config.Directory, name)
		err := createDiagnosticSegment(path, r.owner)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, syncDirectory(r.config.Directory)
	}
	return "", errors.New("diagnostic segment name exhausted")
}

func (r *DiskRing) recover() (result error) {
	ctx, cancel := context.WithTimeout(context.Background(), diskOperationTimeout)
	defer cancel()
	unlock, err := acquireDiskLock(ctx, filepath.Join(r.config.Directory, "writer.lock"), r.owner)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	segments, total, err := r.segments()
	if err != nil {
		return err
	}
	cutoff := r.config.Clock().UTC().Add(-r.config.Retention)
	kept := segments[:0]
	for _, segment := range segments {
		if segment.expired(cutoff) {
			if err := removeDiagnosticFile(segment.path, r.owner); err != nil {
				return err
			}
			total -= segment.size
			continue
		}
		kept = append(kept, segment)
	}
	segments = kept
	// Enforce the disk byte budget before reading corrupt records into memory.
	for total > r.config.MaximumBytes && len(segments) > 0 {
		if err := removeDiagnosticFile(segments[0].path, r.owner); err != nil {
			return err
		}
		total -= segments[0].size
		segments = segments[1:]
	}
	// Writes append validated, newline-terminated records to the newest segment
	// under writer.lock. Older segments are immutable. A crash can leave only an
	// incomplete final record, so inspect the active tail before a full repair.
	if len(segments) > 0 {
		latest := segments[len(segments)-1]
		partial, err := hasPartialTail(latest.path, r.owner)
		if err != nil {
			return err
		}
		if partial {
			if err := recoverSegment(latest.path, r.owner); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *DiskRing) ReadAll(ctx context.Context, maximum int64) (data []byte, result error) {
	if r == nil || ctx == nil || maximum <= 0 || maximum > r.config.MaximumBytes {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, diskOperationTimeout)
	defer cancel()
	unlock, err := acquireDiskLock(ctx, filepath.Join(r.config.Directory, "writer.lock"), r.owner)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	segments, _, err := r.segments()
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	for _, segment := range segments {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if int64(output.Len())+segment.size > maximum {
			return nil, errors.New("diagnostic export exceeds limit")
		}
		file, err := openDiagnosticRead(segment.path, r.owner)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.CopyN(&output, file, segment.size)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, errors.Join(copyErr, closeErr)
		}
	}
	return output.Bytes(), nil
}

func (r *DiskRing) ReadTail(ctx context.Context, maximum int64) (data []byte, result error) {
	if r == nil || ctx == nil || maximum <= 0 || maximum > r.config.MaximumBytes {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, diskOperationTimeout)
	defer cancel()
	unlock, err := acquireDiskLock(ctx, filepath.Join(r.config.Directory, "writer.lock"), r.owner)
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, unlock()) }()
	segments, _, err := r.segments()
	if err != nil {
		return nil, err
	}
	start := len(segments)
	var selected int64
	for start > 0 && selected+segments[start-1].size <= maximum {
		start--
		selected += segments[start].size
	}
	var output bytes.Buffer
	for _, segment := range segments[start:] {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		file, err := openDiagnosticRead(segment.path, r.owner)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.CopyN(&output, file, segment.size)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return nil, errors.Join(copyErr, closeErr)
		}
	}
	return output.Bytes(), nil
}

func recoverSegment(path string, owner diagnosticOwner) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil || !validDiagnosticFile(path, info, owner) {
		_ = file.Close()
		return ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(file, info.Size()+1))
	if err != nil || int64(len(data)) != info.Size() {
		_ = file.Close()
		return errors.Join(ErrInvalid, err)
	}
	validOffset := 0
	for validOffset < len(data) {
		relativeEnd := bytes.IndexByte(data[validOffset:], '\n')
		if relativeEnd < 0 || relativeEnd+1 > MaximumRecordBytes {
			break
		}
		end := validOffset + relativeEnd
		var event Event
		if json.Unmarshal(data[validOffset:end], &event) != nil || event.Validate() != nil {
			break
		}
		validOffset = end + 1
	}
	if int64(validOffset) != info.Size() {
		if err := file.Truncate(int64(validOffset)); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return err
		}
	}
	return file.Close()
}

func hasPartialTail(path string, owner diagnosticOwner) (bool, error) {
	file, err := openDiagnosticRead(path, owner)
	if err != nil {
		return false, err
	}
	info, statErr := file.Stat()
	if statErr != nil {
		return false, errors.Join(statErr, file.Close())
	}
	if info.Size() == 0 {
		return false, file.Close()
	}
	var finalByte [1]byte
	read, readErr := file.ReadAt(finalByte[:], info.Size()-1)
	if read != len(finalByte) && readErr == nil {
		readErr = io.ErrUnexpectedEOF
	}
	if errors.Is(readErr, io.EOF) {
		readErr = io.ErrUnexpectedEOF
	}
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return false, errors.Join(readErr, closeErr)
	}
	return finalByte[0] != '\n', nil
}
