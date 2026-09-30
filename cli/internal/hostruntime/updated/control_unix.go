//go:build darwin || linux

package updated

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/autoupdate"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
	"github.com/pinksaucepasta/paperboat/internal/ospeer"
)

var ErrRecoveryState = errors.New("update recovery state cannot be read; update completion is unknown; retry status after recovery")

type controlServer struct {
	socketPath    string
	uid           int
	gid           int
	invoke        func(context.Context, string) (ControlResponse, error) // legacy test seam
	invokeRequest func(context.Context, ControlRequest) (ControlResponse, error)
	afterResponse func(ControlRequest, ControlResponse)
}

func (s *controlServer) listen() (*net.UnixListener, error) {
	if !filepath.IsAbs(s.socketPath) || !validUnixWorkerIdentity(s.uid, s.gid) || s.invoke == nil && s.invokeRequest == nil {
		return nil, ErrInvalidConfig
	}
	directory := filepath.Dir(s.socketPath)
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o755 {
		return nil, ErrInvalidConfig
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || owner.Uid != 0 {
		return nil, ErrInvalidConfig
	}
	if info, err := os.Lstat(s.socketPath); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrInvalidConfig
		}
		if err := os.Remove(s.socketPath); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: s.socketPath, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err := os.Chown(s.socketPath, s.uid, s.gid); err != nil {
		_ = listener.Close()
		return nil, err
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

// serve deliberately treats malformed or unauthorized requests as local
// caller failures. They cannot take down the mandatory scheduler.
func (s *controlServer) serve(ctx context.Context, listener *net.UnixListener) {
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			return
		}
		go func(connection *net.UnixConn) {
			defer connection.Close()
			_ = s.handle(connection)
		}(connection)
	}
}

func (s *controlServer) handle(connection *net.UnixConn) error {
	identity, err := ospeer.Get(connection)
	if err != nil || identity.UID != s.uid {
		return ErrControlDenied
	}
	_ = connection.SetDeadline(time.Now().Add(maxUpdateControlTimeout))
	decoder := json.NewDecoder(io.LimitReader(connection, 4<<10))
	decoder.DisallowUnknownFields()
	var request ControlRequest
	if err := decoder.Decode(&request); err != nil {
		return s.respond(connection, ControlResponse{Schema: ControlProtocolV1, Status: "error", ErrorCode: "invalid_request"})
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF || request.Schema != ControlProtocolV1 || !validControlRequest(request) {
		return s.respond(connection, ControlResponse{Schema: ControlProtocolV1, Status: "error", ErrorCode: "invalid_request"})
	}
	var response ControlResponse
	var invokeErr error
	if s.invokeRequest != nil {
		response, invokeErr = s.invokeRequest(context.Background(), request)
	} else {
		response, invokeErr = s.invoke(context.Background(), request.Operation)
	}
	if invokeErr != nil {
		response.Schema = ControlProtocolV1
		response.Status = "error"
		response.ErrorCode = controlErrorCode(invokeErr)
		response.ErrorMessage = boundedControlErrorMessage(invokeErr)
	}
	if err := s.respond(connection, response); err != nil {
		return err
	}
	// Finish the response stream before a successful update can replace this
	// process image. The client validates EOF to reject appended control data,
	// so closing only the write half gives it a complete response while the
	// server retains the connection long enough to schedule the handoff.
	if err := connection.CloseWrite(); err != nil {
		return err
	}
	if s.afterResponse != nil {
		s.afterResponse(request, response)
	}
	return nil
}

func controlErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrApprovalRequired), errors.Is(err, workerupdate.ErrApprovalRequired):
		return "approval_required"
	case errors.Is(err, ErrCandidateChanged), errors.Is(err, workerupdate.ErrPreparedCandidate):
		return "candidate_changed"
	case errors.As(err, new(*autoupdate.ActiveTerminalSessionsError)):
		return autoupdate.BlockedActiveTerminalSessions
	case errors.Is(err, ErrRecoveryState), errors.Is(err, workerupdate.ErrBlocked):
		return "recovery_required"
	case errors.Is(err, workerupdate.ErrInvalidRelease):
		return "release_not_found"
	default:
		return "update_failed"
	}
}

func (s *controlServer) respond(writer io.Writer, response ControlResponse) error {
	if response.Schema == "" {
		response.Schema = ControlProtocolV1
	}
	if response.Status == "" {
		response.Status = "ok"
	}
	return json.NewEncoder(writer).Encode(response)
}

func (s *Service) controlRequestWithRequest(ctx context.Context, request ControlRequest) (ControlResponse, error) {
	switch request.Operation {
	case "status":
		response := ControlResponse{Schema: ControlProtocolV1, Status: "ok", Observation: s.Snapshot(), UpdaterVersion: buildinfo.Version}
		response.Version = s.currentManager().ActiveVersion()
		err := s.populateControlState(&response)
		if response.Transaction.ActiveVersion != "" {
			response.Version = response.Transaction.ActiveVersion
		}
		return response, err
	case "check":
		response := ControlResponse{Schema: ControlProtocolV1, Status: "ok", Observation: s.Snapshot(), UpdaterVersion: buildinfo.Version}
		result, err := s.Check(ctx)
		response.Version, response.Updated = result.Version, result.Updated
		stateErr := s.populateControlState(&response)
		return response, errors.Join(err, stateErr)
	case "download", "install":
		s.controlMu.Lock()
		defer s.controlMu.Unlock()
		response := ControlResponse{Schema: ControlProtocolV1, Status: "ok", Observation: s.Snapshot(), UpdaterVersion: buildinfo.Version}
		var err error
		if request.Operation == "download" {
			var candidate workerupdate.PreparedCandidate
			candidate, err = s.Download(ctx)
			if candidate.ID != "" {
				response.Candidate = &candidate
				response.Version = candidate.Version
			}
		} else {
			var result workerupdate.Result
			result, err = s.Install(ctx, request.ApprovalID)
			response.Version, response.Updated = result.Version, result.Updated
		}
		stateErr := s.populateControlState(&response)
		return response, errors.Join(err, stateErr)
	default:
		return ControlResponse{}, ErrInvalidControl
	}
}

// All control operations report the durable activation outcome. A missing
// handoff means complete only when both state records were read successfully.
func (s *Service) populateControlState(response *ControlResponse) error {
	state, err := s.currentManager().TransactionState()
	if err != nil {
		return ErrRecoveryState
	}
	response.Transaction = state
	if state.Stage == updateflow.StageAwaitingApproval {
		candidate, candidateErr := s.currentManager().PreparedCandidate()
		if candidateErr != nil {
			return candidateErr
		}
		response.Candidate = &candidate
	}
	response.ActivationFailure = string(state.Failure)
	if s.config.StateRoot != "" {
		handoff, err := readUnixHandoff(s.config.StateRoot)
		if err != nil {
			return ErrRecoveryState
		}
		response.Pending = handoff != nil
	}
	return nil
}
