package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/browserbroadcastserver"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configapply"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/execprocess"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/health"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/history"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/process"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
)

type HealthSource interface{ Snapshot() health.Snapshot }
type SessionLauncher interface {
	Launch(context.Context, process.LaunchRequest) (session.Snapshot, error)
}
type CapabilityGate interface{ Enabled(string) bool }

type TerminalJoin struct {
	AccessSessionID   string `json:"access_session_id"`
	TerminalSessionID string `json:"terminal_session_id"`
	AttachmentID      string `json:"attachment_id"`
}
type TerminalJoinRecorder func(context.Context, TerminalJoin) error

type DispatcherConfig struct {
	RecordTerminalJoin TerminalJoinRecorder
	BrowserOutput      *browserbroadcastserver.Registry
	Sessions           session.Service
	ConfigApply        configapply.Handler
	ConfigCompare      ConfigComparisonReader
	Health             HealthSource
	SessionLauncher    SessionLauncher
	WorkspaceRoot      string
	Random             io.Reader
	Now                func() time.Time
	FileTransfers      *filetransfer.Service
	Writers            *filetransfer.WriterRegistry
	Exec               execprocess.Service
	SSH                *managedssh.Host
	Capabilities       CapabilityGate
	Inspector          BrowserInspector
}

type Dispatcher struct {
	compareGate           chan struct{}
	config                DispatcherConfig
	browserTransferCreate chan struct{}
	ssh                   sshStreamRegistry
	// browserScreens maps a live browser attachment to its stream's checkpoint requests ("screen").
	browserScreens sync.Map
}

func NewDispatcher(config DispatcherConfig) (*Dispatcher, error) {
	if config.Sessions == nil || config.Health == nil || config.SessionLauncher == nil || !filepath.IsAbs(config.WorkspaceRoot) || config.Random == nil {
		return nil, ErrInvalidConfiguration
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Dispatcher{compareGate: make(chan struct{}, 1), config: config, browserTransferCreate: make(chan struct{}, 1)}, nil
}

func (d *Dispatcher) Capabilities() []string {
	capabilities := []string{"health.v1"}
	capabilities = append(capabilities, "terminal.v1")
	if d.config.FileTransfers != nil {
		capabilities = append(capabilities, "file-transfer.v1")
	}
	if d.config.ConfigCompare != nil {
		capabilities = append(capabilities, "config.compare.v1")
	}
	if d.config.ConfigApply != nil {
		capabilities = append(capabilities, "config.apply.v1")
	}
	if d.config.Exec != nil {
		capabilities = append(capabilities, "exec.v1")
	}
	if d.config.SSH != nil {
		capabilities = append(capabilities, "ssh.v1")
	}
	if d.config.Inspector != nil {
		capabilities = append(capabilities, "inspector.v1")
	}
	return capabilities
}

func (d *Dispatcher) Handle(ctx context.Context, authorization Authorization, capability string, payload json.RawMessage) operation.Outcome {
	ctx, notices := envinject.WithLaunchNotices(ctx)
	return withLaunchWarnings(d.handle(ctx, authorization, capability, payload), notices)
}

// withLaunchWarnings reports, inside a successful object result, that managed
// ENV was not applied to the launch. The launch itself already ran.
func withLaunchWarnings(outcome operation.Outcome, notices *envinject.LaunchNotices) operation.Outcome {
	codes := notices.Codes()
	if len(codes) == 0 || outcome.ErrorCode != "" || len(outcome.Result) == 0 {
		return outcome
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(outcome.Result, &fields) != nil || fields == nil {
		return outcome
	}
	encoded, err := json.Marshal(codes)
	if err != nil {
		return outcome
	}
	fields["launch_warnings"] = encoded
	merged, err := json.Marshal(fields)
	if err != nil {
		return outcome
	}
	outcome.Result = merged
	return outcome
}

func (d *Dispatcher) handle(ctx context.Context, authorization Authorization, capability string, payload json.RawMessage) operation.Outcome {
	ctx = envinject.WithLaunchContext(ctx, authorization.WorkspaceID, authorization.ActorAccountID)
	if capability != "health.v1" && !d.capabilityEnabled(capability) {
		return failure("capability_disabled")
	}
	switch capability {
	case "file-transfer.v1":
		return d.browserFileTransfer(ctx, authorization, payload)
	case "terminal.v1":
		return d.terminal(ctx, authorization, payload)
	case "health.v1":
		return result(d.config.Health.Snapshot())
	case "config.compare.v1":
		return d.configCompare(ctx, authorization, payload)
	case "config.apply.v1":
		return d.configApply(ctx, authorization, payload)
	case "exec.v1":
		return d.exec(ctx, payload)
	case "ssh.v1":
		return d.sshRequest(payload)
	case "inspector.v1":
		return d.browserInspector(ctx, authorization, payload)
	default:
		return failure("capability_required")
	}
}

func (d *Dispatcher) capabilityEnabled(capability string) bool {
	return d.config.Capabilities == nil || d.config.Capabilities.Enabled(capability)
}

// CloseSSHStreams terminates active managed-SSH streams when that capability
// is disabled. The host remains available for a later re-enable.
func (d *Dispatcher) CloseSSHStreams() error { return d.ssh.closeAll() }

func (d *Dispatcher) HandleOperation(ctx context.Context, authorization Authorization, capability, operationID string, payload json.RawMessage) operation.Outcome {
	if capability == "exec.v1" {
		var request execRequest
		if decodeStrict(payload, &request) != nil || request.OperationID != operationID {
			return failure("invalid_request")
		}
	}
	if capability == "ssh.v1" {
		var request sshRequest
		if decodeStrict(payload, &request) != nil || request.OperationID != operationID {
			return failure("invalid_request")
		}
	}
	return d.Handle(ctx, authorization, capability, payload)
}

type configApplyRequest struct {
	Action           string `json:"action"`
	AssignmentID     string `json:"assignment_id"`
	ExpectedRevision string `json:"expected_revision"`
	ObservedRevision string `json:"observed_revision"`
}

func (d *Dispatcher) configApply(ctx context.Context, authorization Authorization, payload json.RawMessage) operation.Outcome {
	if d.config.ConfigApply == nil {
		return failure("capability_required")
	}
	var request configApplyRequest
	if decodeStrict(payload, &request) != nil || authorization.ResourceID == "" || request.AssignmentID != authorization.ResourceID {
		return failure("not_found_or_forbidden")
	}
	value, err := d.config.ConfigApply.Handle(ctx, configapply.Request{
		Action: request.Action, AssignmentID: request.AssignmentID,
		ExpectedRevision: request.ExpectedRevision, ObservedRevision: request.ObservedRevision,
	})
	return domainResult(value, err)
}

type attachmentControl struct {
	SessionID    string `json:"session_id"`
	AttachmentID string `json:"attachment_id"`
	NextSequence uint64 `json:"next_sequence,omitempty"`
}

func (d *Dispatcher) mayType(authorization Authorization, sessionID, attachmentID string, generation uint64) bool {
	return !(authorization.ClientID == "" || sessionID == "" || attachmentID == "" || generation == 0 || isGenerationBoundTerminal(authorization) && generation != authorization.TerminalGeneration || (authorization.SessionID != "" && authorization.SessionID != sessionID) || authorization.TerminalRole == TerminalRoleViewer || isGenerationBoundTerminal(authorization) && (!browserAttachmentMatches(authorization, attachmentID) || !d.config.Sessions.AttachmentOwnedBy(sessionID, attachmentID, authorization.AccountID, authorization.ClientID)))
}

func (d *Dispatcher) HandleTerminalInput(_ context.Context, authorization Authorization, sessionID, attachmentID string, generation, inputSequence uint64, data []byte) (session.InputDecision, error) {
	if !d.mayType(authorization, sessionID, attachmentID, generation) {
		return session.InputDecision{InputSequence: inputSequence}, session.ErrInvalidInput
	}
	decision, err := d.config.Sessions.Write(sessionID, session.InputKey{ClientID: authorization.ClientID, AttachmentID: attachmentID, Generation: generation, InputSequence: inputSequence}, data)
	if err == nil && d.config.Writers != nil && decision.Status != session.InputUncertain {
		d.config.Writers.Input(sessionID, attachmentID, authorization.ClientID, d.config.Now())
	}
	if decision.InputSequence == 0 {
		decision.InputSequence = inputSequence
	}
	return decision, err
}

// InterruptTerminalInput delivers a lone interrupt key ahead of input still queued for the stream; the key's own
// input frame follows in order and is answered from the journal.
func (d *Dispatcher) InterruptTerminalInput(_ context.Context, authorization Authorization, sessionID, attachmentID string, generation, inputSequence uint64, data byte) bool {
	return d.mayType(authorization, sessionID, attachmentID, generation) && d.config.Sessions.Interrupt(sessionID, session.InputKey{ClientID: authorization.ClientID, AttachmentID: attachmentID, Generation: generation, InputSequence: inputSequence}, data)
}

func (d *Dispatcher) HandleTerminalACK(_ context.Context, authorization Authorization, sessionID, attachmentID string, nextSequence uint64) error {
	if authorization.ClientID == "" || sessionID == "" || attachmentID == "" || (authorization.SessionID != "" && authorization.SessionID != sessionID) || isGenerationBoundTerminal(authorization) && (!browserAttachmentMatches(authorization, attachmentID) || !d.config.Sessions.AttachmentOwnedBy(sessionID, attachmentID, authorization.AccountID, authorization.ClientID)) {
		return session.ErrInvalidInput
	}
	if isGenerationBoundTerminal(authorization) {
		return d.config.Sessions.AcknowledgeAtGeneration(sessionID, attachmentID, nextSequence, authorization.TerminalGeneration)
	}
	return d.config.Sessions.Acknowledge(sessionID, attachmentID, nextSequence)
}

func (d *Dispatcher) HandleTerminalResize(_ context.Context, authorization Authorization, sessionID, attachmentID string, columns, rows uint16) error {
	if authorization.ClientID == "" || sessionID == "" || attachmentID == "" || columns == 0 || rows == 0 || (authorization.SessionID != "" && authorization.SessionID != sessionID) || authorization.TerminalRole == TerminalRoleViewer || isGenerationBoundTerminal(authorization) && (!browserAttachmentMatches(authorization, attachmentID) || !d.config.Sessions.AttachmentOwnedBy(sessionID, attachmentID, authorization.AccountID, authorization.ClientID)) {
		return session.ErrInvalidInput
	}
	if isGenerationBoundTerminal(authorization) {
		return d.config.Sessions.ResizeParticipantAtGeneration(sessionID, attachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration, pty.Dimensions{Columns: columns, Rows: rows}, d.config.Now())
	}
	return d.config.Sessions.Resize(sessionID, attachmentID, pty.Dimensions{Columns: columns, Rows: rows}, d.config.Now())
}

func (d *Dispatcher) HandleExecInput(_ context.Context, authorization Authorization, operationID string, data []byte) error {
	if d.config.Exec == nil || authorization.ClientID == "" || operationID == "" || len(data) == 0 {
		return execprocess.ErrInvalid
	}
	execution, err := d.config.Exec.Get(operationID)
	if err != nil {
		return err
	}
	_, err = execution.Write(data)
	return err
}

func (d *Dispatcher) HandleExecEOF(_ context.Context, authorization Authorization, operationID string) error {
	if d.config.Exec == nil || authorization.ClientID == "" || operationID == "" {
		return execprocess.ErrInvalid
	}
	execution, err := d.config.Exec.Get(operationID)
	if err != nil {
		return err
	}
	return execution.CloseInput()
}

func (d *Dispatcher) HandleExecResize(_ context.Context, authorization Authorization, operationID string, columns, rows uint16) error {
	if d.config.Exec == nil || authorization.ClientID == "" || operationID == "" || columns == 0 || rows == 0 {
		return execprocess.ErrInvalid
	}
	execution, err := d.config.Exec.Get(operationID)
	if err != nil {
		return err
	}
	return execution.Resize(pty.Dimensions{Columns: columns, Rows: rows})
}

func (d *Dispatcher) HandleControl(_ context.Context, authorization Authorization, frame protocol.Frame) operation.Outcome {
	var control attachmentControl
	if decodeStrict(frame.Payload, &control) != nil || control.SessionID == "" || control.AttachmentID == "" || authorization.ClientID == "" {
		return failure("invalid_request")
	}
	if authorization.SessionID != "" && authorization.SessionID != control.SessionID || isGenerationBoundTerminal(authorization) && (!browserAttachmentMatches(authorization, control.AttachmentID) || !d.config.Sessions.AttachmentOwnedBy(control.SessionID, control.AttachmentID, authorization.AccountID, authorization.ClientID)) {
		return failure("not_found_or_forbidden")
	}
	var err error
	switch frame.Type {
	case "ack":
		if isGenerationBoundTerminal(authorization) {
			err = d.config.Sessions.AcknowledgeAtGeneration(control.SessionID, control.AttachmentID, control.NextSequence, authorization.TerminalGeneration)
		} else {
			err = d.config.Sessions.Acknowledge(control.SessionID, control.AttachmentID, control.NextSequence)
		}
	case "detach":
		if isGenerationBoundTerminal(authorization) {
			err = d.config.Sessions.DetachParticipantAtGeneration(control.SessionID, control.AttachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration)
		} else {
			err = d.config.Sessions.Detach(control.SessionID, control.AttachmentID)
		}
		if err == nil && d.config.Writers != nil {
			d.config.Writers.Detach(control.SessionID, control.AttachmentID)
		}
	default:
		return failure("invalid_request")
	}
	return domainResult(struct{}{}, err)
}

func execResult(value any, err error) operation.Outcome {
	var delivery *envinject.DeliveryError
	if errors.As(err, &delivery) {
		return failure("environment_unavailable")
	}
	switch {
	case err == nil:
		return result(value)
	case allErrorLeavesMatch(err, func(leaf error) bool { return leaf == envinject.ErrNotReady }):
		return failure("environment_unavailable")
	case errors.Is(err, execprocess.ErrNotFound):
		return failure("not_found_or_forbidden")
	case errors.Is(err, execprocess.ErrConflict):
		return failure("exec_already_running")
	case errors.Is(err, execprocess.ErrCapacity):
		return failure("resource_limit")
	case errors.Is(err, execprocess.ErrReplayUnavailable):
		return failure("exec_result_unavailable")
	default:
		return failure("invalid_request")
	}
}

type execOutputStream struct {
	execution          execprocess.ExecutionService
	reader             execprocess.ReaderService
	lastOutputSequence uint64
}

const execOutputEnvelopeVersion = byte(1)

func encodeExecOutput(sequence uint64, data []byte) []byte {
	encoded := make([]byte, 9+len(data))
	encoded[0] = execOutputEnvelopeVersion
	binary.BigEndian.PutUint64(encoded[1:9], sequence)
	copy(encoded[9:], data)
	return encoded
}

func (s *execOutputStream) Next(ctx context.Context) (protocol.BinaryFrame, error) {
	for {
		event, release, err := s.reader.Next(ctx)
		if errors.Is(err, execprocess.ErrReplayUnavailable) {
			return protocol.BinaryFrame{}, &StreamError{Code: "replay_gap", CloseCode: protocol.CloseReplayGap}
		}
		if err != nil {
			return protocol.BinaryFrame{}, err
		}
		if len(event.Data) != 0 {
			s.lastOutputSequence = event.Sequence
			channel := protocol.Stdout
			if event.Stream == "stderr" {
				channel = protocol.Stderr
			}
			return protocol.BinaryFrame{Channel: channel, StartSequence: event.StreamSequence, Data: encodeExecOutput(event.Sequence, event.Data), Release: release}, nil
		}
		release()
		if event.State == execprocess.StateExited || event.State == execprocess.StateSignaled || event.State == execprocess.StateCanceled || event.State == execprocess.StateFailed {
			payload, marshalErr := json.Marshal(struct {
				Event              string              `json:"event"`
				OperationID        string              `json:"operation_id"`
				State              execprocess.State   `json:"state"`
				Result             *execprocess.Result `json:"result,omitempty"`
				ErrorCode          string              `json:"error_code,omitempty"`
				Sequence           uint64              `json:"sequence"`
				LastOutputSequence uint64              `json:"last_output_sequence,omitempty"`
			}{"exec_stream_end", s.execution.Snapshot().OperationID, event.State, event.Result, event.ErrorCode, event.Sequence, s.lastOutputSequence})
			if marshalErr != nil {
				return protocol.BinaryFrame{}, marshalErr
			}
			return protocol.BinaryFrame{}, &StreamEnd{Payload: payload}
		}
	}
}

func (s *execOutputStream) Close() error {
	if s == nil || s.reader == nil {
		return nil
	}
	return s.reader.Close()
}

func (s *terminalOutputStream) Next(ctx context.Context) (protocol.BinaryFrame, error) {
	if !s.checkpointSent {
		const chunkSize = 64 << 10
		remaining := len(s.checkpoint) - s.checkpointOffset
		n := min(remaining, chunkSize)
		last := n == remaining
		data := make([]byte, 1+n)
		if last {
			data[0] = 1
		}
		copy(data[1:], s.checkpoint[s.checkpointOffset:s.checkpointOffset+n])
		s.checkpointOffset += n
		if last {
			s.checkpointSent = true
			s.checkpoint = nil
		}
		return protocol.BinaryFrame{Channel: protocol.TerminalScreenCheckpoint, StartSequence: s.checkpointSequence, Data: data}, nil
	}
	if s.keyUpdates != nil {
		for {
			// Only browser terminals (this key-update stream) get participant events: released CLI
			// terminals end the connection on an event they do not know.
			if err := s.participantsEvent(); err != nil {
				return protocol.BinaryFrame{}, err
			}
			select {
			case <-ctx.Done():
				return protocol.BinaryFrame{}, ctx.Err()
			case <-s.screenRequests:
				if err := s.loadScreen(); err != nil {
					return protocol.BinaryFrame{}, err
				}
				return s.Next(ctx)
			case epoch, ok := <-s.keyUpdates:
				if !ok {
					return protocol.BinaryFrame{}, ErrStreamClosed
				}
				data := make([]byte, 1+len(epoch.ID)+len(epoch.Key))
				data[0] = 1
				copy(data[1:17], epoch.ID[:])
				copy(data[17:], epoch.Key[:])
				return protocol.BinaryFrame{Channel: protocol.TerminalBroadcastKey, StartSequence: s.checkpointSequence, Data: data}, nil
			case <-time.After(time.Second):
				snapshot, err := s.snapshot()
				if err != nil {
					return protocol.BinaryFrame{}, ErrStreamClosed
				}
				if snapshot.State == session.Exited || snapshot.State == session.Closed {
					payload, marshalErr := json.Marshal(struct {
						Event         string          `json:"event"`
						SessionID     string          `json:"session_id"`
						State         session.State   `json:"state"`
						FinalSequence uint64          `json:"final_sequence"`
						Exit          *pty.ExitResult `json:"exit,omitempty"`
					}{"terminal_stream_end", snapshot.ID, snapshot.State, snapshot.LatestSequence, snapshot.Exit})
					if marshalErr != nil {
						return protocol.BinaryFrame{}, marshalErr
					}
					return protocol.BinaryFrame{}, &StreamEnd{Payload: payload}
				}
				continue
			}
		}
	}
	if _, err := s.snapshot(); err != nil {
		return protocol.BinaryFrame{}, ErrStreamClosed
	}
	for {
		if s.idleWait == 0 {
			s.idleWait = terminalExitObservationInterval
		}
		waitCtx, cancel := context.WithTimeout(ctx, s.idleWait)
		event, err := s.manager.WaitNext(waitCtx, s.sessionID, s.attachmentID)
		cancel()
		if err == nil {
			if _, err := s.snapshot(); err != nil {
				event.Release()
				return protocol.BinaryFrame{}, ErrStreamClosed
			}
			s.idleWait = terminalExitObservationInterval
			return protocol.BinaryFrame{Channel: event.Channel, StartSequence: event.StartSequence, Data: event.Data, Release: event.Release}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
			snapshot, snapshotErr := s.snapshot()
			if snapshotErr != nil {
				return protocol.BinaryFrame{}, ErrStreamClosed
			}
			if snapshot.State != session.Exited && snapshot.State != session.Closed {
				s.idleWait = min(2*s.idleWait, terminalIdleObservationMaximum)
				continue
			}
			payload, marshalErr := json.Marshal(struct {
				Event         string          `json:"event"`
				SessionID     string          `json:"session_id"`
				State         session.State   `json:"state"`
				FinalSequence uint64          `json:"final_sequence"`
				Exit          *pty.ExitResult `json:"exit,omitempty"`
			}{"terminal_stream_end", snapshot.ID, snapshot.State, snapshot.LatestSequence, snapshot.Exit})
			if marshalErr != nil {
				return protocol.BinaryFrame{}, marshalErr
			}
			return protocol.BinaryFrame{}, &StreamEnd{Payload: payload}
		}
		if errors.Is(err, session.ErrAttachmentLagged) {
			// A free-running viewer fell behind a flood: send it the current screen in-band, no reconnect.
			if err := s.loadScreen(); err != nil {
				return protocol.BinaryFrame{}, err
			}
			return s.Next(ctx)
		}
		if errors.Is(err, session.ErrAttachmentEvicted) {
			_, queued, _ := s.manager.AttachmentStatus(s.sessionID, s.attachmentID)
			details, _ := json.Marshal(struct {
				QueuedBytes uint64 `json:"queued_bytes"`
			}{queued})
			return protocol.BinaryFrame{}, &StreamError{Code: "slow_consumer", Details: details, CloseCode: protocol.CloseSlowConsumer}
		}
		if errors.Is(err, session.ErrInvalidTransition) || errors.Is(err, session.ErrAttachmentUnknown) {
			return protocol.BinaryFrame{}, ErrStreamClosed
		}
		return protocol.BinaryFrame{}, err
	}
}

// loadScreen queues a fresh checkpoint of this attachment's screen and moves it to that boundary.
func (s *terminalOutputStream) loadScreen() error {
	checkpoint, boundary, err := s.manager.BrowserScreenAttachment(s.sessionID, s.attachmentID, s.accountID, s.clientID, s.generation)
	if err != nil {
		return err
	}
	s.checkpoint, s.checkpointSequence, s.checkpointOffset, s.checkpointSent = checkpoint, boundary, 0, false
	return nil
}

func (s *terminalOutputStream) snapshot() (session.Snapshot, error) {
	if s.expectedGeneration != 0 {
		return s.manager.SnapshotAtGeneration(s.sessionID, s.expectedGeneration)
	}
	return s.manager.Snapshot(s.sessionID)
}

func (s *terminalOutputStream) Close() error {
	if s.unregisterScreens != nil {
		s.unregisterScreens()
	}
	if s.broadcast != nil {
		s.broadcast.Leave(s.sessionID, s.attachmentID)
	}
	err := s.manager.Detach(s.sessionID, s.attachmentID)
	if s.writers != nil {
		s.writers.Detach(s.sessionID, s.attachmentID)
	}
	return err
}

type terminalRequest struct {
	Action         string            `json:"action"`
	OperationID    string            `json:"operation_id,omitempty"`
	Name           string            `json:"name,omitempty"`
	CWD            string            `json:"cwd,omitempty"`
	SessionID      string            `json:"session_id,omitempty"`
	AttachmentID   string            `json:"attachment_id,omitempty"`
	FromSequence   uint64            `json:"from_sequence,omitempty"`
	AtLiveBoundary bool              `json:"at_live_boundary,omitempty"`
	Generation     uint64            `json:"generation,omitempty"`
	Columns        uint16            `json:"columns,omitempty"`
	Rows           uint16            `json:"rows,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Signal         string            `json:"signal,omitempty"`
	// ExistingSnapshot asks create to return the current snapshot instead of
	// failing when the session already exists. Responses to such creates carry
	// an "existing" flag so the client can pick the right attach boundary.
	ExistingSnapshot bool `json:"existing_snapshot,omitempty"`
}

func (d *Dispatcher) terminal(ctx context.Context, authorization Authorization, payload json.RawMessage) operation.Outcome {
	var request terminalRequest
	if decodeStrict(payload, &request) != nil {
		return failure("invalid_terminal_payload")
	}
	if authorization.ClientID == "" {
		return failure("not_found_or_forbidden")
	}
	if authorization.SessionID != "" && request.SessionID != "" && authorization.SessionID != request.SessionID {
		return failure("not_found_or_forbidden")
	}
	shared := isSharedTerminal(authorization)
	participantBound := isGenerationBoundTerminal(authorization)
	if (shared || authorization.BrowserTerminal) && (authorization.SessionID == "" || request.SessionID != "" && request.SessionID != authorization.SessionID) {
		return failure("not_found_or_forbidden")
	}
	if shared && !sharedTerminalActionAllowed(authorization.TerminalRole, request) {
		return failure("not_found_or_forbidden")
	}
	if authorization.BrowserTerminal && !browserTerminalActionAllowed(authorization.TerminalRole, request) {
		return failure("not_found_or_forbidden")
	}
	if participantBound && request.SessionID == "" {
		request.SessionID = authorization.SessionID
	}
	switch request.Action {
	case "list":
		if authorization.SessionID != "" {
			value, err := d.config.Sessions.SnapshotAtGeneration(authorization.SessionID, authorization.TerminalGeneration)
			if err != nil {
				return domainResult(nil, err)
			}
			return result(struct {
				Sessions []session.Snapshot `json:"sessions"`
			}{[]session.Snapshot{value}})
		}
		return result(struct {
			Sessions []session.Snapshot `json:"sessions"`
		}{d.config.Sessions.List()})
	case "get", "snapshot":
		value, err := d.config.Sessions.SnapshotAtGeneration(request.SessionID, authorization.TerminalGeneration)
		return domainResult(value, err)
	case "cursor":
		value, err := d.config.Sessions.SnapshotAtGeneration(request.SessionID, authorization.TerminalGeneration)
		if err != nil {
			return domainResult(nil, err)
		}
		return result(struct {
			LatestSequence uint64 `json:"latest_sequence"`
		}{value.LatestSequence})
	case "transfer-destinations":
		value, err := d.config.Sessions.SnapshotAtGeneration(request.SessionID, authorization.TerminalGeneration)
		if err != nil {
			return domainResult(nil, err)
		}
		eligible := []string{}
		if d.config.Writers != nil {
			eligible = d.config.Writers.EligibleMachines(request.SessionID)
		}
		return result(struct {
			session.Snapshot
			EligibleTransferDestinationMachineIDs []string `json:"eligible_transfer_destination_machine_ids"`
		}{Snapshot: value, EligibleTransferDestinationMachineIDs: eligible})
	case "create":
		if authorization.BrowserTerminal {
			return failure("not_found_or_forbidden")
		}
		if request.SessionID == "" {
			request.SessionID = authorization.SessionID
		}
		cwd, ok := d.cwd(request.CWD)
		if !ok {
			return failure("invalid_terminal_workspace")
		}
		if request.Columns == 0 || request.Rows == 0 {
			return failure("invalid_terminal_dimensions")
		}
		value, err := d.config.SessionLauncher.Launch(ctx, process.LaunchRequest{ID: request.SessionID, Name: request.Name, CWD: cwd, Dimensions: pty.Dimensions{Columns: request.Columns, Rows: request.Rows}, Environment: request.Environment})
		if err != nil && !allErrorLeavesMatch(err, func(leaf error) bool { return leaf == session.ErrSessionExists }) {
			outcome := domainResult(nil, err)
			if outcome.ErrorCode == "unavailable" {
				fault := errorreport.Current().CaptureFailure(ctx, "paperboat-daemon", "sessions", "command", "terminal_session_failed", err)
				return failureDetails("terminal_create_failed", struct {
					ErrorType        string `json:"error_type"`
					Errno            int    `json:"errno"`
					SupportReference string `json:"support_reference"`
				}{fault.ErrorType, fault.Errno, fault.SupportReference})
			}
			return outcome
		}
		if request.ExistingSnapshot {
			// create-or-get lets one round trip both create a fresh session
			// and resolve an already-running one. A name collision without a
			// matching session ID keeps the original failure so clients
			// cannot claim another session's name.
			if errors.Is(err, session.ErrSessionExists) {
				if snapshot, snapshotErr := d.config.Sessions.Snapshot(request.SessionID); snapshotErr == nil {
					return result(struct {
						session.Snapshot
						Existing bool `json:"existing"`
					}{Snapshot: snapshot, Existing: true})
				}
			}
			if err == nil {
				return result(struct {
					session.Snapshot
					Existing bool `json:"existing"`
				}{Snapshot: value, Existing: false})
			}
		}
		return domainResult(value, err)
	case "attach", "replay":
		attachmentID := request.AttachmentID
		if authorization.BrowserTerminal {
			if attachmentID != "" && attachmentID != authorization.BrowserAttachmentID {
				return failure("not_found_or_forbidden")
			}
			attachmentID = authorization.BrowserAttachmentID
		}
		if attachmentID == "" {
			attachmentID = d.randomID()
		}
		if attachmentID == "" {
			return failure("unavailable")
		}
		value, err := d.attachTerminalParticipant(authorization, request, attachmentID)
		if err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "sessions", "attach", "terminal_session_failed", err)
			return domainResult(nil, err)
		}
		attachResponse := newTerminalAttachResponse(attachmentID, value)
		if attachResponse.InputSequence, err = d.config.Sessions.InputSequence(request.SessionID, authorization.ClientID, attachmentID, value.Snapshot.Generation); err != nil {
			errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "sessions", "input_sequence", "terminal_session_failed", err)
			if participantBound {
				_ = d.config.Sessions.DetachParticipantAtGeneration(request.SessionID, attachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration)
			}
			return domainResult(nil, err)
		}
		if shared {
			joinCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			if d.config.RecordTerminalJoin == nil {
				err = errors.New("terminal join recording unavailable")
			} else {
				err = d.config.RecordTerminalJoin(joinCtx, TerminalJoin{AccessSessionID: authorization.ResourceID, TerminalSessionID: request.SessionID, AttachmentID: attachmentID})
			}
			if err == nil {
				err = joinCtx.Err()
			}
			cancel()
			if err != nil {
				// A concurrent restart/removal already clears the old-generation participant.
				_ = d.config.Sessions.DetachParticipantAtGeneration(request.SessionID, attachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration)
				return failure("unavailable")
			}
		}
		// Replay bytes travel as binary frames after this control response. Keeping
		// them out of JSON prevents a terminal-sized replay from overflowing the
		// structured-frame limit and avoids delivering the same output twice.
		return result(attachResponse)
	case "detach":
		var err error
		if authorization.BrowserTerminal {
			if request.AttachmentID != "" && request.AttachmentID != authorization.BrowserAttachmentID {
				return failure("not_found_or_forbidden")
			}
			request.AttachmentID = authorization.BrowserAttachmentID
		}
		if participantBound {
			err = d.config.Sessions.DetachParticipantAtGeneration(request.SessionID, request.AttachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration)
		} else {
			err = d.config.Sessions.Detach(request.SessionID, request.AttachmentID)
		}
		if err == nil && d.config.Writers != nil {
			d.config.Writers.Detach(request.SessionID, request.AttachmentID)
		}
		return domainResult(struct{}{}, err)
	case "screen":
		// A browser that fell behind a flood asks for a fresh screen on its live stream instead of reconnecting.
		requests, ok := d.browserScreens.Load(authorization.BrowserAttachmentID)
		if !authorization.BrowserTerminal || !ok {
			return failure("not_found_or_forbidden")
		}
		select {
		case requests.(chan struct{}) <- struct{}{}:
		default: // one is already pending; it will reflect everything up to when it is taken
		}
		return result(struct{}{})
	case "resize":
		if authorization.BrowserTerminal && !browserAttachmentMatches(authorization, request.AttachmentID) {
			return failure("not_found_or_forbidden")
		}
		var err error
		if participantBound {
			err = d.config.Sessions.ResizeParticipantAtGeneration(request.SessionID, request.AttachmentID, authorization.AccountID, authorization.ClientID, authorization.TerminalGeneration, pty.Dimensions{Columns: request.Columns, Rows: request.Rows}, d.config.Now())
		} else {
			err = d.config.Sessions.Resize(request.SessionID, request.AttachmentID, pty.Dimensions{Columns: request.Columns, Rows: request.Rows}, d.config.Now())
		}
		return domainResult(struct{}{}, err)
	case "signal":
		generation := request.Generation
		if participantBound {
			generation = authorization.TerminalGeneration
		}
		err := d.config.Sessions.Signal(request.SessionID, generation, pty.Signal(request.Signal))
		return domainResult(struct{}{}, err)
	case "clear":
		if authorization.BrowserTerminal {
			return failure("not_found_or_forbidden")
		}
		sequence, err := d.config.Sessions.Clear(request.SessionID)
		return domainResult(struct {
			Sequence uint64 `json:"sequence"`
		}{sequence}, err)
	case "restart":
		if authorization.BrowserTerminal {
			return failure("not_found_or_forbidden")
		}
		value, err := d.config.Sessions.RestartContext(ctx, request.SessionID)
		return domainResult(value, err)
	case "close":
		if authorization.BrowserTerminal {
			return failure("not_found_or_forbidden")
		}
		value, err := d.config.Sessions.Close(ctx, request.SessionID)
		if errors.Is(err, session.ErrSessionUnknown) {
			return result(session.Snapshot{ID: request.SessionID, State: session.Closed})
		}
		return domainResult(value, err)
	case "delete":
		if authorization.BrowserTerminal {
			return failure("not_found_or_forbidden")
		}
		err := d.config.Sessions.Delete(request.SessionID)
		if errors.Is(err, session.ErrSessionUnknown) {
			return result(struct{}{})
		}
		return domainResult(struct{}{}, err)
	default:
		return failure("invalid_terminal_action")
	}
}

func isSharedTerminal(authorization Authorization) bool {
	return !authorization.BrowserTerminal && (authorization.TerminalRole == TerminalRoleViewer || authorization.TerminalRole == TerminalRoleInteractive)
}

func isGenerationBoundTerminal(authorization Authorization) bool {
	return authorization.BrowserTerminal || isSharedTerminal(authorization)
}

func browserAttachmentMatches(authorization Authorization, attachmentID string) bool {
	return !authorization.BrowserTerminal || authorization.BrowserAttachmentID != "" && authorization.BrowserAttachmentID == attachmentID
}

func browserTerminalActionAllowed(role TerminalRole, request terminalRequest) bool {
	switch request.Action {
	case "attach", "replay", "detach", "cursor", "screen":
		return role == TerminalRoleOwner || role == TerminalRoleViewer || role == TerminalRoleInteractive
	case "resize", "signal":
		return role == TerminalRoleOwner || role == TerminalRoleInteractive
	default:
		return false
	}
}

func sharedTerminalActionAllowed(role TerminalRole, request terminalRequest) bool {
	switch request.Action {
	case "list", "get", "snapshot", "attach", "replay", "detach":
		return !request.AtLiveBoundary
	case "resize", "signal":
		return role == TerminalRoleInteractive
	default:
		return false
	}
}

func (d *Dispatcher) cwd(value string) (string, bool) {
	if value == "" {
		value = d.config.WorkspaceRoot
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(d.config.WorkspaceRoot, value)
	}
	// Authenticated terminal creation launches the enrolled user's shell. The
	// workspace root is the default, not a filesystem sandbox for that shell.
	return filepath.Clean(value), !strings.ContainsRune(value, '\x00')
}

func (d *Dispatcher) randomID() string {
	id, err := uuid.NewRandomFromReader(d.config.Random)
	if err != nil {
		return ""
	}
	return "attachment_" + id.String()
}

func decodeStrict(payload []byte, target any) error {
	if err := rejectDuplicateJSONKeys(payload); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func rejectDuplicateJSONKeys(payload []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return errors.New("duplicate JSON object key")
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func result(value any) operation.Outcome {
	encoded, err := json.Marshal(value)
	if err != nil {
		return failure("unavailable")
	}
	return operation.Outcome{Result: encoded}
}

func failure(code string) operation.Outcome { return operation.Outcome{ErrorCode: code} }

func failureDetails(code string, details any) operation.Outcome {
	encoded, err := json.Marshal(details)
	if err != nil {
		return failure("unavailable")
	}
	return operation.Outcome{ErrorCode: code, Result: encoded}
}

func domainResult(value any, err error) operation.Outcome {
	var delivery *envinject.DeliveryError
	if errors.As(err, &delivery) || allErrorLeavesMatch(err, func(leaf error) bool { return leaf == envinject.ErrNotReady }) {
		return failure("environment_unavailable")
	}
	if err == nil {
		return result(value)
	}
	var gap *history.GapError
	if allErrorLeavesMatch(err, func(leaf error) bool {
		_, ok := leaf.(*history.GapError)
		return ok
	}) && errors.As(err, &gap) {
		return failureDetails("replay_gap", struct {
			RequestedSequence uint64 `json:"requested_sequence"`
			EarliestSequence  uint64 `json:"earliest_sequence"`
			LatestSequence    uint64 `json:"latest_sequence"`
		}{gap.RequestedSequence, gap.EarliestSequence, gap.LatestSequence})
	}
	var stale *session.StaleGenerationError
	if allErrorLeavesMatch(err, func(leaf error) bool {
		return leaf == session.ErrStaleGeneration
	}) && errors.As(err, &stale) {
		return failureDetails("stale_generation", struct {
			CurrentGeneration uint64 `json:"current_generation"`
		}{stale.CurrentGeneration})
	}
	allLeavesAre := func(want error) bool {
		return allErrorLeavesMatch(err, func(leaf error) bool { return leaf == want })
	}
	switch {
	case allLeavesAre(context.Canceled):
		return failure("operation_canceled")
	case allLeavesAre(context.DeadlineExceeded):
		return failure("deadline_exceeded")
	case allLeavesAre(session.ErrSessionUnknown):
		return failure("not_found_or_forbidden")
	case allLeavesAre(session.ErrSessionExists):
		return failure("session_exists")
	case allLeavesAre(session.ErrUpdateInProgress):
		return failure("update_in_progress")
	case allLeavesAre(session.ErrSessionRunning):
		return failure("session_running")
	case allLeavesAre(session.ErrStaleGeneration):
		return failure("stale_generation")
	case allLeavesAre(session.ErrInputConflict):
		return failure("input_id_conflict")
	case allLeavesAre(session.ErrInputUnknown):
		return failure("input_unknown")
	case allLeavesAre(session.ErrResourceLimit), allLeavesAre(session.ErrInputJournalFull):
		return failure("resource_limit")
	case allLeavesAre(configapply.ErrRevisionConflict):
		return failure("config_revision_conflict")
	case allLeavesAre(configapply.ErrInvalidRequest):
		return failure("invalid_request")
	default:
		return failure("unavailable")
	}
}
