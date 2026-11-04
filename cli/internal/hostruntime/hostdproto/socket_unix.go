//go:build darwin || linux

package hostdproto

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

var (
	ErrSocketConfig = errors.New("invalid hostd worker socket configuration")
	ErrUnauthorized = errors.New("hostd worker socket peer is unauthorized")
)

const (
	installationTokenBytes = 32
	defaultRequestTimeout  = 5 * time.Second
	defaultMaxConcurrent   = 8
	maxConcurrent          = 64
)

// SocketConfig defines a local lifecycle socket. The socket is intentionally
// per-user: peer credentials and the installation capability must both match
// before a request frame is parsed.
type SocketConfig struct {
	MaintenancePending         bool
	VerifyMaintenanceCommitted func(context.Context) error
	SocketPath                 string
	StatePath                  string
	UID                        int
	GID                        int
	Token                      []byte
	APIMin                     uint16
	APIMax                     uint16
	Random                     io.Reader

	RequestTimeout time.Duration
	MaxConcurrent  int
	// Workloads is read-only stable-host state. It is evaluated only when a
	// status request is returned to the updater.
	Workloads func() WorkloadStatus
	// OpenSSH counts the managed SSH connections the worker bridges itself; an update asks it
	// before it starts so a manual update can refuse while one is open.
	OpenSSH         func(context.Context) (int, error)
	DrainExecutions func(context.Context) error
	WorkerControl   WorkerControlHandler
	UpdateGate      UpdateGateHandler

	// peerUID is test-only injection for platform credential checks. Production
	// callers always use the OS-specific implementation.
	peerUID func(*net.UnixConn) (int, error)
}

// Server accepts one authenticated lifecycle request per Unix connection.
// It does not log peers, tokens, frames, or worker identities.
type Server struct {
	config     SocketConfig
	controller *Controller
}

func NewServer(config SocketConfig) (*Server, error) {
	if !filepath.IsAbs(config.SocketPath) || !filepath.IsAbs(config.StatePath) || config.UID < 0 || config.GID < 0 ||
		os.Geteuid() != config.UID || len(config.Token) != installationTokenBytes || !validRange(config.APIMin, config.APIMax) {
		return nil, ErrSocketConfig
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = defaultMaxConcurrent
	}
	if config.RequestTimeout < 100*time.Millisecond || config.RequestTimeout > 31*time.Minute || config.MaxConcurrent < 1 || config.MaxConcurrent > maxConcurrent {
		return nil, ErrSocketConfig
	}
	if config.peerUID == nil {
		config.peerUID = peerUID
	}
	if err := secureStatePath(config.StatePath, config.UID); err != nil {
		return nil, err
	}
	state, err := LoadFenceState(config.StatePath)
	if err != nil {
		return nil, err
	}
	controller, err := NewController(ControllerConfig{
		APIMin: config.APIMin, APIMax: config.APIMax, Random: config.Random,
		MaintenancePending: config.MaintenancePending, VerifyMaintenanceCommitted: config.VerifyMaintenanceCommitted,
		InitialEpoch: state.Epoch, PersistActivation: NewFenceStatePersister(config.StatePath, config.UID, config.GID),
	})
	if err != nil {
		return nil, err
	}
	config.Token = append([]byte(nil), config.Token...)
	return &Server{config: config, controller: controller}, nil
}

func (s *Server) Status() Status { return s.controller.Status() }

// CompleteMaintenance is the native owner's bounded startup observer hook.
// It shares the authenticated commit paths' protected source verifier/fence.
func (s *Server) CompleteMaintenance(ctx context.Context) error {
	if s == nil || s.controller == nil {
		return ErrInvalidConfig
	}
	return s.controller.CompleteMaintenance(ctx)
}

// Run owns the socket path for its lifetime. Cancellation closes the listener;
// in-flight requests remain bounded by RequestTimeout.
func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listener, err := s.listen()
	if err != nil {
		return causedFailure{message: "hostd lifecycle socket could not start", cause: err}
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener *net.UnixListener) (result error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	semaphore := make(chan struct{}, s.config.MaxConcurrent)
	var workers sync.WaitGroup
	var connectionsMu sync.Mutex
	connections := make(map[*net.UnixConn]struct{})
	stopping := false
	var listenerClose sync.Once
	var listenerCloseErr error
	closeListener := func() { listenerClose.Do(func() { listenerCloseErr = listener.Close() }) }
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		closeListener()
		connectionsMu.Lock()
		stopping = true
		for connection := range connections {
			_ = connection.Close()
		}
		connectionsMu.Unlock()
		close(stopped)
	})
	defer func() {
		cancel()
		if !stop() {
			<-stopped
		}
		connectionsMu.Lock()
		stopping = true
		for connection := range connections {
			_ = connection.Close()
		}
		connectionsMu.Unlock()
		closeListener()
		workers.Wait()
		if listenerCloseErr != nil && !errors.Is(listenerCloseErr, net.ErrClosed) {
			result = errors.Join(result, causedFailure{message: "hostd lifecycle socket close failed", cause: errors.Join(ctx.Err(), listenerCloseErr)})
		}
	}()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return causedFailure{message: "hostd lifecycle socket accept failed", cause: errors.Join(ctx.Err(), err)}
		}
		select {
		case semaphore <- struct{}{}:
			connectionsMu.Lock()
			if stopping || ctx.Err() != nil {
				connectionsMu.Unlock()
				<-semaphore
				_ = connection.Close()
				return nil
			}
			connections[connection] = struct{}{}
			workers.Add(1)
			connectionsMu.Unlock()
			go func() {
				defer workers.Done()
				defer func() { <-semaphore }()
				defer func() {
					connectionsMu.Lock()
					delete(connections, connection)
					connectionsMu.Unlock()
				}()
				requestCtx, requestCancel := context.WithTimeout(ctx, s.config.RequestTimeout)
				defer requestCancel()
				if supportref.FromContext(requestCtx) == "" {
					requestCtx = supportref.WithContext(requestCtx, supportref.New())
				}
				observeSocketRequestFailure(requestCtx, s.serveOne(requestCtx, connection))
			}()
		default:
			_ = connection.Close()
		}
	}
}

func (s *Server) serveOne(ctx context.Context, connection *net.UnixConn) (result error) {
	defer func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, &socketOperationFailure{message: "hostd lifecycle client connection close failed", cause: errors.Join(ctx.Err(), err)})
		}
	}()
	if ctx == nil {
		ctx = context.Background()
	}
	deadline, _ := ctx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		if expectedSocketRequestFailure(err) {
			return nil
		}
		return &socketOperationFailure{message: "hostd lifecycle request deadline could not be set", cause: errors.Join(ctx.Err(), err)}
	}
	uid, err := s.config.peerUID(connection)
	// The enrolled worker UID may use the lifecycle API. The local root updater
	// may also connect, but it still needs the per-installation capability and
	// cannot create a valid lease for an existing worker. Root access is needed
	// only to query a persisted cutover after power loss.
	if err != nil {
		if expectedSocketRequestFailure(err) {
			return nil
		}
		return &socketOperationFailure{message: "hostd lifecycle client identity could not be checked", cause: errors.Join(ctx.Err(), err)}
	}
	if uid != s.config.UID && uid != 0 {
		return nil
	}
	token := make([]byte, installationTokenBytes)
	if _, err := io.ReadFull(connection, token); err != nil {
		if expectedSocketRequestFailure(err) {
			return nil
		}
		return &socketOperationFailure{message: "hostd lifecycle client capability could not be read", cause: errors.Join(ctx.Err(), err)}
	}
	if subtle.ConstantTimeCompare(token, s.config.Token) != 1 {
		return nil
	}
	request, err := ReadFrame(connection)
	if err != nil {
		return errors.Join(causedFailure{message: "hostd lifecycle request frame could not be read", cause: errors.Join(ctx.Err(), err)}, s.writeError(ctx, connection, err))
	}
	var response Message
	if control, ok := request.(*WorkerControlRequest); ok && control.Operation == "prepare_maintenance" {
		peerCtx, stopWatching := watchPeerDisconnect(ctx, connection)
		maintenanceCtx, cancel := context.WithTimeout(peerCtx, MaximumUpdateGateDuration)
		status, preparationErr := s.controller.PrepareMaintenanceWithDrain(maintenanceCtx, control.Force, s.config.Workloads, s.config.DrainExecutions)
		cancel()
		stopWatching()
		response, err = &WorkerControlResponse{Status: &status, Completed: true}, preparationErr
	} else if control, ok := request.(*WorkerControlRequest); ok && control.Operation == "open_ssh" {
		if s.config.OpenSSH == nil {
			return errors.Join(ErrNotReady, s.writeError(ctx, connection, ErrNotReady))
		}
		openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		open, openErr := s.config.OpenSSH(openCtx)
		cancel()
		response, err = &WorkerControlResponse{Completed: true, OpenSSH: &open}, openErr
	} else if control, ok := request.(*WorkerControlRequest); ok && control.Operation == "abort_maintenance" {
		s.controller.AbortMaintenance()
		response = &WorkerControlResponse{Completed: true}
	} else if control, ok := request.(*WorkerControlRequest); ok && control.Operation == "complete_maintenance" {
		peerCtx, stopWatching := watchPeerDisconnect(ctx, connection)
		response, err = &WorkerControlResponse{Completed: true}, s.controller.CompleteMaintenance(peerCtx)
		stopWatching()
	} else if control, ok := request.(*WorkerControlRequest); ok {
		if s.config.WorkerControl == nil {
			return errors.Join(ErrNotReady, s.writeError(ctx, connection, ErrNotReady))
		}
		controlCtx, cancel := context.WithTimeout(ctx, time.Minute)
		value, controlErr := s.config.WorkerControl.HandleWorkerControl(controlCtx, *control)
		if controlErr == nil && control.Operation == "stop_active" {
			controlErr = s.controller.FinishWorkerStop(controlCtx)
		}
		cancel()
		response, err = &value, controlErr
	} else if gate, ok := request.(*UpdateGateRequest); ok {
		if s.config.UpdateGate == nil {
			return errors.Join(ErrNotReady, s.writeError(ctx, connection, ErrNotReady))
		}
		gateCtx, cancel := context.WithTimeout(ctx, s.config.RequestTimeout)
		var value UpdateGateResponse
		var gateErr error
		if gate.Operation == UpdateGateCommit {
			// Owner commit releases its transfer fence. Native source commit
			// must already be authoritative before forwarding that transition.
			gateErr = s.controller.verifyStartupCommit(gateCtx)
		}
		if gateErr == nil {
			value, gateErr = s.config.UpdateGate.HandleUpdateGate(gateCtx, *gate)
		}
		if gateErr == nil {
			gateErr = value.validate()
		}
		if gateErr == nil && gate.Operation == UpdateGateCommit {
			if gate.ExpectedTarget == nil || value.Target != *gate.ExpectedTarget || value.BlockedReason != "" {
				gateErr = ErrInvalidFrame
			} else {
				gateErr = s.controller.CompleteMaintenance(gateCtx)
			}
		}
		cancel()
		response, err = &value, gateErr
	} else {
		response, err = s.controller.Handle(request)
	}
	if err != nil {
		writeErr := s.writeError(ctx, connection, err)
		if socketHTTPAttemptObserved(err) {
			return writeErr
		}
		return errors.Join(causedFailure{message: "hostd lifecycle request failed", cause: err}, writeErr)
	}
	if _, ok := request.(*Status); ok && s.config.Workloads != nil {
		var status Status
		switch value := response.(type) {
		case Status:
			status = value
		case *Status:
			status = *value
		}
		// Empty status cannot carry workload fields in the v1 wire contract.
		if status.State == StateActive || status.State == StateCandidate {
			workload := s.config.Workloads()
			status.WorkloadGeneration = workload.Generation
			status.ProtectedWorkloads = workload.Protected
			response = status
		}
	}
	if err := WriteFrame(connection, response); err != nil {
		return &socketOperationFailure{message: "hostd lifecycle response could not be written", cause: errors.Join(ctx.Err(), err)}
	}
	return nil
}

func (s *Server) writeError(ctx context.Context, connection *net.UnixConn, err error) error {
	code := socketErrorResponseCode(err)
	if err := WriteFrame(connection, Error{Code: code}); err != nil {
		return &socketOperationFailure{message: "hostd lifecycle error response could not be written", cause: errors.Join(ctx.Err(), err)}
	}
	return nil
}

func (s *Server) listen() (*net.UnixListener, error) {
	directory := filepath.Dir(s.config.SocketPath)
	if err := secureSocketDirectory(directory, s.config.UID); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(s.config.SocketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrSocketConfig
		}
		if err := os.Remove(s.config.SocketPath); err != nil {
			return nil, causedFailure{message: "hostd lifecycle socket could not replace its stale endpoint", cause: err}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, causedFailure{message: "hostd lifecycle socket path could not be inspected", cause: err}
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.config.SocketPath, Net: "unix"})
	if err != nil {
		return nil, causedFailure{message: "hostd lifecycle socket could not bind", cause: err}
	}
	listener.SetUnlinkOnClose(true)
	if err := os.Chown(s.config.SocketPath, s.config.UID, s.config.GID); err != nil {
		return nil, errors.Join(causedFailure{message: "hostd lifecycle socket ownership could not be set", cause: err}, closeUnixListener(listener))
	}
	if err := os.Chmod(s.config.SocketPath, 0o600); err != nil {
		return nil, errors.Join(causedFailure{message: "hostd lifecycle socket permissions could not be set", cause: err}, closeUnixListener(listener))
	}
	return listener, nil
}

func closeUnixListener(listener *net.UnixListener) error {
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return causedFailure{message: "hostd lifecycle socket close failed", cause: err}
	}
	return nil
}

func secureSocketDirectory(path string, ownerUID int) error {
	if !filepath.IsAbs(path) {
		return ErrSocketConfig
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return causedFailure{message: "hostd lifecycle socket directory could not be created", cause: err}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return causedFailure{message: "hostd lifecycle socket directory could not be checked", cause: err}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 || fileOwnerUID(info) != ownerUID {
		return ErrSocketConfig
	}
	return nil
}

func secureStatePath(path string, ownerUID int) error {
	if !filepath.IsAbs(path) {
		return ErrSocketConfig
	}
	if err := secureSocketDirectory(filepath.Dir(path), ownerUID); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return causedFailure{message: "hostd lifecycle state path could not be checked", cause: err}
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || fileOwnerUID(info) != ownerUID {
		return ErrSocketConfig
	}
	return nil
}

// Client is the intentionally small worker-side lifecycle client. It carries
// the installation token only in memory and opens a fresh one-request socket
// for every call, preventing a compromised worker from retaining authority
// after a hostd restart.
type Client struct {
	socketPath string
	token      []byte
	timeout    time.Duration
}

func NewClient(socketPath string, token []byte, timeout time.Duration) (*Client, error) {
	if !filepath.IsAbs(socketPath) || len(token) != installationTokenBytes {
		return nil, ErrSocketConfig
	}
	if timeout == 0 {
		timeout = defaultRequestTimeout
	}
	if timeout < 100*time.Millisecond || timeout > 31*time.Minute {
		return nil, ErrSocketConfig
	}
	return &Client{socketPath: socketPath, token: append([]byte(nil), token...), timeout: timeout}, nil
}

func (c *Client) Request(ctx context.Context, request Message) (response Message, resultErr error) {
	if request == nil || request.validate() != nil {
		return nil, ErrInvalidFrame
	}
	if ctx == nil {
		ctx = context.Background()
	}
	requestTimeout := c.timeout
	if control, ok := request.(*WorkerControlRequest); ok && control.Operation == "prepare_maintenance" {
		requestTimeout = MaximumUpdateGateDuration
	}
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	failure := func(message string, cause error) error {
		return causedFailure{message: message, cause: errors.Join(requestCtx.Err(), cause)}
	}
	connection, err := (&net.Dialer{}).DialContext(requestCtx, "unix", c.socketPath)
	if err != nil {
		return nil, failure("hostd lifecycle socket connection failed", err)
	}
	var closeErr error
	var closeOnce sync.Once
	closeConnection := func() { closeOnce.Do(func() { closeErr = connection.Close() }) }
	stopClose := context.AfterFunc(requestCtx, closeConnection)
	defer func() {
		stopClose()
		closeConnection()
		if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			response = nil
			resultErr = causedFailure{message: "hostd lifecycle socket cleanup failed", cause: errors.Join(resultErr, closeErr)}
		}
	}()
	deadline, _ := requestCtx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, failure("hostd lifecycle socket deadline could not be set", err)
	}
	if err := writeAll(connection, c.token); err != nil {
		return nil, failure("hostd lifecycle request could not be written", err)
	}
	if err := WriteFrame(connection, request); err != nil {
		return nil, failure("hostd lifecycle request could not be framed", err)
	}
	response, err = ReadFrame(connection)
	if err != nil {
		return nil, failure("hostd lifecycle response could not be read", err)
	}
	if remote, ok := response.(*Error); ok {
		return nil, errorForCode(remote.Code)
	}
	return response, nil
}

// Active reads hostd's persisted worker fence through the authenticated local
// socket. It is deliberately read-only, so a root updater can resolve an
// interrupted cutover without receiving any workload-control capability.
func (c *Client) Active(ctx context.Context) (Status, error) {
	response, err := c.Request(ctx, Status{State: StateEmpty})
	if err != nil {
		return Status{}, err
	}
	status, ok := response.(*Status)
	if !ok || status.validate() != nil {
		return Status{}, ErrInvalidFrame
	}
	return *status, nil
}

// OpenSSH is the number of managed SSH connections the worker bridges now. A worker from before
// this operation refuses the request.
func (c *Client) OpenSSH(ctx context.Context) (int, error) {
	response, err := c.Request(ctx, WorkerControlRequest{Operation: "open_ssh"})
	if err != nil {
		return 0, err
	}
	value, ok := response.(*WorkerControlResponse)
	if !ok || value.validate() != nil || value.OpenSSH == nil {
		return 0, ErrInvalidFrame
	}
	return *value.OpenSSH, nil
}

func (c *Client) UpdateGate(ctx context.Context, request UpdateGateRequest) (UpdateGateResponse, error) {
	response, err := c.Request(ctx, request)
	if err != nil {
		return UpdateGateResponse{}, err
	}
	gate, ok := response.(*UpdateGateResponse)
	if !ok || gate.validate() != nil {
		return UpdateGateResponse{}, ErrInvalidFrame
	}
	return *gate, nil
}

func errorForCode(code string) error {
	switch code {
	case "incompatible":
		return ErrIncompatible
	case "fenced":
		return ErrFenced
	case "maintenance_busy":
		return ErrMaintenanceBusy
	case "not_ready":
		return ErrNotReady
	default:
		return ErrInvalidFrame
	}
}

func expectedSocketRequestShutdownFailure(context.Context, error) bool { return false }

func (s *Server) String() string {
	return "hostd lifecycle socket server"
}

func (s *Server) AcquireActive(workerID string, epoch uint64) (func(), error) {
	return s.controller.AcquireActive(workerID, epoch)
}

func (c *Client) StopActive(ctx context.Context) error {
	_, err := ControlWorker(ctx, c, WorkerControlRequest{Operation: "stop_active"})
	return err
}

func (c *Client) PrepareMaintenance(ctx context.Context, force bool) error {
	_, err := ControlWorker(ctx, c, WorkerControlRequest{Operation: "prepare_maintenance", Force: force})
	return err
}
func (c *Client) AbortMaintenance(ctx context.Context) error {
	_, err := ControlWorker(ctx, c, WorkerControlRequest{Operation: "abort_maintenance"})
	return err
}

// CompleteMaintenance verifies durable native commit before reopening the
// candidate's startup mutation fence. It supplies no caller-controlled receipt.
func (c *Client) CompleteMaintenance(ctx context.Context) error {
	_, err := ControlWorker(ctx, c, WorkerControlRequest{Operation: "complete_maintenance"})
	return err
}
