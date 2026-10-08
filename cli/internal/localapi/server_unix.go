package localapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const (
	maxHeaderBytes = 32 << 10
	maxJSONBytes   = 1 << 20
)

type SnapshotSource interface {
	Snapshot(context.Context) (Snapshot, error)
}

type SnapshotWatcher interface {
	Watch(context.Context, uint64) (Snapshot, error)
}

type CompletionSource interface {
	Completions(context.Context) (CompletionSnapshot, error)
}

type ObservationSink interface {
	PublishObservation(context.Context, Peer, TransportObservation) error
}

type PeerStreamBroker interface {
	OpenPeerStream(context.Context, Peer, PeerStreamRequest) (net.Conn, error)
}

type PeerProbeBroker interface {
	ProbePeer(context.Context, Peer, PeerStreamRequest) (PeerProbeResult, error)
}

type FileTransferBroker interface {
	PrepareFileTransfer(context.Context, Peer, FileTransferRequest) (FileTransferResult, error)
	OpenFileTransferStream(context.Context, Peer, string) (net.Conn, error)
	ReleaseFileTransfer(Peer, string) error
}

type StaleSocketAuthority interface {
	CanRemoveStaleSocket(context.Context, string) bool
}

type ReadAuthorizer func(Peer) bool

type ServerConfig struct {
	SocketPath string
	OwnerUID   int
	OwnerGID   int
	// OwnerSID is the enrolled Windows owner. Windows uses it in the named-pipe
	// DACL and verifies every accepted client process token against it.
	// Unix callers leave it empty and use OwnerUID/OwnerGID.
	OwnerSID             string
	Source               SnapshotSource
	Completions          CompletionSource
	Observations         ObservationSink
	PeerStreams          PeerStreamBroker
	PeerProbes           PeerProbeBroker
	RelayInventory       func(context.Context) (RelayInventory, error)
	FileTransfers        FileTransferBroker
	Authorize            ReadAuthorizer
	AuthorizeDiagnostics ReadAuthorizer
	Diagnostics          DiagnosticService
	Stale                StaleSocketAuthority
	Timeout              time.Duration
	WatchDuration        time.Duration
	MaxWatchEvents       int
}

type Server struct {
	config  ServerConfig
	cleanup func() error
}

type peerContextKey struct{}

func NewServer(config ServerConfig) (*Server, error) {
	if config.Source == nil {
		return nil, ErrInvalidConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 5 * time.Second
	}
	if config.Timeout <= 0 || config.Timeout > time.Minute {
		return nil, ErrInvalidConfig
	}
	if config.WatchDuration == 0 {
		config.WatchDuration = 10 * time.Minute
	}
	if config.MaxWatchEvents == 0 {
		config.MaxWatchEvents = 1024
	}
	if config.WatchDuration <= 0 || config.WatchDuration > time.Hour || config.MaxWatchEvents < 1 || config.MaxWatchEvents > 65_536 {
		return nil, ErrInvalidConfig
	}
	if err := validateServerConfig(config); err != nil {
		return nil, err
	}
	if config.Authorize == nil {
		config.Authorize = defaultReadAuthorizer(config)
	}
	if config.AuthorizeDiagnostics == nil {
		config.AuthorizeDiagnostics = defaultReadAuthorizer(config)
	}
	return &Server{config: config, cleanup: func() error { return nil }}, nil
}

func (s *Server) Run(ctx context.Context) (runErr error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	listener, err := s.listen(ctx)
	if err != nil {
		return localListenerFailure{cause: err}
	}
	defer func() {
		closeErr := listener.Close()
		if errors.Is(closeErr, net.ErrClosed) {
			closeErr = nil
		}
		runErr = errors.Join(runErr, closeErr, s.cleanup())
	}()
	httpServer := &http.Server{
		Handler:           s.handler(),
		ErrorLog:          log.New(localServerLog{ctx: ctx}, "", 0),
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: s.config.Timeout,
		ReadTimeout:       s.config.Timeout,
		WriteTimeout:      0,
		IdleTimeout:       s.config.Timeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ConnContext: func(parent context.Context, connection net.Conn) context.Context {
			peer, err := peerIdentity(connection)
			if err != nil {
				peer = Peer{UID: -1, GID: -1, PID: -1}
			}
			return context.WithValue(parent, peerContextKey{}, peer)
		},
	}
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		// A daemon stop must interrupt in-flight local setup requests immediately;
		// graceful Shutdown can otherwise wait for a peer dial until systemd's
		// stop deadline. Hijacked application streams own their separate bridge
		// lifetime and are closed by their transport lease.
		_ = httpServer.Close()
	}()
	defer func() { cancel(); <-shutdownDone }()
	err = httpServer.Serve(listener)
	if ctx.Err() != nil && errors.Is(err, http.ErrServerClosed) {
		<-shutdownDone
		return ctx.Err()
	}
	return err
}

type localListenerFailure struct{ cause error }

func (localListenerFailure) Error() string           { return "local API listener could not start" }
func (failure localListenerFailure) Unwrap() error   { return failure.cause }
func (localListenerFailure) DiagnosticStage() string { return "listener_bind" }
func (localListenerFailure) DiagnosticCode() string  { return "local_gateway_failed" }

func (s *Server) handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if reference := request.Header.Get(supportref.Header); supportref.Valid(reference) {
			request = request.WithContext(supportref.WithContext(request.Context(), reference))
		}
		defer func() {
			if failure := recover(); failure != nil {
				if failure != http.ErrAbortHandler {
					errorreport.Current().CaptureFailure(request.Context(), "paperboatd", "diagnostic", "process", "process_panic", localHandlerPanic{})
				}
				// net/http aborts this response without logging the panic value,
				// stack, request address, or any user-controlled handler state.
				panic(http.ErrAbortHandler)
			}
		}()
		requestID := request.Header.Get("X-Paperboat-Request-ID")
		if !validRequestID(requestID) {
			requestID = localRequestID()
		}
		writer.Header().Set("X-Paperboat-Request-ID", requestID)
		writer.Header().Set("Cache-Control", "no-store")
		peer, ok := request.Context().Value(peerContextKey{}).(Peer)
		if !ok || !s.config.Authorize(peer) {
			writeError(writer, http.StatusForbidden, requestID, "permission_denied", "local API access denied")
			return
		}
		if request.URL.Path == "/v1/diagnostics" || request.URL.Path == "/v1/diagnostics/bugreport-marker" || request.URL.Path == "/v1/bugreports" {
			if !s.config.AuthorizeDiagnostics(peer) {
				writeError(writer, http.StatusForbidden, requestID, "permission_denied", "diagnostic access denied")
				return
			}
			s.serveDiagnostics(writer, request, requestID)
			return
		}
		if request.URL.Path == "/v1/watch" {
			s.watch(writer, request, requestID)
			return
		}
		if request.URL.Path == "/v1/observations/transport" {
			s.observeTransport(writer, request, requestID, peer)
			return
		}
		if request.URL.Path == "/v1/completions" {
			s.completions(writer, request, requestID)
			return
		}
		if request.URL.Path == "/v1/peer-streams" {
			s.peerStream(writer, request, requestID, peer)
			return
		}
		if request.URL.Path == "/v1/peer-probes" {
			s.peerProbe(writer, request, requestID, peer)
			return
		}
		if request.URL.Path == "/v1/relay-inventory" {
			s.relayInventory(writer, request, requestID)
			return
		}
		if request.URL.Path == "/v1/file-transfers" {
			s.fileTransfer(writer, request, requestID, peer)
			return
		}
		if request.URL.Path == "/v1/file-transfer-streams" {
			s.fileTransferStream(writer, request, requestID, peer)
			return
		}
		if request.URL.Path != "/v1/snapshot" || request.URL.RawQuery != "" {
			writeError(writer, http.StatusNotFound, requestID, "not_found", "local API resource not found")
			return
		}
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "local API method not allowed")
			return
		}
		if request.ContentLength != 0 || request.Header.Get("Content-Type") != "" {
			writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "snapshot request must not contain a body")
			return
		}
		requestCtx, cancel := context.WithTimeout(request.Context(), s.config.Timeout)
		defer cancel()
		snapshot, err := s.config.Source.Snapshot(requestCtx)
		if err == nil {
			err = snapshot.Validate()
		}
		if err != nil {
			errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
			writeError(writer, http.StatusServiceUnavailable, requestID, "snapshot_unavailable", "local snapshot is unavailable")
			return
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil || len(encoded) > maxJSONBytes {
			if err == nil {
				err = ErrInvalidResponse
			}
			errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
			writeError(writer, http.StatusServiceUnavailable, requestID, "snapshot_unavailable", "local snapshot is unavailable")
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
		_, _ = writer.Write(append(encoded, '\n'))
	})
}

func (s *Server) fileTransfer(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/json" || request.ContentLength < 0 || request.ContentLength > maxJSONBytes || s.config.FileTransfers == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "file transfer request is invalid")
		return
	}
	var value FileTransferRequest
	if decodeStrictJSON(io.LimitReader(request.Body, maxJSONBytes+1), &value) != nil || value.Validate(time.Now().UTC()) != nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_file_transfer", "file transfer key request is invalid")
		return
	}
	result, err := s.config.FileTransfers.PrepareFileTransfer(request.Context(), peer, value)
	if err != nil || result.Handle == "" {
		if err == nil {
			err = ErrInvalidResponse
		}
		if result.Handle != "" {
			_ = s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		}
		errorreport.Current().CaptureFailure(request.Context(), "paperboatd", "transfer", "stream_open", "file_transfer_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "file_transfer_unavailable", "file transfer setup failed; retry the transfer")
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		if result.Handle != "" {
			_ = s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		}
		writeError(writer, http.StatusInternalServerError, requestID, "upgrade_unavailable", "local stream upgrade is unavailable")
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		if result.Handle != "" {
			_ = s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		}
		return
	}
	if err := local.SetDeadline(time.Time{}); err != nil {
		_ = local.Close()
		if result.Handle != "" {
			_ = s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		}
		return
	}
	headers := "HTTP/1.1 200 OK\r\nX-Paperboat-Protocol: " + ProtocolV1 + "\r\nX-Paperboat-Transfer-Handle: " + result.Handle + "\r\nConnection: close\r\n"
	if _, err = buffered.WriteString(headers + "\r\n"); err != nil || buffered.Flush() != nil {
		_ = local.Close()
		if result.Handle != "" {
			_ = s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		}
		return
	}
	go func() {
		defer local.Close()
		defer s.config.FileTransfers.ReleaseFileTransfer(peer, result.Handle)
		ctx, cancel := context.WithCancel(context.WithoutCancel(request.Context()))
		defer cancel()
		watchControlHangup(ctx, local, peer, cancel)
	}()
}

func (s *Server) fileTransferStream(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || request.ContentLength != 0 || request.Header.Get("Content-Type") != "" || s.config.FileTransfers == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "file transfer stream request is invalid")
		return
	}
	handle := request.Header.Get("X-Paperboat-Transfer-Handle")
	if !safeValue(handle) {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_file_transfer", "file transfer stream request is invalid")
		return
	}
	stream, err := s.config.FileTransfers.OpenFileTransferStream(request.Context(), peer, handle)
	if err != nil || stream == nil {
		if err == nil {
			err = ErrInvalidResponse
		}
		if stream != nil {
			_ = stream.Close()
		}
		errorreport.Current().CaptureFailure(request.Context(), "paperboatd", "transfer", "stream_open", "file_transfer_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "file_transfer_stream_unavailable", "file transfer stream could not open; refresh the target and retry the transfer")
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		_ = stream.Close()
		writeError(writer, http.StatusInternalServerError, requestID, "upgrade_unavailable", "local stream upgrade is unavailable")
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = stream.Close()
		return
	}
	if err := local.SetDeadline(time.Time{}); err != nil {
		_ = local.Close()
		_ = stream.Close()
		return
	}
	if _, err = buffered.WriteString("HTTP/1.1 200 OK\r\nX-Paperboat-Protocol: " + ProtocolV1 + "\r\nConnection: close\r\n\r\n"); err != nil || buffered.Flush() != nil {
		_ = local.Close()
		_ = stream.Close()
		return
	}
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(request.Context()))
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchPeerHangup(streamCtx, local, peer, cancel)
	}()
	go func() {
		bridgePeerStream(streamCtx, local, stream)
		cancel()
		<-watchDone
	}()
}

func (s *Server) peerProbe(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost || request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/json" || request.ContentLength < 0 || request.ContentLength > maxJSONBytes || s.config.PeerProbes == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "peer probe request is invalid")
		return
	}
	var value PeerStreamRequest
	if err := decodeStrictJSON(io.LimitReader(request.Body, maxJSONBytes+1), &value); err != nil || value.Consumer != "health_probe" || value.Validate(time.Now().UTC()) != nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_peer_probe", "peer probe request is invalid")
		return
	}
	ctx, cancel := context.WithDeadline(request.Context(), value.Deadline)
	defer cancel()
	result, err := s.config.PeerProbes.ProbePeer(ctx, peer, value)
	if err != nil {
		if IsPermissionFailure(err) {
			writeError(writer, http.StatusForbidden, requestID, "permission_denied", "peer probe authority was denied; refresh the target and its authorization")
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			writeError(writer, http.StatusGatewayTimeout, requestID, "deadline_exceeded", "peer probe deadline expired")
			return
		}
		errorreport.Current().CaptureFailure(request.Context(), "paperboatd", "peer_probe", "peer_connect", "native_private_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "peer_probe_unavailable", "peer probe failed; check target connectivity with pb doctor")
		return
	}
	if !validNativeProbePath(result.Path) || result.ConnectionNanoseconds < 0 {
		writeError(writer, http.StatusServiceUnavailable, requestID, "peer_probe_unavailable", "peer probe is unavailable")
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, requestID, "peer_probe_unavailable", "peer probe is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_, _ = writer.Write(append(encoded, '\n'))
}

func (s *Server) peerStream(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "peer stream method not allowed")
		return
	}
	if request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/json" || request.ContentLength < 0 || request.ContentLength > maxJSONBytes || s.config.PeerStreams == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "peer stream request is invalid")
		return
	}
	var value PeerStreamRequest
	if err := decodeStrictJSON(io.LimitReader(request.Body, maxJSONBytes+1), &value); err != nil || value.ValidatePending(time.Now().UTC()) != nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_peer_stream", "peer stream request is invalid")
		return
	}
	setupCtx, cancelSetup := context.WithCancel(request.Context())
	processExit, closeProcessExit := watchProcessExit(peer.PID)
	setupDone := make(chan struct{})
	go func() {
		defer close(setupDone)
		select {
		case <-processExit:
			cancelSetup()
		case <-setupCtx.Done():
		}
	}()
	stream, err := s.config.PeerStreams.OpenPeerStream(setupCtx, peer, value)
	cancelSetup()
	<-setupDone
	closeProcessExit()
	if err != nil || stream == nil {
		if err == nil {
			err = ErrInvalidResponse
		}
		if stream != nil {
			_ = stream.Close()
		}
		errorreport.Current().CaptureFailure(request.Context(), "paperboatd", "peer_stream", "stream_open", "native_private_failed", err)
		message := "peer stream could not open; refresh the target and check connectivity with pb doctor"
		code := "peer_stream_unavailable"
		var coded interface{ LocalAPICode() string }
		if errors.As(err, &coded) && coded.LocalAPICode() == "exec_start_uncertain" {
			code = coded.LocalAPICode()
			message = "remote execution start outcome is uncertain; check the execution status before starting it again"
		}
		writeError(writer, http.StatusServiceUnavailable, requestID, code, message)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		_ = stream.Close()
		writeError(writer, http.StatusInternalServerError, requestID, "upgrade_unavailable", "local stream upgrade is unavailable")
		return
	}
	local, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = stream.Close()
		return
	}
	if err := local.SetDeadline(time.Time{}); err != nil {
		_ = local.Close()
		_ = stream.Close()
		return
	}
	if _, err = buffered.WriteString("HTTP/1.1 200 OK\r\nX-Paperboat-Protocol: " + ProtocolV1 + "\r\nConnection: close\r\n\r\n"); err != nil || buffered.Flush() != nil {
		_ = local.Close()
		_ = stream.Close()
		return
	}
	// Admission already validated the credential deadline. The hijacked stream
	// is canceled by either endpoint closing, not by credential expiry.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(request.Context()))
	go watchPeerHangup(streamCtx, local, peer, cancel)
	go func() { defer cancel(); bridgePeerStream(streamCtx, local, stream) }()
}

type localHandlerPanic struct{}

func (localHandlerPanic) Error() string { return "local API handler panic" }

type localServerLog struct{ ctx context.Context }

func (writer localServerLog) Write(content []byte) (int, error) {
	// net/http diagnostics may contain arbitrary request and panic data.
	// Record a finite fallback classification without inspecting that content.
	errorreport.Current().ObserveFailure(writer.ctx, "paperboatd", "diagnostic", "local_gateway", "local_gateway_failed", errors.New("local HTTP server failure"))
	return len(content), nil
}

func bridgePeerStream(ctx context.Context, local, remote net.Conn) {
	done := make(chan error, 2)
	copyOne := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if closer, ok := destination.(interface{ CloseWrite() error }); ok {
			err = errors.Join(err, closer.CloseWrite())
		}
		done <- err
	}
	go copyOne(remote, local)
	go copyOne(local, remote)
	var failure error
	closed := false
	closeEndpoints := func() {
		if !closed {
			closed = true
			failure = errors.Join(failure, local.Close(), remote.Close())
		}
	}
	canceled := ctx.Done()
	for completed := 0; completed < 2; {
		select {
		case err := <-done:
			completed++
			failure = errors.Join(failure, err)
			if err != nil {
				closeEndpoints()
			}
		case <-canceled:
			failure = errors.Join(failure, ctx.Err())
			canceled = nil
			closeEndpoints()
		}
	}
	closeEndpoints()
	if failure != nil && !normalPeerStreamTermination(failure) {
		errorreport.Current().CaptureFailure(ctx, "paperboatd", "peer_stream", "delivery", "native_private_failed", failure)
	}
}

func (s *Server) relayInventory(writer http.ResponseWriter, request *http.Request, requestID string) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "relay inventory method not allowed")
		return
	}
	if request.URL.RawQuery != "" || request.ContentLength != 0 || request.Header.Get("Content-Type") != "" || s.config.RelayInventory == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "relay inventory request is invalid")
		return
	}
	requestCtx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	inventory, err := s.config.RelayInventory(requestCtx)
	if err == nil {
		err = inventory.Validate()
	}
	if err != nil {
		errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "relay", "local_gateway", "local_gateway_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "relay_inventory_unavailable", "verified relay inventory is unavailable")
		return
	}
	encoded, err := json.Marshal(inventory)
	if err != nil || len(encoded) > maxJSONBytes {
		if err == nil {
			err = ErrInvalidResponse
		}
		errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "relay", "local_gateway", "local_gateway_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "relay_inventory_unavailable", "verified relay inventory is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_, _ = writer.Write(append(encoded, '\n'))
}

func (s *Server) completions(writer http.ResponseWriter, request *http.Request, requestID string) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "completion method not allowed")
		return
	}
	if request.URL.RawQuery != "" || request.ContentLength != 0 || request.Header.Get("Content-Type") != "" || s.config.Completions == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "completion request is invalid")
		return
	}
	requestCtx, cancel := context.WithTimeout(request.Context(), s.config.Timeout)
	defer cancel()
	snapshot, err := s.config.Completions.Completions(requestCtx)
	if err == nil {
		err = snapshot.Validate()
	}
	if err != nil {
		errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "completion_unavailable", "completion inventory is unavailable")
		return
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > maxJSONBytes {
		if err == nil {
			err = ErrInvalidResponse
		}
		errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
		writeError(writer, http.StatusServiceUnavailable, requestID, "completion_unavailable", "completion inventory is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	_, _ = writer.Write(append(encoded, '\n'))
}

func (s *Server) observeTransport(writer http.ResponseWriter, request *http.Request, requestID string, peer Peer) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "transport observation method not allowed")
		return
	}
	if request.URL.RawQuery != "" || request.Header.Get("Content-Type") != "application/json" || request.ContentLength < 0 || request.ContentLength > maxJSONBytes || s.config.Observations == nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "transport observation request is invalid")
		return
	}
	requestCtx, cancel := context.WithTimeout(request.Context(), s.config.Timeout)
	defer cancel()
	var observation TransportObservation
	if err := decodeStrictJSON(io.LimitReader(request.Body, maxJSONBytes+1), &observation); err != nil || observation.Validate() != nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_observation", "transport observation is invalid")
		return
	}
	if err := s.config.Observations.PublishObservation(requestCtx, peer, observation); err != nil {
		if errors.Is(err, ErrStaleObservation) {
			writeError(writer, http.StatusConflict, requestID, "stale_observation", "transport observation is stale")
			return
		}
		if errors.Is(err, ErrObservationLimit) {
			writeError(writer, http.StatusTooManyRequests, requestID, "observation_limit", "transport observation capacity is exhausted")
			return
		}
		writeError(writer, http.StatusServiceUnavailable, requestID, "observation_unavailable", "transport observation could not be committed")
		errorreport.Current().CaptureFailure(requestCtx, "paperboatd", "runtime_observation", "local_gateway", "local_gateway_failed", err)
		return
	}
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	writer.WriteHeader(http.StatusNoContent)
}

func (s *Server) watch(writer http.ResponseWriter, request *http.Request, requestID string) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeError(writer, http.StatusMethodNotAllowed, requestID, "method_not_allowed", "local API method not allowed")
		return
	}
	if request.ContentLength != 0 || request.Header.Get("Content-Type") != "" || request.Header.Get("Accept") != "application/x-ndjson" {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "watch request is invalid")
		return
	}
	values := request.URL.Query()
	afterValues, ok := values["after"]
	if !ok || len(afterValues) != 1 || len(values) != 1 {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "watch cursor is required")
		return
	}
	after, err := strconv.ParseUint(afterValues[0], 10, 64)
	if err != nil {
		writeError(writer, http.StatusBadRequest, requestID, "invalid_request", "watch cursor is invalid")
		return
	}
	watcher, ok := s.config.Source.(SnapshotWatcher)
	if !ok {
		writeError(writer, http.StatusNotImplemented, requestID, "capability_required", "snapshot watch is unavailable")
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, requestID, "stream_unavailable", "snapshot watch stream is unavailable")
		return
	}
	watchCtx, cancel := context.WithTimeout(request.Context(), s.config.WatchDuration)
	defer cancel()
	writer.Header().Set("Content-Type", "application/x-ndjson")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	for count := 0; count < s.config.MaxWatchEvents; count++ {
		snapshot, err := watcher.Watch(watchCtx, after)
		if err != nil {
			if err != context.Canceled && err != context.DeadlineExceeded {
				errorreport.Current().CaptureFailure(watchCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
			}
			return
		}
		if err := snapshot.Validate(); err != nil {
			errorreport.Current().CaptureFailure(watchCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
			return
		}
		if snapshot.Generation <= after {
			errorreport.Current().CaptureFailure(watchCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", ErrInvalidResponse)
			return
		}
		event := StatusEvent{Schema: StatusEventSchemaV1, Snapshot: snapshot}
		encoded, err := json.Marshal(event)
		if err != nil || len(encoded) > maxJSONBytes {
			if err == nil {
				err = ErrInvalidResponse
			}
			errorreport.Current().CaptureFailure(watchCtx, "paperboatd", "status", "local_gateway", "local_gateway_failed", err)
			return
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			errorreport.Current().ObserveFailure(watchCtx, "paperboatd", "status", "delivery", "local_gateway_failed", err)
			return
		}
		flusher.Flush()
		after = snapshot.Generation
	}
}

func writeError(writer http.ResponseWriter, status int, requestID, code, message string) {
	if len(message) > 512 {
		message = message[:512]
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Paperboat-Protocol", ProtocolV1)
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(struct {
		Schema    string `json:"schema"`
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
	}{ProtocolV1, code, message, requestID})
}

func localRequestID() string {
	return "request_" + uuid.NewString()
}

func validRequestID(value string) bool {
	return safeValue(value)
}
