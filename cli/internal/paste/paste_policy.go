package paste

import (
	"context"
	"io"
	"sync"
	"time"

	transfer "github.com/pinksaucepasta/paperboat/internal/filetransfer"
)

type Policy struct {
	mu             sync.RWMutex
	transfer       BatchUploader
	transferLimits transfer.Limits
	sessionID      string
}

type BatchUploader interface {
	SendBatch(context.Context, string, string, []transfer.Source) (transfer.Batch, error)
}

type uploadCompletion struct {
	seq         uint64
	framed      []byte
	err         error
	uploaded    bool
	closeFailed bool
}

type policySnapshot struct {
	transfer       BatchUploader
	transferLimits transfer.Limits
	sessionID      string
}

func NewPolicy(uploader BatchUploader, sessionID string, limits transfer.Limits) *Policy {
	return &Policy{transfer: uploader, sessionID: sessionID, transferLimits: limits}
}

func (p *Policy) Update(uploader BatchUploader, sessionID string, limits transfer.Limits) {
	p.mu.Lock()
	p.transfer, p.sessionID, p.transferLimits = uploader, sessionID, limits
	p.mu.Unlock()
}

func (p *Policy) snapshot() policySnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return policySnapshot{p.transfer, p.transferLimits, p.sessionID}
}

// Option configures an Interceptor.
type Option func(*Interceptor)

// LifecycleEvent is metadata-only so status renderers never receive local
// paths, remote paths, or raw server errors.
type LifecycleEvent string

const (
	FileDetected  LifecycleEvent = "detected"
	FileUploading LifecycleEvent = "uploading"
	FileComplete  LifecycleEvent = "complete"
	FileFailed    LifecycleEvent = "failed"
)

// WithNotifier sets where user-facing (fail-open) messages are written.
func WithNotifier(w io.Writer) Option { return func(i *Interceptor) { i.notify = w } }

// WithFailureReporter assigns upload, replacement, and notice-write failures
// to their final owner. It returns an optional safe support reference for the
// fail-open notice.
func WithFailureReporter(reporter func(error) string) Option {
	return func(i *Interceptor) { i.failureReporter = reporter }
}

// WithLifecycle reports image-paste lifecycle transitions.
func WithLifecycle(callback func(LifecycleEvent)) Option {
	return func(i *Interceptor) { i.lifecycle = callback }
}

// WithPartialFlushDelay sets how long a partial paste start marker is withheld
// before being forwarded as ordinary input. Zero or negative disables the
// flush (partial prefixes wait for the next write indefinitely).
func WithPartialFlushDelay(d time.Duration) Option {
	return func(i *Interceptor) { i.flushDelay = d }
}

// WithWatchDirs restricts temp-image detection to these directories (in
// addition to absolute paths that exist). Empty means "any existing path".
func WithWatchDirs(dirs []string) Option { return func(i *Interceptor) { i.watchDirs = dirs } }

// WithTempFilePatterns restricts recognized terminal temp files by glob.
func WithTempFilePatterns(patterns []string) Option {
	return func(i *Interceptor) { i.tempPatterns = append([]string(nil), patterns...) }
}

// WithMaxQueuedBytes bounds input held behind an in-flight file upload.
func WithMaxQueuedBytes(n int) Option {
	return func(i *Interceptor) {
		if n <= 0 {
			return
		}
		i.queueChunkSize = defaultQueueChunkSize
		if n < i.queueChunkSize {
			i.queueChunkSize = n
		}
		i.queueChunks = (n + i.queueChunkSize - 1) / i.queueChunkSize
	}
}

// WithDirectInput sends ordinary non-paste bytes to the destination inline.
// Only possible bracketed-paste markers enter the asynchronous upload queue.
func WithDirectInput() Option { return func(i *Interceptor) { i.directInput = true } }

func NewWithPolicy(dest io.Writer, policy *Policy, opts ...Option) *Interceptor {
	ctx, cancel := context.WithCancel(context.Background())
	i := &Interceptor{
		ctx:            ctx,
		cancel:         cancel,
		policy:         policy,
		dest:           dest,
		notify:         io.Discard,
		timeout:        DefaultUploadTimeout,
		flushDelay:     DefaultPartialFlushDelay,
		queueChunkSize: defaultQueueChunkSize,
		queueChunks:    defaultQueueChunks,
	}
	for _, o := range opts {
		o(i)
	}
	i.input = make(chan []byte, i.queueChunks)
	i.completed = make(chan uploadCompletion, 16)
	i.done = make(chan struct{})
	i.errCh = make(chan error, 1)
	go i.run()
	return i
}
