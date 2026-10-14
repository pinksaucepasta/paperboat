package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

// NativeClient sends application records exclusively through an authenticated
// endpoint stream. The opener owns native session admission and reconnection;
// HTTP never dials a public endpoint or falls back to a resource relay.
type NativeClient struct{ *Client }

func NewNativeClient(endpoint string, auth Auth, binding Binding, open func(context.Context) (net.Conn, error)) (*NativeClient, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") || auth.Token == "" || binding.SourceMachineID == "" || binding.DestinationMachineID == "" || binding.InitiatingUserID == "" || binding.SourceMachineID == binding.DestinationMachineID {
		return nil, errors.New("invalid native file transfer configuration")
	}
	transport, err := NewNativeRoundTripper(open)
	if err != nil {
		return nil, err
	}
	client := NewClient(endpoint, auth, binding, &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }})
	client.native = true
	return &NativeClient{Client: client}, nil
}

func (c *Client) uploadNative(ctx context.Context, manifest Manifest, source Source) error {
	state := newNativeRecovery(ctx)
	ctx = context.WithValue(ctx, nativeRecoveryKey{}, state)
	var previous int64 = -1
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return callerContextError(ctx)
		}
		requestCtx, requestCancel := state.requestContext(ctx)
		offset, err := c.Offset(requestCtx, manifest.TransferID)
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
			err = c.patchNative(requestCtx, manifest.TransferID, offset, source)
			if err == nil {
				requestCancel()
				previous = offset
				continue
			}
		}
		requestCancel()
		if !retryableTransferFailure(err) {
			return err
		}
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

func (c *Client) patchNative(ctx context.Context, id string, offset int64, source Source) error {
	if _, err := source.Reader.Seek(offset, io.SeekStart); err != nil {
		return &nativeSourceFailure{err}
	}
	chunk := make([]byte, min(source.Size-offset, int64(protocol.FileTransferChunkBytes)))
	if _, err := io.ReadFull(source.Reader, chunk); err != nil {
		return &nativeSourceFailure{err}
	}
	digest := sha256.Sum256(chunk)
	headers := http.Header{"Upload-Digest": {"sha256=" + hex.EncodeToString(digest[:])}}
	response, err := c.requestWithBody(ctx, http.MethodPatch, c.Endpoint+"/"+id+"/content", operationID("patch", id), contentType, offset, func() (io.Reader, error) { return bytes.NewReader(chunk), nil }, headers)
	return errors.Join(err, closeResponseBody(response))
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
	if c.native && !errors.Is(ctx.Err(), context.Canceled) && retryableTransferFailure(err) {
		return batch, &ResumeRequiredError{BatchID: batch.BatchID, Err: err}
	}
	return batch, errors.Join(err, c.cancelBatch(batch.Transfers))
}

// Thirty minutes of signed stability plus the bounded two-minute drain is the
// longest planned maintenance interruption. The same deadline spans batch RPCs;
// progress and repeated failures never restart it. Credential expiry is separate.
const nativeRecoveryWindow = 32 * time.Minute

type nativeRecoveryKey struct{}
type nativeRecovery struct {
	context  context.Context
	mu       sync.Mutex
	deadline time.Time
}

func newNativeRecovery(ctx context.Context) *nativeRecovery {
	if state, ok := ctx.Value(nativeRecoveryKey{}).(*nativeRecovery); ok {
		return state
	}
	state := &nativeRecovery{}
	state.context = context.WithValue(ctx, nativeRecoveryKey{}, state)
	return state
}
func (s *nativeRecovery) afterFailure(ctx context.Context) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	if s.deadline.IsZero() {
		s.deadline = time.Now().Add(nativeRecoveryWindow)
	}
	deadline := s.deadline
	s.mu.Unlock()
	return context.WithDeadline(ctx, deadline)
}

type nativeSourceFailure struct{ cause error }

func (e *nativeSourceFailure) Error() string { return "file transfer source could not be read" }
func (e *nativeSourceFailure) Unwrap() error { return e.cause }

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
		}
		if remote, ok := err.(*Error); ok && !transientHTTPStatus(remote.StatusCode) {
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
			return transientHTTPStatus(remote.StatusCode)
		}
		switch err {
		case io.EOF, io.ErrUnexpectedEOF, net.ErrClosed, context.DeadlineExceeded, syscall.ECONNRESET, syscall.ECONNABORTED, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH, hostdproto.ErrMaintenanceBusy, errNativeAuthorizationChanged:
			return true
		}
		if timeout, ok := err.(net.Error); ok {
			return timeout.Timeout() || timeout.Temporary()
		}
		return false
	}
	return visit(err)
}

func (s *nativeRecovery) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	s.mu.Lock()
	deadline := s.deadline
	s.mu.Unlock()
	if deadline.IsZero() {
		return ctx, func() {}
	}
	return context.WithDeadline(ctx, deadline)
}
