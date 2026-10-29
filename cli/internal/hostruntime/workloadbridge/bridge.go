// Package workloadbridge is the private typed session/exec boundary. It never
// owns a PTY, journals input or logs request bodies.
package workloadbridge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const MaxBodyBytes = 8 << 20

var ErrFenced = errors.New("terminal owner runtime is fenced")

type wireResponse struct {
	Data  json.RawMessage
	Error *wireError
}

// OwnerIdentity binds authenticated readiness to the registered executable and store.
type OwnerIdentity struct {
	Schema           string `json:"schema"`
	Generation       string `json:"generation"`
	ExecutableSHA256 string `json:"executable_sha256"`
	Retired          bool   `json:"retired,omitempty"`
	RetainedSessions uint64 `json:"retained_sessions"`
}

type Server struct {
	ArchiveMetadata   func(context.Context, string) (ArchiveMetadata, error)
	Endpoint          string
	Token             []byte
	Sessions          session.Service
	SessionOnly       bool
	Identity          *OwnerIdentity
	requests          atomic.Int64
	Retire            func(context.Context) error
	admission         sync.RWMutex
	retired           atomic.Bool
	Executions        *execprocess.RemoteServer
	Fence             func() hostdproto.Status
	AcquireActive     func(string, uint64) (func(), error)
	Ready             chan error
	MaximumConcurrent int
	workerMu          sync.Mutex
	worker            string
	epoch             uint64
	workerDone        chan struct{} // custody cleanup in progress; guarded by workerMu
}

func (s *Server) Run(ctx context.Context) (resultErr error) {
	if len(s.Token) != 32 || s.Sessions == nil || (!s.SessionOnly && s.Executions == nil) || s.Fence == nil || s.MaximumConcurrent < 1 {
		return session.ErrInvalidSession
	}
	listener, err := hostdproto.ListenWorkloads(s.Endpoint)
	if err != nil {
		err = ownerTransportFailure{cause: err}
	}
	if s.Ready != nil {
		s.Ready <- err
	}
	if err != nil {
		return err
	}
	server := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	done := make(chan struct{})
	closed := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		closed <- server.Close()
	}()
	defer func() {
		close(done)
		closeErr := <-closed
		var readersErr error
		if s.Executions != nil {
			readersErr = s.Executions.ResetReaders()
		}
		removeErr := hostdproto.RemoveWorkloadSocket(s.Endpoint)
		// The Unix listener may already unlink its owned socket on close.
		if os.IsNotExist(removeErr) {
			removeErr = nil
		}
		if err := errors.Join(resultErr, closeErr, readersErr, removeErr); err != nil {
			resultErr = ownerTransportFailure{cause: err}
		}
	}()

	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}
func (s *Server) handler() http.Handler {
	semaphore := make(chan struct{}, s.MaximumConcurrent)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		token, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Owner-Capability"))
		if err != nil || subtle.ConstantTimeCompare(token, s.Token) != 1 {
			w.WriteHeader(403)
			return
		}
		epoch, err := strconv.ParseUint(r.Header.Get("X-Paperboat-Worker-Epoch"), 10, 64)
		worker := r.Header.Get("X-Paperboat-Worker-ID")
		fence := s.Fence()
		if err != nil || epoch == 0 || worker == "" || fence.State != hostdproto.StateActive || fence.WorkerID != worker || fence.Epoch != epoch {
			if r.URL.Path == "/owner/admission" {
				w.WriteHeader(http.StatusConflict)
			} else {
				writeResponse(r.Context(), w, nil, ErrFenced)
			}
			return
		}
		// Observations can proceed during another request's custody cleanup.
		if err := s.activateWorker(r.Context(), worker, epoch, !mutatesRequest(r.URL.Path)); err != nil {
			writeResponse(r.Context(), w, nil, err)
			return
		}
		select {
		case semaphore <- struct{}{}:
			s.requests.Add(1)
			defer func() { s.requests.Add(-1); <-semaphore }()
		default:
			if r.URL.Path == "/owner/admission" {
				w.WriteHeader(http.StatusTooManyRequests)
			} else {
				writeResponse(r.Context(), w, nil, session.ErrResourceLimit)
			}
			return
		}
		if r.URL.Path == "/owner/admission" {
			if s.SessionOnly || s.AcquireActive == nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			body, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 32))
			if readErr != nil || string(body) != "{}" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			release, leaseErr := s.AcquireActive(worker, epoch)
			if leaseErr != nil || release == nil {
				w.WriteHeader(http.StatusConflict)
				return
			}
			defer release()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte{0}); err != nil {
				return
			}
			if err := http.NewResponseController(w).Flush(); err != nil {
				return
			}
			<-r.Context().Done()
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		requestTimeout := 25 * time.Second
		if len(parts) == 2 && parts[0] == "exec" && parts[1] == "begin_update" {
			requestTimeout = hostdproto.MaximumUpdateGateDuration
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			if oversized, ok := err.(*http.MaxBytesError); ok && oversized != nil {
				writeResponse(ctx, w, nil, session.ErrInvalidSession)
			} else {
				writeResponse(ctx, w, nil, err)
			}
			return
		}
		var result json.RawMessage
		if len(parts) != 2 {
			writeResponse(ctx, w, nil, session.ErrInvalidSession)
			return
		}
		if parts[0] == "owner" && parts[1] == "retire" {
			s.admission.Lock()
			defer s.admission.Unlock()
		} else if mutates(parts[0], parts[1]) {
			s.admission.RLock()
			defer s.admission.RUnlock()
		}
		if s.retired.Load() && !(parts[0] == "owner" && (parts[1] == "health" || parts[1] == "retire")) {
			writeResponse(ctx, w, nil, session.ErrManagerStopped)
			return
		}
		if mutates(parts[0], parts[1]) {
			if s.AcquireActive == nil {
				writeResponse(ctx, w, nil, ErrFenced)
				return
			}
			release, leaseErr := s.AcquireActive(worker, epoch)
			if leaseErr != nil {
				if leaseErr == hostdproto.ErrFenced {
					leaseErr = ErrFenced
				}
				writeResponse(ctx, w, nil, leaseErr)
				return
			}
			defer release()
		}
		switch parts[0] {
		case "session":
			result, err = session.ServeRemoteCall(ctx, s.Sessions, parts[1], body)
			if s.SessionOnly && err == nil && strings.HasPrefix(parts[1], "Attach") {
				if recorder := diagnostics.FromContext(ctx); recorder != nil {
					_ = recorder.Record("terminal", "owner_attachment_ready", "info", map[string]string{"generation": worker})
				}
			}
		case "exec":
			if s.Executions == nil {
				err = session.ErrOwnerUnavailable
			} else {
				result, err = s.Executions.Call(ctx, parts[1], body)
			}
		case "owner":
			if parts[1] == "archive-metadata" && s.SessionOnly && s.ArchiveMetadata != nil {
				var request struct {
					Generation string `json:"generation"`
				}
				decoder := json.NewDecoder(bytes.NewReader(body))
				decoder.DisallowUnknownFields()
				var extra any
				if decoder.Decode(&request) != nil || decoder.Decode(&extra) != io.EOF {
					err = session.ErrInvalidSession
				} else {
					var metadata ArchiveMetadata
					metadata, err = s.ArchiveMetadata(ctx, request.Generation)
					if err == nil {
						result, err = json.Marshal(metadata)
					}
				}
			} else if parts[1] == "retire" && s.SessionOnly && s.Retire != nil {
				if !s.retired.Load() {
					err = s.Retire(ctx)
					if err == nil {
						s.retired.Store(true)
					}
				}
				result = json.RawMessage(`{}`)
			} else if parts[1] != "health" {
				err = session.ErrInvalidSession
			} else {
				if s.Identity != nil {
					identity := *s.Identity
					identity.Retired = s.retired.Load()
					identity.RetainedSessions = uint64(len(s.Sessions.List()))
					result, err = json.Marshal(identity)
				} else {
					result = json.RawMessage(`{}`)
				}
			}
		default:
			err = session.ErrInvalidSession
		}
		writeResponse(ctx, w, result, err)
	})
}
func mutatesRequest(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 2 && (mutates(parts[0], parts[1]) || path == "/owner/admission")
}

// activateWorker serializes custody changes without holding bridge state over
// detach/reset calls. The initiating request confirms cleanup; observations
// arriving during it remain responsive. Mutations wait for custody with ctx.
func (s *Server) activateWorker(ctx context.Context, worker string, epoch uint64, observe bool) error {
	for {
		fence := s.Fence()
		if fence.State != hostdproto.StateActive || fence.WorkerID != worker || fence.Epoch != epoch {
			return ErrFenced
		}
		s.workerMu.Lock()
		if s.worker == worker && s.epoch == epoch {
			s.workerMu.Unlock()
			return nil
		}
		if done := s.workerDone; done != nil {
			s.workerMu.Unlock()
			if observe {
				return nil
			}
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		done := make(chan struct{})
		s.workerDone = done
		s.workerMu.Unlock()
		var err error
		if owner, ok := s.Sessions.(interface{ DetachAllAttachments() error }); ok {
			err = owner.DetachAllAttachments()
		}
		if err == nil && s.Executions != nil {
			err = s.Executions.ResetReaders()
		}
		s.workerMu.Lock()
		if err == nil {
			s.worker, s.epoch = worker, epoch
		}
		s.workerDone = nil
		close(done)
		s.workerMu.Unlock()
		return err
	}
}

func writeResponse(ctx context.Context, w http.ResponseWriter, data json.RawMessage, err error) {
	var failure *wireError
	if err != nil {
		failure = encodeError(err)
		if failure.Code == "owner_operation_failed" {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "service", "delivery", "service_failed", err)
		}
	}
	body, marshalErr := json.Marshal(wireResponse{Data: data, Error: failure})
	if marshalErr != nil || len(body) > MaxBodyBytes {
		cause := marshalErr
		if cause == nil {
			cause = session.ErrResourceLimit
		}
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "service", "delivery", "service_failed", cause)
		w.WriteHeader(500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(body); err != nil {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "service", "delivery", "service_failed", err)
	}
}

type Client struct {
	endpoint  string
	token     string
	worker    string
	epoch     uint64
	transport *http.Transport
	http      *http.Client
}

func NewClient(endpoint string, token []byte, worker string, epoch uint64) (*Client, error) {
	if endpoint == "" || len(token) != 32 || worker == "" || epoch == 0 {
		return nil, session.ErrInvalidSession
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return hostdproto.DialWorkloads(ctx, endpoint)
	}, MaxIdleConns: 16, MaxIdleConnsPerHost: 16, IdleConnTimeout: 30 * time.Second}
	return &Client{endpoint: endpoint, token: base64.RawURLEncoding.EncodeToString(token), worker: worker, epoch: epoch, transport: transport, http: &http.Client{Transport: transport}}, nil
}
func (c *Client) Close() { c.transport.CloseIdleConnections() }
func (c *Client) Call(ctx context.Context, kind, op string, body json.RawMessage) (json.RawMessage, error) {
	for {
		data, err := c.callOnce(ctx, kind, op, body)
		// Only the pure remote long-poll deadline authorizes another poll.
		if err == context.DeadlineExceeded && ctx.Err() == nil && ((kind == "session" && op == "WaitNext") || (kind == "exec" && (op == "next" || op == "wait" || op == "reader_next"))) {
			continue
		}
		return data, err
	}
}

type ownerTransportFailure struct{ cause error }

func (ownerTransportFailure) Error() string     { return session.ErrOwnerUnavailable.Error() }
func (e ownerTransportFailure) Unwrap() []error { return []error{session.ErrOwnerUnavailable, e.cause} }

func (c *Client) callOnce(ctx context.Context, kind, op string, body json.RawMessage) (data json.RawMessage, resultErr error) {
	callerCtx := ctx
	requestTimeout := 30 * time.Second
	if kind == "exec" && op == "begin_update" {
		requestTimeout = hostdproto.MaximumUpdateGateDuration
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "http://terminal-owner/"+kind+"/"+op, bytes.NewReader(body))
	if err != nil {
		return nil, ownerTransportFailure{cause: err}
	}
	req.Header.Set("X-Paperboat-Owner-Capability", c.token)
	req.Header.Set("X-Paperboat-Worker-ID", c.worker)
	req.Header.Set("X-Paperboat-Worker-Epoch", strconv.FormatUint(c.epoch, 10))
	response, err := c.http.Do(req)
	if err != nil {
		if cancellation := callerCancellation(callerCtx, err); cancellation != nil {
			return nil, cancellation
		}
		return nil, ownerTransportFailure{cause: errors.Join(err, ctx.Err())}
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			data = nil
			resultErr = ownerTransportFailure{cause: errors.Join(resultErr, err)}
		}
	}()
	if response.StatusCode != 200 {
		return nil, ownerHTTPFailure{status: response.StatusCode}
	}
	reader := io.LimitReader(response.Body, MaxBodyBytes+1)
	raw, err := io.ReadAll(reader)
	if err != nil {
		if cancellation := callerCancellation(callerCtx, err); cancellation != nil {
			return nil, cancellation
		}
		return nil, ownerTransportFailure{cause: err}
	}
	if len(raw) > MaxBodyBytes {
		return nil, ownerResponseLimitFailure{}
	}
	var out wireResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, ownerTransportFailure{cause: err}
	}
	if out.Error != nil {
		return nil, decodeError(out.Error)
	}
	return out.Data, nil
}

// callerCancellation projects only a single wrapper chain ending in the
// caller's own cancellation. Joined native failures retain their owner-failure
// classification even when one branch is a deadline.
func callerCancellation(ctx context.Context, err error) error {
	canceled := ctx.Err()
	if canceled == nil {
		return nil
	}
	for range 16 {
		if err == canceled {
			return canceled
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = wrapped.Unwrap()
	}
	return nil
}

func (c *Client) Sessions() (*session.RemoteManager, error) {
	return session.NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return c.Call(ctx, "session", op, body)
	})
}
func (c *Client) Executions() (*execprocess.RemoteManager, error) {
	return execprocess.NewRemoteManager(func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return c.Call(ctx, "exec", op, body)
	})
}
func (c *Client) Health(ctx context.Context) error {
	_, err := c.Call(ctx, "owner", "health", json.RawMessage(`{}`))
	return err
}

func mutates(kind, op string) bool {
	if kind == "exec" {
		switch op {
		case "get", "list", "wait", "next", "reader_next":
			return false
		default:
			return true
		}
	}
	if kind == "session" {
		switch op {
		case "ResourceCounts", "Snapshot", "SnapshotAtGeneration", "List", "AttachmentOwnedBy", "Next", "WaitNext", "InputSequence", "QueryInput", "AttachmentStatus":
			return false
		default:
			return true
		}
	}
	return false
}

// ActiveRequests allows a session-only owner to fence retirement before closing IPC.
func (s *Server) ActiveRequests() int64 { return s.requests.Load() }

func (c *Client) OwnerIdentity(ctx context.Context) (OwnerIdentity, error) {
	body, err := c.Call(ctx, "owner", "health", json.RawMessage(`{}`))
	if err != nil {
		return OwnerIdentity{}, err
	}
	var identity OwnerIdentity
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&identity); err != nil {
		return OwnerIdentity{}, ownerTransportFailure{cause: err}
	}
	var extra any
	_, generationErr := hex.DecodeString(identity.Generation)
	_, digestErr := hex.DecodeString(identity.ExecutableSHA256)
	if decoder.Decode(&extra) != io.EOF || generationErr != nil || digestErr != nil || identity.Schema != "paperboat.session-owner/v1" || len(identity.Generation) != 32 || len(identity.ExecutableSHA256) != 64 {
		return OwnerIdentity{}, session.ErrOwnerUnavailable
	}
	return identity, nil
}

// Retire fences further terminal calls and confirms durable, process-free custody.
func (c *Client) Retire(ctx context.Context) error {
	_, err := c.Call(ctx, "owner", "retire", json.RawMessage(`{}`))
	return err
}

// AcquireAdmission holds the hostd's real epoch lease through an accepted
// feature stream. Closing the response releases admission; loss of native
// custody is signalled independently so forwarding can stop immediately.
func (c *Client) AcquireAdmission(ctx context.Context) (func(), <-chan struct{}, error) {
	if c == nil || ctx == nil {
		return nil, nil, session.ErrInvalidSession
	}
	lifetime, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(lifetime, http.MethodPost, "http://terminal-owner/owner/admission", strings.NewReader("{}"))
	if err != nil {
		cancel()
		return nil, nil, err
	}
	req.Header.Set("X-Paperboat-Owner-Capability", c.token)
	req.Header.Set("X-Paperboat-Worker-ID", c.worker)
	req.Header.Set("X-Paperboat-Worker-Epoch", strconv.FormatUint(c.epoch, 10))
	response, err := c.http.Do(req)
	if err != nil {
		cancel()
		return nil, nil, ownerTransportFailure{cause: err}
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		cancel()
		if response.StatusCode == http.StatusConflict {
			return nil, nil, ErrFenced
		}
		if response.StatusCode == http.StatusTooManyRequests {
			return nil, nil, session.ErrResourceLimit
		}
		return nil, nil, session.ErrOwnerUnavailable
	}
	var ack [1]byte
	if _, err = io.ReadFull(response.Body, ack[:]); err != nil || ack[0] != 0 {
		response.Body.Close()
		cancel()
		return nil, nil, session.ErrOwnerUnavailable
	}
	lost := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { cancel(); _ = response.Body.Close() }) }
	go func() { var unexpected [1]byte; _, _ = response.Body.Read(unexpected[:]); release(); close(lost) }()
	return release, lost, nil
}
