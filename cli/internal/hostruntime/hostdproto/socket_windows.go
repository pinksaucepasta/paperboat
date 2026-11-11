//go:build windows

package hostdproto

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/winpipe"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"golang.org/x/sys/windows"
)

var (
	ErrSocketConfig = errors.New("invalid hostd worker named-pipe configuration")
	ErrUnauthorized = errors.New("hostd worker named-pipe peer is unauthorized")
)

const (
	installationTokenBytes = 32
	defaultRequestTimeout  = 5 * time.Second
	defaultMaxConcurrent   = 8
	maxConcurrent          = 64
)

// SocketConfig describes the authenticated hostd lifecycle pipe. The
// protected DACL admits only the enrolled SID and SYSTEM; the installation
// capability is still required in-band for every request.
type SocketConfig struct {
	MaintenancePending         bool
	VerifyMaintenanceCommitted func(context.Context) error
	SocketPath                 string
	StatePath                  string
	SID                        string
	Token                      []byte
	APIMin                     uint16
	APIMax                     uint16
	Random                     io.Reader

	RequestTimeout time.Duration
	MaxConcurrent  int
	Workloads      func() WorkloadStatus
	// OpenSSH counts the managed SSH connections the worker bridges itself; the user daemon adds it to the open
	// connections an update asks about.
	OpenSSH       func(context.Context) (int, error)
	WorkerControl WorkerControlHandler
	UpdateGate    UpdateGateHandler
}

type Server struct {
	config     SocketConfig
	controller *Controller
}

func NewServer(config SocketConfig) (*Server, error) {
	if !validPipePath(config.SocketPath) || !filepath.IsAbs(config.StatePath) || len(config.Token) != installationTokenBytes || !validRange(config.APIMin, config.APIMax) || !validSID(config.SID) {
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
	if err := secureStatePathWindows(config.StatePath, config.SID); err != nil {
		return nil, err
	}
	state, err := LoadFenceState(config.StatePath)
	if err != nil {
		return nil, err
	}
	controller, err := NewController(ControllerConfig{
		APIMin: config.APIMin, APIMax: config.APIMax, Random: config.Random,
		MaintenancePending: config.MaintenancePending, VerifyMaintenanceCommitted: config.VerifyMaintenanceCommitted,
		InitialEpoch: state.Epoch,
		PersistActivation: func(status Status) error {
			if status.State != StateActive || status.validate() != nil {
				return ErrInvalidFrame
			}
			body := []byte(fmt.Sprintf(`{"schema":%q,"worker_id":%q,"api_version":%d,"epoch":%d}`+"\n", stateSchemaV1, status.WorkerID, status.APIVersion, status.Epoch))
			if err := atomicfile.Write(config.StatePath, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1,
				SecurityDescriptor: "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + config.SID + ")"}); err != nil {
				return causedFailure{message: "hostd fence state write failed", cause: err}
			}
			return nil
		},
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

func (s *Server) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	listener, err := winio.ListenPipe(s.config.SocketPath, &winio.PipeConfig{
		SecurityDescriptor: pipeSecurityDescriptor(s.config.SID),
		MessageMode:        false,
		InputBufferSize:    MaxFrameBytes + installationTokenBytes + 4,
		OutputBufferSize:   MaxFrameBytes + 4,
	})
	if err != nil {
		return causedFailure{message: "hostd lifecycle pipe could not start", cause: err}
	}
	return s.serve(ctx, listener)
}

func (s *Server) serve(ctx context.Context, listener net.Listener) (result error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	semaphore := make(chan struct{}, s.config.MaxConcurrent)
	var workers sync.WaitGroup
	var connectionsMu sync.Mutex
	connections := make(map[net.Conn]struct{})
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
		if listenerCloseErr != nil && !errors.Is(listenerCloseErr, net.ErrClosed) && !errors.Is(listenerCloseErr, winio.ErrPipeListenerClosed) {
			result = errors.Join(result, causedFailure{message: "hostd lifecycle pipe close failed", cause: errors.Join(ctx.Err(), listenerCloseErr)})
		}
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || errors.Is(err, winio.ErrPipeListenerClosed) {
				return nil
			}
			return causedFailure{message: "hostd lifecycle pipe accept failed", cause: errors.Join(ctx.Err(), err)}
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

func (s *Server) serveOne(ctx context.Context, connection net.Conn) (result error) {
	defer func() {
		if err := connection.Close(); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, winio.ErrPipeListenerClosed) {
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
	// The enrolled SID is enforced by the protected named-pipe DACL. SYSTEM
	// is admitted for the privileged updater. The capability token prevents a
	// second process under either identity from using the endpoint.
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
		status, preparationErr := s.controller.PrepareMaintenanceWithDrain(maintenanceCtx, control.Force, s.config.Workloads, nil)
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

func (s *Server) writeError(ctx context.Context, connection io.Writer, err error) error {
	code := socketErrorResponseCode(err)
	if err := WriteFrame(connection, Error{Code: code}); err != nil {
		return &socketOperationFailure{message: "hostd lifecycle error response could not be written", cause: errors.Join(ctx.Err(), err)}
	}
	return nil
}

type Client struct {
	socketPath string
	token      []byte
	timeout    time.Duration
}

func NewClient(socketPath string, token []byte, timeout time.Duration) (*Client, error) {
	if !validPipePath(socketPath) || len(token) != installationTokenBytes {
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
	connection, err := winpipe.Dial(requestCtx, c.socketPath, "")
	if err != nil {
		return nil, failure("hostd lifecycle pipe connection failed", err)
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
			resultErr = causedFailure{message: "hostd lifecycle pipe cleanup failed", cause: errors.Join(resultErr, closeErr)}
		}
	}()
	deadline, _ := requestCtx.Deadline()
	if err := connection.SetDeadline(deadline); err != nil {
		return nil, failure("hostd lifecycle pipe deadline could not be set", err)
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

// OpenSSH is the number of managed SSH connections the worker bridges now.
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

// expectedSocketRequestShutdownFailure suppresses only a complete error tree
// made of the active request context's cancellation and a native named-pipe
// disconnect caused by server shutdown. An unrelated operational leaf keeps
// the request observable, and an unclassified peer disconnect without a
// canceled request remains an operational write failure.
func expectedSocketRequestShutdownFailure(ctx context.Context, err error) (expected bool) {
	defer func() {
		if recover() != nil {
			expected = false
		}
	}()
	if ctx == nil || err == nil {
		return false
	}
	contextErr := ctx.Err()
	if contextErr != context.Canceled && contextErr != context.DeadlineExceeded {
		return false
	}
	pending := []error{err}
	seen := make(map[error]struct{})
	sawContext, sawPipeDisconnect := false, false
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if !value.Comparable() {
			return false
		}
		switch value.Kind() {
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
			if value.IsNil() {
				return false
			}
		}
		if _, exists := seen[current]; exists {
			if current == contextErr {
				sawContext = true
				leaves++
				continue
			}
			if errno, ok := current.(syscall.Errno); ok && windowsPipeDisconnect(errno) {
				sawPipeDisconnect = true
				leaves++
				continue
			}
			return false
		}
		seen[current] = struct{}{}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 16-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil {
				return false
			}
			pending = append(pending, child)
		default:
			leaves++
			if current == contextErr {
				sawContext = true
				continue
			}
			errno, ok := current.(syscall.Errno)
			if !ok {
				return false
			}
			if windowsPipeDisconnect(errno) {
				sawPipeDisconnect = true
				continue
			}
			return false
		}
	}
	return leaves > 0 && sawContext && sawPipeDisconnect
}

func windowsPipeDisconnect(errno syscall.Errno) bool {
	switch errno {
	case syscall.Errno(109), syscall.Errno(232), syscall.Errno(233):
		// ERROR_BROKEN_PIPE, ERROR_NO_DATA, ERROR_PIPE_NOT_CONNECTED.
		return true
	default:
		return false
	}
}

func (s *Server) String() string {
	return "hostd lifecycle named pipe server"
}

func validPipePath(path string) bool {
	const prefix = `\\.\pipe\`
	if !strings.HasPrefix(strings.ToLower(path), prefix) || len(path) <= len(prefix) || len(path) > 256 {
		return false
	}
	return !strings.ContainsAny(path[len(prefix):], `/\:*?"<>|`)
}

func validSID(value string) bool {
	sid, err := windows.StringToSid(value)
	return err == nil && sid != nil && sid.String() == value
}

func pipeSecurityDescriptor(sid string) string {
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + sid + ")"
}

func secureStatePathWindows(path, enrolledSID string) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil {
		return causedFailure{message: "hostd lifecycle state directory could not be checked", cause: err}
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrSocketConfig
	}
	attributes, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(parent))
	if err != nil {
		return causedFailure{message: "hostd lifecycle state directory security could not be checked", cause: err}
	}
	if attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !protectedWindowsPath(parent, enrolledSID) {
		return ErrSocketConfig
	}
	if info, err := os.Lstat(path); err == nil {
		attributes, attributeErr := windows.GetFileAttributes(windows.StringToUTF16Ptr(path))
		if attributeErr != nil {
			return causedFailure{message: "hostd lifecycle state security could not be checked", cause: attributeErr}
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !protectedWindowsPath(path, enrolledSID) {
			return ErrSocketConfig
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return causedFailure{message: "hostd lifecycle state path could not be checked", cause: err}
	}
	return nil
}

func protectedWindowsPath(path, enrolledSID string) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil || !descriptor.IsValid() {
		return false
	}
	owner, _, err := descriptor.Owner()
	if err != nil || owner == nil || owner.String() != enrolledSID {
		return false
	}
	control, _, err := descriptor.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	// Compare principals as SIDs, not SDDL text: the API spells the built-in Administrator (RID 500) "LA".
	acl, _, err := descriptor.DACL()
	if err != nil || acl == nil {
		return false
	}
	system, systemErr := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	administrators, administratorsErr := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	enrolled, enrolledErr := windows.StringToSid(enrolledSID)
	if systemErr != nil || administratorsErr != nil || enrolledErr != nil {
		return false
	}
	granted := map[*windows.SID]bool{system: false, administrators: false, enrolled: false}
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false // a deny or any other entry type
		}
		const fileAllAccess = 0x1F01FF
		if ace.Mask&fileAllAccess != fileAllAccess && ace.Mask&windows.GENERIC_ALL == 0 {
			return false
		}
		principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		matched := false
		for sid := range granted {
			if principal.Equals(sid) {
				granted[sid], matched = true, true
			}
		}
		if !matched {
			return false
		}
	}
	return granted[system] && granted[administrators] && granted[enrolled]
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
