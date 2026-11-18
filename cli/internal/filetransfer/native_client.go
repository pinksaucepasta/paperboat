package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/cli/internal/neterr"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
)

// NativeClient sends application records exclusively through an authenticated
// endpoint stream. The opener owns native session admission and reconnection;
// HTTP never dials a public endpoint or falls back to a resource relay.
type NativeClient struct{ *Client }

// NativeEndpointPath is the only path a native descriptor endpoint carries.
const NativeEndpointPath = "/v1/file-transfers"

func NewNativeClient(endpoint string, auth Auth, binding Binding, open func(context.Context) (net.Conn, error)) (*NativeClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || auth.Token == "" || binding.SourceMachineID == "" || binding.DestinationMachineID == "" || binding.InitiatingUserID == "" || binding.SourceMachineID == binding.DestinationMachineID {
		return nil, errors.New("invalid native file transfer configuration")
	}
	// The stream reaches the receiver directly, so an edge-routing prefix would only be a 404 there.
	if strings.TrimRight(u.Path, "/") != NativeEndpointPath {
		return nil, errors.New("invalid native file transfer configuration: endpoint path must be " + NativeEndpointPath)
	}
	transport, err := NewNativeRoundTripper(open)
	if err != nil {
		return nil, err
	}
	client := NewClient(endpoint, auth, binding, &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	client.native = true
	return &NativeClient{Client: client}, nil
}

func (c *Client) uploadNative(ctx context.Context, manifest Manifest, source Source) error {
	state := newNativeRecovery(ctx)
	ctx = context.WithValue(ctx, nativeRecoveryKey{}, state)
	// A folder streams from a tar-writer goroutine; a terminal outcome releases it (and the in-flight file fd) now
	// instead of holding both until PreparedBatch.Close at batch end.
	defer closeFolderSource(source)
	// A successful PATCH returns the committed offset, so the next chunk follows
	// without a HEAD; the offset is asked again only after a failure.
	var previous, committed int64 = -1, -1
	// attemptFrom is where the last request started; a later HEAD past it means that request committed
	// checkpoints before it failed, which counts as progress for the recovery window and the renewal budget.
	// reported keeps the progress callback from going backwards when a cut stream resumes from the receiver's offset.
	var attemptFrom, reported int64 = -1, 0
	// refusedOffset is the failure of a request the receiver refused for its offset: that is the cut stream's late
	// checkpoint passing ours, so the next HEAD must be past where the request started, or the conflict is real.
	var refusedOffset error
	report := func(offset int64) {
		if c.Committed != nil && offset > reported {
			reported = offset
			c.Committed(manifest.TransferID, offset)
		}
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return callerContextError(ctx)
		}
		requestCtx, requestCancel := state.requestContext(ctx)
		offset, err := committed, error(nil)
		if offset < 0 {
			offset, err = c.Offset(requestCtx, manifest.TransferID)
			if err == nil && attemptFrom >= 0 && offset > attemptFrom {
				state.recovery.Progressed()
				c.renewalProgressed()
			}
			if err == nil && refusedOffset != nil && offset <= attemptFrom {
				requestCancel()
				return refusedOffset
			}
		}
		committed = -1
		if err == nil {
			if offset == source.Size {
				requestCancel()
				return nil
			}
			if offset < 0 || offset > source.Size {
				requestCancel()
				return errors.New("invalid committed transfer offset")
			}
			if offset <= previous {
				requestCancel()
				return errors.New("committed transfer offset did not advance")
			}
			attemptFrom = offset
			committed, err = c.patchNative(requestCtx, manifest.TransferID, offset, source, report)
			if err == nil {
				requestCancel()
				state.recovery.Progressed()
				c.renewalProgressed()
				previous = offset
				report(committed)
				continue
			}
			committed = -1
		}
		requestCancel()
		if !retryableTransferFailure(err) && !receiverBusy(err) {
			return err
		}
		refusedOffset = nil
		if remote := new(*Error); errors.As(err, remote) && (*remote).Code == "offset_conflict" {
			refusedOffset = err
		}
		c.noteWaiting(err)
		retryCtx, cancel := state.afterFailure(ctx)
		wait := c.retryWait
		if wait == nil {
			wait = waitOperationRetry
		}
		waitErr := wait(retryCtx, attempt)
		cancel()
		if waitErr != nil {
			if ctx.Err() != nil {
				return callerContextError(ctx)
			}
			return err
		}
	}
}

// nativeStallTimeout bounds a segment whose transport accepts no more bytes: the stream is cut and the send
// resumes from the receiver's committed offset instead of waiting for a dead peer.
var nativeStallTimeout = 2 * time.Minute

// nativeStallStep is the least progress that restarts the stall timer.
const nativeStallStep = 32 << 10

// nativeSegmentBytes is how much one request streams before the receiver answers. A var only so tests can shorten it.
var nativeSegmentBytes int64 = protocol.FileTransferSegmentBytes

// errNativeStalled is retryable: it wraps context.DeadlineExceeded.
var errNativeStalled = fmt.Errorf("file transfer stream stalled: %w", context.DeadlineExceeded)

// receiverBusy: every transfer slot of the receiver is taken by other uploads, which frees up as they finish.
func receiverBusy(err error) bool {
	var remote *Error
	return errors.As(err, &remote) && remote.Code == "resource_limit" && remote.StatusCode == http.StatusTooManyRequests
}

// patchNative streams the next segment from offset in one request with no acknowledgement inside it (flow control
// belongs to the transport, as in taildrop's PUT and rsync's pipeline) and returns the receiver's committed offset.
// The receiver commits checkpoints while the body is still arriving; report is told the offset as bytes are handed
// to the transport (once per MiB), and the caller reports the confirmed offset when the receiver answers.
func (c *Client) patchNative(ctx context.Context, id string, offset int64, source Source, report func(int64)) (committed int64, resultErr error) {
	length := min(source.Size-offset, nativeSegmentBytes)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// The timer starts with the first byte handed over, so waiting for a stream or a path is not a stall.
	stall := time.AfterFunc(time.Hour, func() { cancel(errNativeStalled) })
	stall.Stop()
	defer stall.Stop()
	var body *segmentReader
	// The transport may still be reading a body after it has answered (an early refusal) and only then closes it, and
	// the source is shared by every attempt: the next attempt, and this call's return, wait for that Close.
	defer func() {
		if body == nil {
			return
		}
		select {
		case <-body.done:
		default:
			cancel(nil) // still being written after the answer: end the request, which makes the transport close the body
		}
		// A body the transport still holds after a minute is reported by the next attempt's own wait, never by
		// overwriting the answer already in hand.
		_ = body.wait()
	}()
	newBody := func() (io.Reader, error) {
		if err := body.wait(); err != nil {
			return nil, &nativeSourceFailure{err}
		}
		if _, err := source.Reader.Seek(offset, io.SeekStart); err != nil {
			return nil, &nativeSourceFailure{err}
		}
		b := &segmentReader{reader: source.Reader, remaining: length, total: length, lastReset: -nativeStallStep, done: make(chan struct{})}
		body = b
		b.onRead = func(sent int64) {
			// Once the last byte is handed over, the receiver's answer is bounded by the response-header timeout. Until
			// then the stall timer restarts only for real movement, so a trickle cannot hold the stream open.
			if sent == length {
				stall.Stop()
			} else if sent-b.lastReset >= nativeStallStep {
				b.lastReset = sent
				stall.Reset(nativeStallTimeout)
			}
			if sent-b.lastReport >= 1<<20 || sent == length {
				b.lastReport = sent
				report(offset + sent)
			}
		}
		return b, nil
	}
	response, err := c.requestWithBody(ctx, http.MethodPatch, c.Endpoint+"/"+id+"/content", operationID("patch", id), contentType, offset, newBody)
	if err != nil {
		if errors.Is(context.Cause(ctx), errNativeStalled) {
			// Whatever the transport reports for its canceled request, the cause is the stall.
			err = errNativeStalled
		}
		return 0, errors.Join(err, closeResponseBody(response))
	}
	committed, parseErr := strconv.ParseInt(response.Header.Get("Upload-Offset"), 10, 64)
	if closeErr := closeResponseBody(response); closeErr != nil {
		return 0, closeErr
	}
	if parseErr != nil {
		return 0, &Error{Code: "http_error", Message: "helper returned invalid Upload-Offset", cause: parseErr}
	}
	return committed, nil
}

// segmentReader hands out exactly remaining bytes of the source. A source that ends early or fails is a source
// failure, never a transport one, so the send does not retry a file that changed under it. onRead gets the bytes
// handed out so far after every read.
type segmentReader struct {
	reader                 io.Reader
	remaining, sent, total int64
	lastReset, lastReport  int64
	onRead                 func(sent int64)
	started                atomic.Bool
	done                   chan struct{} // closed by the transport when it has finished with the body
	once                   sync.Once
}

// Length reports the bytes this body will hand out, so HTTP sends a Content-Length instead of chunked.
func (r *segmentReader) Length() int64 { return r.total }

// Close is the transport's signal that it will not read the body again.
func (r *segmentReader) Close() error {
	r.once.Do(func() { close(r.done) })
	return nil
}

// wait returns once the transport has closed a body it started reading; a body it never read needs no wait.
func (r *segmentReader) wait() error {
	if r == nil || !r.started.Load() {
		return nil
	}
	select {
	case <-r.done:
		return nil
	case <-time.After(time.Minute):
		return errors.New("the previous transfer request still holds the file")
	}
}

func (r *segmentReader) Read(p []byte) (int, error) {
	r.started.Store(true)
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n, err := r.reader.Read(p[:min(int64(len(p)), r.remaining)])
	r.remaining -= int64(n)
	r.sent += int64(n)
	if err == io.EOF && r.remaining > 0 {
		return n, &nativeSourceFailure{io.ErrUnexpectedEOF}
	}
	if err != nil && err != io.EOF {
		return n, &nativeSourceFailure{err}
	}
	if n > 0 {
		r.onRead(r.sent)
	}
	if r.remaining == 0 {
		err = io.EOF
	}
	return n, err
}

// SendBatch also resumes an existing batch. Its ID and source manifests must be
// unchanged after a process restart; the receiver's durable offset is authoritative.
func (c *NativeClient) SendBatch(ctx context.Context, batchID, sessionID string, sources []Source) (Batch, error) {
	if c == nil || c.Client == nil || !c.native || batchID == "" {
		return Batch{}, errors.New("invalid native file transfer")
	}
	state := newNativeRecovery(ctx)
	return c.sendBatchPlaintext(state.context, batchID, sessionID, sources)
}

// ResumeRequiredError distinguishes an interrupted live transfer from explicit
// cancellation. Only the endpoints retain the bounded partial data.
type ResumeRequiredError struct {
	BatchID string
	Err     error
}

func (e *ResumeRequiredError) Error() string {
	return "file transfer interrupted; resume the same batch with the original files"
}
func (e *ResumeRequiredError) Unwrap() error { return e.Err }

func (c *Client) failedBatch(ctx context.Context, batch Batch, err error) (Batch, error) {
	var authorization *nativeAuthorizationFailure
	var denied *Error
	if errors.As(err, &authorization) || (errors.As(err, &denied) && (denied.StatusCode == http.StatusUnauthorized || denied.StatusCode == http.StatusForbidden)) {
		return batch, err
	}
	if c.native && !errors.Is(ctx.Err(), context.Canceled) && (retryableTransferFailure(err) || receiverBusy(err)) {
		return batch, &ResumeRequiredError{BatchID: batch.BatchID, Err: err}
	}
	return batch, errors.Join(err, c.cancelBatch(batch.Transfers))
}

// closeFolderSource releases a folder's tar writer on a terminal send outcome. Regular files need nothing (*os.File
// double-close errors, so only folders). Idempotent: folderStream.Close and PreparedBatch.Close tolerate repeats.
func closeFolderSource(source Source) {
	if folder, ok := source.Reader.(*folderStream); ok {
		_ = folder.Close()
	}
}

// Thirty minutes of signed stability plus the bounded two-minute drain is the
// longest planned maintenance interruption. The same deadline spans batch RPCs;
// repeated failures never restart it, and a committed chunk ends it: a 1 GiB send at
// 0.5 MiB/s (a hosted runner, F14-02) outlives any window started by one update.
// Credential expiry is separate. A var only so tests can shorten it.
var nativeRecoveryWindow = 32 * time.Minute

type nativeRecoveryKey struct{}
type nativeRecovery struct {
	context  context.Context
	recovery *reattach.Recovery
}

func newNativeRecovery(ctx context.Context) *nativeRecovery {
	if state, ok := ctx.Value(nativeRecoveryKey{}).(*nativeRecovery); ok {
		return state
	}
	state := &nativeRecovery{recovery: reattach.New(reattach.Policy{Window: nativeRecoveryWindow})}
	state.context = context.WithValue(ctx, nativeRecoveryKey{}, state)
	return state
}
func (s *nativeRecovery) afterFailure(ctx context.Context) (context.Context, context.CancelFunc) {
	return s.recovery.AfterFailure(ctx)
}

type nativeSourceFailure struct{ cause error }

func (e *nativeSourceFailure) Error() string { return "file transfer source could not be read" }
func (e *nativeSourceFailure) Unwrap() error { return e.cause }

// retryableRemote: a refused offset is the stream cut before this one still committing its last checkpoint; the
// next HEAD reports where the receiver really is.
func retryableRemote(remote *Error) bool {
	return transientHTTPStatus(remote.StatusCode) || remote.Code == "offset_conflict"
}

func retryableTransferFailure(err error) bool {
	count := 0
	seen := make(map[error]bool)
	var visit func(error) bool
	visit = func(err error) bool {
		count++
		if err == nil || count > 64 || !reflect.TypeOf(err).Comparable() || seen[err] {
			return false
		}
		seen[err] = true
		defer delete(seen, err)
		switch err.(type) {
		case *nativeAuthorizationFailure, *nativeSourceFailure:
			return false
		case *localDaemonUnavailable, *transferLeaseLost, *machineUnavailable, *serverUnavailable, *PathWaitingError:
			return true
		}
		if remote, ok := err.(*Error); ok && !retryableRemote(remote) {
			return false
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			if child := wrapped.Unwrap(); child != nil {
				return visit(child)
			}
		}
		if remote, ok := err.(*Error); ok {
			return retryableRemote(remote)
		}
		// Winsock reports resets and refusals under its own numbers, not syscall.ECONNRESET: without neterr a cut
		// connection on Windows ended the send instead of resuming it.
		if neterr.Reset(err) || neterr.Aborted(err) || neterr.Refused(err) {
			return true
		}
		switch err {
		case io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, context.DeadlineExceeded, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH, hostdproto.ErrMaintenanceBusy, errNativeAuthorizationChanged:
			return true
		}
		if timeout, ok := err.(interface {
			Timeout() bool
			Temporary() bool
		}); ok {
			return timeout.Timeout() || timeout.Temporary()
		}
		return false
	}
	return visit(err)
}

func (s *nativeRecovery) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return s.recovery.RequestContext(ctx)
}
