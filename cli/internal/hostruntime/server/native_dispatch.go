package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/inspector"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

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
	Fence             func(context.Context, string, uint64) (func(), <-chan struct{}, error)
	Authorize         func(context.Context, streamauth.Header) (string, error)
	ServeStream       func(context.Context, streamauth.Header, net.Conn) error
	Current           NativePrivateTCPCurrent
	DialPrivate       func(context.Context, nativeprivate.Binding) (net.Conn, error)
	SSHTarget         func(context.Context) (managedssh.HostTarget, error)
}
type nativeDispatchEnvelope struct {
	WorkerID string                 `json:"worker_id"`
	Epoch    uint64                 `json:"epoch"`
	Header   json.RawMessage        `json:"header,omitempty"`
	Binding  *nativeprivate.Binding `json:"binding,omitempty"`
}
type nativeDispatchResponse struct {
	AccessSessionID string                 `json:"access_session_id,omitempty"`
	ValidUntil      time.Time              `json:"valid_until,omitempty"`
	SSH             *managedssh.HostTarget `json:"ssh,omitempty"`
}

func nativeDispatchConsumer(consumer string) bool {
	switch consumer {
	case "terminal", "exec", "config_compare", "ssh", "file_transfer", "inspector", "private_tcp", "private_http":
		return true
	}
	return false
}

type nativeDispatchLifetimeKey struct{}
type nativeDispatchRequestLifetime struct{ stopRequest func() bool }
type nativeHTTPAdmissionKey struct{}
type nativeHTTPAdmission func(context.Context) (func(), <-chan struct{}, error)
type NativeDispatchHandler struct {
	handler  http.Handler
	cancel   context.CancelFunc
	lifetime context.Context
	mu       sync.Mutex
	closing  bool
	workers  sync.WaitGroup
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
func NewNativeDispatchHandler(config NativeDispatchConfig) (*NativeDispatchHandler, error) {
	if len(config.Token) < 32 || len(config.Token) > 128 || config.MaximumConcurrent < 1 || config.HandshakeTimeout <= 0 || config.HandshakeTimeout > 30*time.Second || config.Fence == nil || config.Authorize == nil || config.ServeStream == nil {
		return nil, ErrStreamAuthorization
	}
	capacity := make(chan struct{}, config.MaximumConcurrent)
	lifetime, cancel := context.WithCancel(context.Background())
	owner := &NativeDispatchHandler{cancel: cancel, lifetime: lifetime}
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
		if decoder.Decode(&envelope) != nil || decoder.Decode(&extra) != io.EOF || envelope.WorkerID == "" || len(envelope.WorkerID) > 128 || envelope.Epoch == 0 {
			http.Error(w, "invalid dispatch identity", http.StatusBadRequest)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		release, lost, err := config.Fence(r.Context(), envelope.WorkerID, envelope.Epoch)
		if err != nil || release == nil {
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
			header, err = streamauth.Parse(envelope.Header, time.Now().UTC())
			if err != nil || !nativeDispatchConsumer(header.Consumer) {
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
			response.AccessSessionID, err = config.Authorize(r.Context(), header)
			if err != nil || response.AccessSessionID == "" {
				http.Error(w, "dispatch resource denied", http.StatusForbidden)
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
				// exchange carries its own refreshed authorization. Association
				// teardown still closes all of them.
				lifetime, cancel := context.WithCancel(r.Context())
				if header.Consumer != "terminal" && header.Consumer != "exec" && header.Consumer != "file_transfer" {
					cancel()
					lifetime, cancel = context.WithDeadline(r.Context(), time.Unix(header.DeadlineUnix, 0))
				}
				defer cancel()
				if header.Consumer == "file_transfer" {
					// The authenticated association can remain idle. Each HTTP
					// exchange must independently admit this exact worker epoch.
					lifetime = context.WithValue(lifetime, nativeHTTPAdmissionKey{}, nativeHTTPAdmission(func(ctx context.Context) (func(), <-chan struct{}, error) {
						return config.Fence(ctx, envelope.WorkerID, envelope.Epoch)
					}))
				}
				stop := context.AfterFunc(lifetime, func() { _ = connection.Close() })
				defer stop()
				_ = config.ServeStream(lifetime, header, native.LimitConnection(connection, header.MaximumBytes))
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
			verified, verifyErr := nativeprivate.Decode(encoded, time.Now().UTC())
			if operation == "current" {
				verified = binding
				verifyErr = binding.ValidateShape()
			}
			if encodeErr != nil || verifyErr != nil {
				http.Error(w, "invalid dispatch binding", http.StatusBadRequest)
				return
			}
			var revoked <-chan struct{}
			response.ValidUntil, revoked, err = config.Current(r.Context(), verified)
			if err != nil && !errors.Is(err, ErrNativePrivateBinding) && !errors.Is(err, admission.ErrRevoked) {
				http.Error(w, "dispatch authority unavailable", http.StatusServiceUnavailable)
				return
			}
			if err != nil || operation == "private" && !response.ValidUntil.After(time.Now()) {
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
	case http.StatusUnauthorized, http.StatusForbidden:
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
