package server

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
)

const NativeDispatchPath = "/v1/native-dispatch/"
const nativeDispatchMaximum = streamauth.MaximumHeaderSize + streamauth.MaximumTarget + 1024

var ErrNativeDispatchUnavailable = errors.New("native worker dispatch is unavailable")
var ErrNativeDispatchBusy = errors.New("native worker dispatch capacity is exhausted")

type NativeDispatchConfig struct {
	CaptureStore      *inspector.Store
	Token             string
	MaximumConcurrent int
	HandshakeTimeout  time.Duration
	// Fence admits one dispatch under the accepting worker's own hostd lease.
	// During a handover both generations accept on the shared listener; each
	// admits only while its own hostd keeps it active.
	Fence     func(context.Context) (func(), <-chan struct{}, error)
	Authorize func(context.Context, streamauth.Header) (string, error)
	// AuthorizeResume admits a credential-free terminal resume the carrier already proved from its current
	// authority; it only gates the feature. Unset, every resume is refused.
	AuthorizeResume func(context.Context, streamauth.Header) (string, error)
	// AuthorizeOffline admits a credential-free terminal or exec session the carrier already admitted from its signed
	// session scopes while Paperboat is unreachable (F21-11); it only gates the feature. Unset, every one is refused.
	AuthorizeOffline func(context.Context, streamauth.Header) (string, error)
	ServeStream      func(context.Context, streamauth.Header, net.Conn) error
	Current          NativePrivateTCPCurrent
	DialPrivate      func(context.Context, nativeprivate.Binding) (net.Conn, error)
	SSHTarget        func(context.Context) (managedssh.HostTarget, error)
}
type nativeDispatchEnvelope struct {
	Header  json.RawMessage            `json:"header,omitempty"`
	Binding *nativeprivate.Binding     `json:"binding,omitempty"`
	Resume  *NativeDispatchResumePeer  `json:"resume,omitempty"`
	Offline *NativeDispatchOfflinePeer `json:"offline,omitempty"`
}

// NativeDispatchOfflinePeer is the peer the carrier admitted from its own signed scopes for an offline session; the
// dispatch token vouches for it.
type NativeDispatchOfflinePeer struct {
	AccountID  string `json:"account_id"`
	EndpointID string `json:"endpoint_id"`
}

// dispatchOfflineConn is a worker-side stream for one offline session of the carrier-admitted peer: the terminal and
// exec servers authorize its requests from OfflinePeer instead of a server-signed credential.
type dispatchOfflineConn struct {
	net.Conn
	peer   NativeDispatchOfflinePeer
	header streamauth.Header
}

func (c *dispatchOfflineConn) OfflinePeer() (accountID, endpointID, consumer, operationID string, deadline time.Time) {
	return c.peer.AccountID, c.peer.EndpointID, c.header.Consumer, c.header.OperationID, time.Unix(c.header.DeadlineUnix, 0)
}

func (c *dispatchOfflineConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

// NativeDispatchResumePeer is the peer the carrier proved for a terminal resume; the dispatch token vouches for it.
type NativeDispatchResumePeer struct {
	AccountID  string `json:"account_id"`
	EndpointID string `json:"endpoint_id"`
	Standing   bool   `json:"standing,omitempty"`
}

// dispatchResumeConn is a worker-side terminal stream that moves one attachment for the carrier-proven peer.
type dispatchResumeConn struct {
	net.Conn
	peer                NativeDispatchResumePeer
	sessionID, resource string
}

func (c *dispatchResumeConn) ResumePeer() (accountID, endpointID, sessionID, resource string, standing bool) {
	return c.peer.AccountID, c.peer.EndpointID, c.sessionID, c.resource, c.peer.Standing
}

func (c *dispatchResumeConn) CloseWrite() error {
	if closer, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return closer.CloseWrite()
	}
	return nil
}

type nativeDispatchResponse struct {
	AccessSessionID string                 `json:"access_session_id,omitempty"`
	ValidUntil      time.Time              `json:"valid_until,omitempty"`
	SSH             *managedssh.HostTarget `json:"ssh,omitempty"`
}

func nativeDispatchConsumer(consumer string) bool {
	switch consumer {
	case "terminal", "exec", "config_compare", "ssh", "file_transfer", "inspector", "private_tcp", "private_http", "recording":
		return true
	}
	return false
}

type nativeDispatchLifetimeKey struct{}
type nativeDispatchRequestLifetime struct{ stopRequest func() bool }
type nativeHTTPAdmissionKey struct{}
type nativeHTTPAdmission func(context.Context) (func(), <-chan struct{}, error)
type nativeTransferDrainKey struct{}
type NativeDispatchHandler struct {
	handler  http.Handler
	cancel   context.CancelFunc
	lifetime context.Context
	mu       sync.Mutex
	closing  bool
	workers  sync.WaitGroup
	// transfers counts open file_transfer streams; draining closes when this generation retires.
	transfers    atomic.Int64
	draining     chan struct{}
	drainingOnce sync.Once
}

func (h *NativeDispatchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		http.Error(w, "dispatch unavailable", http.StatusServiceUnavailable)
		return
	}
	h.workers.Add(1)
	h.mu.Unlock()
	defer h.workers.Done()
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stopRequest := context.AfterFunc(r.Context(), cancel)
	defer stopRequest()
	ctx = context.WithValue(ctx, nativeDispatchLifetimeKey{}, nativeDispatchRequestLifetime{stopRequest: stopRequest})
	defer cancel()
	stop := context.AfterFunc(h.lifetime, cancel)
	defer stop()
	h.handler.ServeHTTP(w, r.WithContext(ctx))
}
func (h *NativeDispatchHandler) Close(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	h.cancel()
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.workers.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DrainTransfers is a retiring generation's graceful stop for file transfers (nginx's QUIT,
// ngx_process_cycle.c ngx_worker_process_cycle; Teleport sshserver.go Shutdown): every accepted transfer stream
// keeps sending its file here, each connection closing once it holds no unfinished transfer, while new streams
// already reach the next generation. It returns when none is open or ctx ends; Close then cuts what remains.
// Revocation still closes a stream at once (httpAdmission binds it to the authorization).
func (h *NativeDispatchHandler) DrainTransfers(ctx context.Context) error {
	h.drainingOnce.Do(func() { close(h.draining) })
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for h.transfers.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
func NewNativeDispatchHandler(config NativeDispatchConfig) (*NativeDispatchHandler, error) {
	if len(config.Token) < 32 || len(config.Token) > 128 || config.MaximumConcurrent < 1 || config.HandshakeTimeout <= 0 || config.HandshakeTimeout > 30*time.Second || config.Fence == nil || config.Authorize == nil || config.ServeStream == nil {
		return nil, ErrStreamAuthorization
	}
	capacity := make(chan struct{}, config.MaximumConcurrent)
	lifetime, cancel := context.WithCancel(context.Background())
	owner := &NativeDispatchHandler{cancel: cancel, lifetime: lifetime, draining: make(chan struct{})}
	owner.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, NativeDispatchPath) {
			http.Error(w, "invalid dispatch operation", http.StatusBadRequest)
			return
		}
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if len(token) != len(config.Token) || subtle.ConstantTimeCompare([]byte(token), []byte(config.Token)) != 1 {
			http.Error(w, "dispatch access denied", http.StatusUnauthorized)
			return
		}
		select {
		case capacity <- struct{}{}:
			defer func() { <-capacity }()
		default:
			http.Error(w, "dispatch capacity exhausted", http.StatusTooManyRequests)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(config.HandshakeTimeout))
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, nativeDispatchMaximum))
		decoder.DisallowUnknownFields()
		var envelope nativeDispatchEnvelope
		var extra any
		if decoder.Decode(&envelope) != nil || decoder.Decode(&extra) != io.EOF {
			http.Error(w, "invalid dispatch identity", http.StatusBadRequest)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		release, lost, err := config.Fence(r.Context())
		if err != nil || release == nil {
			recordNativeAdmissionFailure("worker_fence", err)
			http.Error(w, "dispatch worker fenced", http.StatusConflict)
			return
		}
		var releaseOnce sync.Once
		releaseAdmission := func() { releaseOnce.Do(release) }
		defer releaseAdmission()
		admissionCtx, admissionCancel := context.WithCancel(r.Context())
		defer admissionCancel()
		leaseDone := make(chan struct{})
		leaseWatchDone := make(chan struct{})
		var leaseDoneOnce sync.Once
		stopLeaseWatch := func() { leaseDoneOnce.Do(func() { close(leaseDone) }); <-leaseWatchDone }
		defer stopLeaseWatch()
		go func() {
			defer close(leaseWatchDone)
			select {
			case <-lost:
				admissionCancel()
			case <-leaseDone:
			case <-admissionCtx.Done():
			}
		}()
		r = r.WithContext(admissionCtx)

		var header streamauth.Header
		operation := strings.TrimPrefix(r.URL.Path, NativeDispatchPath)
		needsHeader := operation == "authorize" || operation == "stream"
		if needsHeader {
			if envelope.Binding != nil {
				http.Error(w, "invalid dispatch binding", http.StatusBadRequest)
				return
			}
			header, err = streamauth.Parse(envelope.Header, serverclock.Now().UTC())
			if err != nil || !nativeDispatchConsumer(header.Consumer) {
				if err == nil {
					err = streamauth.ErrInvalid
				}
				recordNativeAdmissionFailure("stream_header", err)
				http.Error(w, "invalid dispatch header", http.StatusBadRequest)
				return
			}
		} else if len(envelope.Header) != 0 {
			http.Error(w, "invalid dispatch header", http.StatusBadRequest)
			return
		}
		response := nativeDispatchResponse{}
		switch operation {
		case "authorize", "stream":
			// A resume header and the carrier-proven peer come together or not at all, and only on a stream.
			if (header.Resume != "") != (envelope.Resume != nil) || (envelope.Resume != nil && (operation != "stream" || envelope.Resume.AccountID == "" || envelope.Resume.EndpointID == "")) {
				recordNativeAdmissionFailure("resume_peer", ErrStreamAuthorization)
				http.Error(w, "invalid dispatch resume", http.StatusBadRequest)
				return
			}
			// An offline header and the carrier-admitted peer likewise come together or not at all, only on a stream, only
			// for the sessions a host may start offline.
			if header.Offline != (envelope.Offline != nil) || (envelope.Offline != nil && (operation != "stream" || envelope.Resume != nil || envelope.Offline.AccountID == "" || envelope.Offline.EndpointID == "" || header.Consumer != "terminal" && header.Consumer != "exec")) {
				recordNativeAdmissionFailure("offline_peer", ErrStreamAuthorization)
				http.Error(w, "invalid dispatch offline session", http.StatusBadRequest)
				return
			}
			authorize := config.Authorize
			if header.Resume != "" {
				authorize = config.AuthorizeResume
			}
			if header.Offline {
				authorize = config.AuthorizeOffline
			}
			if authorize == nil {
				err = ErrStreamAuthorization
			} else {
				response.AccessSessionID, err = authorize(r.Context(), header)
			}
			if err != nil || response.AccessSessionID == "" {
				recordNativeAdmissionFailure("resource_authorize", err)
				if reportAuthorizationFailure(r.Context(), err) {
					http.Error(w, "dispatch authority unavailable", http.StatusServiceUnavailable)
				} else {
					http.Error(w, "dispatch resource denied", http.StatusForbidden)
				}
				return
			}
			if operation == "stream" {
				if header.Consumer == "ssh" || header.Consumer == "private_http" {
					http.Error(w, "invalid dispatch stream", http.StatusBadRequest)
					return
				}
				connection, err := nativeDispatchHijack(r.Context(), w)
				if err != nil {
					return
				}
				defer connection.Close()
				if header.Consumer == "terminal" || header.Consumer == "exec" || header.Consumer == "private_tcp" || header.Consumer == "file_transfer" {
					stopLeaseWatch()
					releaseAdmission()
				}
				// Setup has already been authenticated above. The setup credential
				// deadline bounds admission, not accepted long-running work: the
				// terminal server owns role expiry and revocation, an exec runs its
				// single authorized operation to completion, and every transfer
				// exchange carries its own refreshed authorization, and a recording
				// replay reads its one recording to the end however long that takes.
				// Association teardown still closes all of them.
				lifetime, cancel := context.WithCancel(r.Context())
				if header.Consumer != "terminal" && header.Consumer != "exec" && header.Consumer != "file_transfer" && header.Consumer != "recording" {
					cancel()
					lifetime, cancel = context.WithTimeout(r.Context(), serverclock.Until(time.Unix(header.DeadlineUnix, 0)))
				}
				defer cancel()
				if header.Consumer == "file_transfer" {
					// The authenticated association can remain idle. Each HTTP
					// exchange must independently admit under this worker's lease.
					lifetime = context.WithValue(lifetime, nativeHTTPAdmissionKey{}, nativeHTTPAdmission(config.Fence))
					lifetime = context.WithValue(lifetime, nativeTransferDrainKey{}, (<-chan struct{})(owner.draining))
					owner.transfers.Add(1)
					defer owner.transfers.Add(-1)
				}
				stop := context.AfterFunc(lifetime, func() { _ = connection.Close() })
				defer stop()
				stream := native.LimitConnection(connection, header.MaximumBytes)
				if envelope.Resume != nil {
					stream = &dispatchResumeConn{Conn: stream, peer: *envelope.Resume, sessionID: header.OperationID, resource: header.Resume}
				}
				if envelope.Offline != nil {
					stream = &dispatchOfflineConn{Conn: stream, peer: *envelope.Offline, header: header}
				}
				if err := config.ServeStream(lifetime, header, stream); err != nil {
					recordNativeAdmissionFailure("application", err)
				}
				return
			}
		case "current", "private":
			if envelope.Binding == nil || config.Current == nil {
				http.Error(w, "invalid dispatch binding", http.StatusBadRequest)
				return
			}
			binding := *envelope.Binding
			// The canonical binding alone determines the target; IPC never accepts
			// a caller-selected network, hostname or filesystem path.
			encoded, encodeErr := json.Marshal(binding)
			verified, verifyErr := nativeprivate.Decode(encoded, serverclock.Now().UTC())
			if operation == "current" {
				verified = binding
				verifyErr = binding.ValidateShape()
			}
			if encodeErr != nil || verifyErr != nil {
				recordNativeAdmissionFailure("private_binding", errors.Join(encodeErr, verifyErr))
				http.Error(w, "invalid dispatch binding", http.StatusBadRequest)
				return
			}
			var revoked <-chan struct{}
			response.ValidUntil, revoked, err = config.Current(r.Context(), verified)
			if err != nil && !errors.Is(err, ErrNativePrivateBinding) && !errors.Is(err, admission.ErrRevoked) {
				http.Error(w, "dispatch authority unavailable", http.StatusServiceUnavailable)
				return
			}
			if err != nil || operation == "private" && !response.ValidUntil.After(serverclock.Now()) {
				recordNativeAdmissionFailure("private_access", cmp.Or(err, errNativePrivateAccessExpired))
				http.Error(w, "dispatch binding denied", http.StatusForbidden)
				return
			}
			select {
			case <-revoked:
				http.Error(w, "dispatch binding revoked", http.StatusForbidden)
				return
			default:
			}
			if verified.ExpiresAt.Before(response.ValidUntil) {
				response.ValidUntil = verified.ExpiresAt
			}
			if operation == "private" {
				if config.DialPrivate == nil {
					http.Error(w, "dispatch unavailable", http.StatusServiceUnavailable)
					return
				}
				lease, err := admission.Admit(r.Context(), response.ValidUntil, revoked)
				if err != nil {
					http.Error(w, "dispatch binding denied", http.StatusForbidden)
					return
				}
				defer lease.Close()
				lifetime := lease.Context()
				if config.CaptureStore != nil && verified.Protocol == "http" {
					resourceID := verified.RouteID
					if verified.ResourceKind == "preview" {
						resourceID = verified.ResourceID
					}
					if pending, captureErr := config.CaptureStore.TryBegin(resourceID); captureErr == nil {
						defer func() {
							_, _ = config.CaptureStore.Finish(pending, inspector.CompletedCapture{
								Method: http.MethodConnect, RawURL: (&url.URL{Scheme: verified.TargetScheme, Host: verified.TargetAddress}).String(),
								ResourceGeneration: verified.ResourceGeneration, RouteGeneration: verified.RouteGeneration, TargetGeneration: verified.TargetGeneration,
								RequestUnsupported: true, ResponseUnsupported: true, ErrorCode: "opaque_native_connection",
							})
						}()
					}
				}
				origin, err := config.DialPrivate(lifetime, verified)
				if err != nil {
					http.Error(w, "dispatch origin unavailable", http.StatusServiceUnavailable)
					return
				}
				defer origin.Close()
				connection, err := nativeDispatchHijack(r.Context(), w)
				if err != nil {
					return
				}
				defer connection.Close()
				stopLeaseWatch()
				releaseAdmission()
				stop := context.AfterFunc(lifetime, func() { _ = origin.Close(); _ = connection.Close() })
				defer stop()
				done := make(chan struct{})
				defer close(done)
				go func() {
					select {
					case <-revoked:
						_ = origin.Close()
						_ = connection.Close()
					case <-done:
					case <-lifetime.Done():
					}
				}()
				_ = bridgeNativePrivateTCP(connection, origin)
				return
			}
		case "ssh-target":
			if envelope.Binding != nil || config.SSHTarget == nil {
				http.Error(w, "dispatch unavailable", http.StatusServiceUnavailable)
				return
			}
			target, targetErr := config.SSHTarget(r.Context())
			if targetErr != nil || !nativeDispatchSSHTargetValid(target) {
				http.Error(w, "dispatch SSH unavailable", http.StatusServiceUnavailable)
				return
			}
			response.SSH = &target
		default:
			http.Error(w, "invalid dispatch operation", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	return owner, nil
}
func nativeDispatchSSHTargetValid(target managedssh.HostTarget) bool {
	return target.Generation > 0 && target.Port > 0 && (target.Readiness.Target.Host == "127.0.0.1" || target.Readiness.Target.Host == "::1") && target.Readiness.Target.Port == target.Port
}
func nativeDispatchError(status int) error {
	switch status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return ErrStreamAuthorization
	case http.StatusConflict:
		return hostdproto.ErrFenced
	case http.StatusTooManyRequests:
		return ErrNativeDispatchBusy
	default:
		return ErrNativeDispatchUnavailable
	}
}

// BridgeNativeDispatch preserves byte ordering, backpressure and half-close
// through the private IPC hop without retaining payloads or replaying writes.
func BridgeNativeDispatch(left, right net.Conn) error { return bridgeNativePrivateTCP(left, right) }

// Retain only the verifier's finite code, never its credential or error text.
func recordNativeAdmissionFailure(stage string, err error) {
	reason := "application_failed"
	switch {
	case errors.Is(err, streamauth.ErrExpired), errors.Is(err, nativeprivate.ErrExpired), errors.Is(err, errNativePrivateAccessExpired):
		reason = "deadline_passed"
	case errors.Is(err, streamauth.ErrInvalid):
		reason = "header_invalid"
	case errors.Is(err, nativeprivate.ErrInvalid):
		reason = "binding_invalid"
	}
	if errors.Is(err, hostdproto.ErrMaintenanceBusy) {
		reason = "maintenance_busy"
	}
	if errors.Is(err, hostdproto.ErrFenced) {
		reason = "worker_fenced"
	}
	var credential *auth.Error
	if errors.As(err, &credential) {
		switch credential.Code {
		case auth.Malformed, auth.AlgorithmInvalid, auth.KeyUnknown, auth.SignatureInvalid,
			auth.AudienceInvalid, auth.ScopeInvalid, auth.BindingInvalid, auth.Expired,
			auth.NotYetValid, auth.Revoked, auth.Replayed:
			reason = string(credential.Code)
		}
	}
	diagnosticlog.TryWarn("native worker admission refused", "stage", stage, "reason", reason, "server_clock_offset", serverclock.Offset().String())
}
