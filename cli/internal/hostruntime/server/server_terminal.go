package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/relay/admission"
)

func (s *Server) handleTerminalData(ctx context.Context, message []byte, state *terminalConnectionState) error {
	return s.handleTerminalDataWithWriter(ctx, message, state, nil)
}

func (s *Server) handleTerminalDataWithWriter(ctx context.Context, message []byte, state *terminalConnectionState, writer *lockedWriter) error {
	opcode, err := protocol.TerminalOpcode(message)
	if err != nil {
		return err
	}
	var streamID uint32
	switch opcode {
	case protocol.TerminalInputOpcode:
		started := time.Now()
		frame, decodeErr := protocol.DecodeTerminalInput(message)
		if decodeErr != nil {
			return decodeErr
		}
		streamID = frame.StreamID
		binding, bindErr := state.binding(streamID)
		if bindErr != nil {
			if errors.Is(bindErr, errTerminalStreamClosed) {
				return nil
			}
			return bindErr
		}
		duplicateInput := frame.Sequence == binding.inputSequence
		if duplicateInput {
			if binding.kind == "exec" || binding.kind == "ssh" {
				return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("stream input sequence is not contiguous")}
			}
			if _, rich := s.config.Handler.(TerminalInputHandler); !rich {
				return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("terminal input sequence is not contiguous")}
			}
		}
		if frame.Sequence != binding.inputSequence+1 && !duplicateInput {
			return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("terminal input sequence is not contiguous")}
		}
		if err := validateTerminalBinding(binding); err != nil {
			return err
		}
		advanceInput := binding.kind == "exec" || binding.kind == "ssh"
		if binding.kind == "exec" {
			handler, ok := s.config.Handler.(ExecDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleExecInput(ctx, binding.authorization, binding.operationID, frame.Data); err != nil {
				return err
			}
		} else if binding.kind == "ssh" {
			handler, ok := s.config.Handler.(SSHDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleSSHInput(ctx, binding.authorization, binding.operationID, frame.Data); err != nil {
				return err
			}
		} else {
			var decision session.InputDecision
			var inputErr error
			if handler, ok := s.config.Handler.(TerminalInputHandler); ok {
				decision, inputErr = handler.HandleTerminalInput(ctx, binding.authorization, binding.sessionID, binding.attachmentID, binding.generation, frame.Sequence, frame.Data)
			} else if handler, ok := s.config.Handler.(legacyTerminalInputHandler); ok {
				inputErr = handler.HandleTerminalInput(ctx, binding.authorization, binding.sessionID, binding.attachmentID, binding.generation, frame.Data)
				decision = session.InputDecision{Status: session.InputAccepted, InputSequence: frame.Sequence, BytesWritten: len(frame.Data)}
			} else {
				return ErrCapabilityUnavailable
			}
			if inputErr != nil {
				if decision.Status == "" || decision.Status != session.InputUncertain {
					decision = inputDecisionForError(inputErr, frame.Sequence)
				} else {
					decision.InputSequence = frame.Sequence
					if decision.WriteError == "" {
						decision.WriteError = "input_uncertain"
					}
				}
			}
			if decision.Status == "" {
				decision = inputDecisionForError(errors.New("empty input decision"), frame.Sequence)
			}
			if writer != nil {
				if err := writeTerminalInputResult(writer, frame.StreamID, decision, inputErr); err != nil {
					return err
				}
			}
			if inputErr != nil && !errors.Is(inputErr, session.ErrInputConflict) && !errors.Is(inputErr, session.ErrInputSequence) && !errors.Is(inputErr, session.ErrInputUncertain) && !errors.Is(inputErr, session.ErrStaleGeneration) && !errors.Is(inputErr, session.ErrInvalidInput) {
				return inputErr
			}
			advanceInput = inputErr == nil
		}
		if metrics, ok := s.config.Metrics.(terminalMetricRecorder); ok {
			metrics.RecordTerminalStage("socket_to_pty", time.Since(started), len(frame.Data))
		}
		if advanceInput {
			state.mu.Lock()
			if current := state.streams[streamID]; current == binding && frame.Sequence > current.inputSequence {
				current.inputSequence = frame.Sequence
			}
			state.mu.Unlock()
		}
		return nil
	case protocol.TerminalEOFOpcode:
		frame, decodeErr := protocol.DecodeTerminalEOF(message)
		if decodeErr != nil {
			return decodeErr
		}
		streamID = frame.StreamID
		binding, bindErr := state.binding(streamID)
		if bindErr != nil {
			if errors.Is(bindErr, errTerminalStreamClosed) {
				return nil
			}
			return bindErr
		}
		if frame.Sequence != binding.inputSequence+1 || binding.kind != "ssh" && binding.kind != "exec" {
			return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("input EOF sequence is not contiguous")}
		}
		if err := validateTerminalBinding(binding); err != nil {
			return err
		}
		if binding.kind == "exec" {
			handler, ok := s.config.Handler.(ExecDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleExecEOF(ctx, binding.authorization, binding.operationID); err != nil {
				return err
			}
		} else {
			handler, ok := s.config.Handler.(SSHDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleSSHEOF(ctx, binding.authorization, binding.operationID); err != nil {
				return err
			}
		}
		state.mu.Lock()
		if current := state.streams[streamID]; current == binding {
			current.inputSequence = frame.Sequence
		}
		state.mu.Unlock()
		return nil
	case protocol.TerminalACKOpcode:
		frame, decodeErr := protocol.DecodeTerminalACK(message)
		if decodeErr != nil {
			return decodeErr
		}
		binding, bindErr := state.binding(frame.StreamID)
		if bindErr != nil {
			if errors.Is(bindErr, errTerminalStreamClosed) {
				return nil
			}
			return bindErr
		}
		if err := validateTerminalBinding(binding); err != nil {
			return err
		}
		if binding.kind == "exec" {
			return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("exec output does not use terminal acknowledgements")}
		}
		handler, ok := s.config.Handler.(TerminalDataHandler)
		if !ok {
			return ErrCapabilityUnavailable
		}
		return handler.HandleTerminalACK(ctx, binding.authorization, binding.sessionID, binding.attachmentID, frame.NextSequence)
	case protocol.TerminalResizeOpcode:
		frame, decodeErr := protocol.DecodeTerminalResize(message)
		if decodeErr != nil {
			return decodeErr
		}
		streamID = frame.StreamID
		binding, bindErr := state.binding(streamID)
		if bindErr != nil {
			if errors.Is(bindErr, errTerminalStreamClosed) {
				return nil
			}
			return bindErr
		}
		if frame.Sequence != binding.resizeSequence+1 {
			return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("terminal resize sequence is not contiguous")}
		}
		if err := validateTerminalBinding(binding); err != nil {
			return err
		}
		if binding.kind == "exec" {
			handler, ok := s.config.Handler.(ExecDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleExecResize(ctx, binding.authorization, binding.operationID, frame.Columns, frame.Rows); err != nil {
				return err
			}
		} else {
			handler, ok := s.config.Handler.(TerminalDataHandler)
			if !ok {
				return ErrCapabilityUnavailable
			}
			if err := handler.HandleTerminalResize(ctx, binding.authorization, binding.sessionID, binding.attachmentID, frame.Columns, frame.Rows); err != nil {
				return err
			}
		}
		state.mu.Lock()
		if current := state.streams[streamID]; current == binding {
			current.resizeSequence = frame.Sequence
		}
		state.mu.Unlock()
		return nil
	default:
		return &protocol.Error{Code: protocol.InvalidFrame}
	}
}

type terminalInputResult struct {
	Event        string              `json:"event"`
	StreamID     uint32              `json:"stream_id"`
	Sequence     uint64              `json:"sequence"`
	Status       session.InputStatus `json:"status"`
	BytesWritten int                 `json:"bytes_written"`
	ErrorCode    string              `json:"error_code,omitempty"`
}

func inputDecisionForError(err error, sequence uint64) session.InputDecision {
	status := session.InputRejected
	code := "input_rejected"
	switch {
	case errors.Is(err, session.ErrInputConflict):
		status, code = session.InputStatus("conflict"), "input_conflict"
	case errors.Is(err, session.ErrInputUncertain):
		status, code = session.InputUncertain, "input_uncertain"
	case errors.Is(err, session.ErrInputSequence):
		status, code = session.InputRejected, "input_sequence"
	case errors.Is(err, session.ErrStaleGeneration):
		status, code = session.InputRejected, "stale_generation"
	case errors.Is(err, session.ErrInvalidInput):
		status, code = session.InputRejected, "invalid_input"
	}
	return session.InputDecision{Status: status, InputSequence: sequence, WriteError: code}
}

func writeTerminalInputResult(writer *lockedWriter, streamID uint32, decision session.InputDecision, inputErr error) error {
	if writer == nil || streamID == 0 {
		return nil
	}
	errorCode := decision.WriteError
	if errorCode == "" && inputErr != nil {
		errorCode = "input_rejected"
	}
	payload, err := json.Marshal(terminalInputResult{Event: "terminal_input_result", StreamID: streamID, Sequence: decision.InputSequence, Status: decision.Status, BytesWritten: decision.BytesWritten, ErrorCode: errorCode})
	if err != nil {
		return err
	}
	return writer.write(protocol.Frame{Type: "event", RequestID: fmt.Sprintf("input_%d_%d", streamID, decision.InputSequence), Version: protocol.ProtocolVersion, Payload: payload})
}

func validateTerminalBinding(binding *terminalStreamBinding) error {
	if binding == nil || binding.authorization.ClientID == "" || (binding.kind == "" || binding.kind == "terminal") && (binding.sessionID == "" || binding.attachmentID == "" || binding.generation == 0) || (binding.kind == "exec" || binding.kind == "ssh") && binding.operationID == "" || binding.kind != "" && binding.kind != "terminal" && binding.kind != "exec" && binding.kind != "ssh" {
		return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("terminal stream is not bound")}
	}
	if binding.revoked != nil && binding.revoked.Load() {
		return &protocol.Error{Code: protocol.CredentialExpired, Cause: errors.New("terminal credential revoked")}
	}
	return nil
}

func (s *terminalConnectionState) binding(streamID uint32) (*terminalStreamBinding, error) {
	if streamID == 0 {
		return nil, &protocol.Error{Code: protocol.InvalidFrame}
	}
	s.mu.RLock()
	binding := s.streams[streamID]
	_, closed := s.closed[streamID]
	s.mu.RUnlock()
	if binding == nil {
		if closed {
			return nil, errTerminalStreamClosed
		}
		return nil, &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("unknown terminal stream")}
	}
	return binding, nil
}

func (s *terminalConnectionState) bind(authorization Authorization, frame protocol.Frame, outcome operation.Outcome) (uint32, error) {
	var request terminalRequest
	var response terminalAttachResponse
	binding := &terminalStreamBinding{authorization: authorization}
	if frame.Capability == "terminal.v1" && decodeStrict(frame.Payload, &request) == nil && request.Action == "attach" && json.Unmarshal(outcome.Result, &response) == nil && request.SessionID != "" && response.AttachmentID != "" && response.Session.Snapshot.Generation != 0 {
		binding.kind, binding.sessionID, binding.attachmentID, binding.generation, binding.inputSequence = "terminal", request.SessionID, response.AttachmentID, response.Session.Snapshot.Generation, response.InputSequence
	} else if frame.Capability == "config.compare.v1" {
		binding.kind = "config_compare"
	} else if frame.Capability == "exec.v1" {
		var execRequest execRequest
		if decodeStrict(frame.Payload, &execRequest) != nil || execRequest.OperationID == "" || execRequest.Action != "start" && execRequest.Action != "attach" {
			return 0, errors.New("invalid exec stream binding")
		}
		binding.kind, binding.operationID = "exec", execRequest.OperationID
	} else if frame.Capability == "ssh.v1" {
		var sshRequest sshRequest
		if decodeStrict(frame.Payload, &sshRequest) != nil || sshRequest.OperationID == "" || sshRequest.Generation == 0 {
			return 0, errors.New("invalid SSH stream binding")
		}
		binding.kind, binding.operationID = "ssh", sshRequest.OperationID
	} else {
		return 0, errors.New("invalid stream binding")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempts := uint64(0); attempts < uint64(^uint32(0)); attempts++ {
		s.nextID++
		if s.nextID == 0 {
			s.nextID++
		}
		if s.streams[s.nextID] != nil {
			continue
		}
		streamID := s.nextID
		revoked := authorization.Revoked
		if revoked == nil {
			revoked = s.revoked
		}
		binding.revoked = revoked
		s.streams[streamID] = binding
		parent := s.ctx
		if parent == nil {
			parent = context.Background()
		}
		lease, err := admission.Admit(parent, authorization.ExpiresAt, authorization.RevokedSignal)
		if err != nil {
			delete(s.streams, streamID)
			return 0, &protocol.Error{Code: protocol.CredentialExpired, Cause: err}
		}
		binding.lease = lease
		binding.stop = lease.Bind(s)
		return streamID, nil
	}
	return 0, errors.New("terminal stream id space exhausted")
}

func (s *terminalConnectionState) Close() error {
	s.expireOnce.Do(func() { close(s.expired) })
	return nil
}

func (s *terminalConnectionState) remove(streamID uint32) {
	s.mu.Lock()
	if b := s.streams[streamID]; b != nil && b.lease != nil {
		b.stop()
		b.lease.Close()
	}
	delete(s.streams, streamID)
	if _, exists := s.closed[streamID]; !exists {
		s.closed[streamID] = struct{}{}
		s.closedIDs = append(s.closedIDs, streamID)
		if len(s.closedIDs) > 1024 {
			oldest := s.closedIDs[0]
			s.closedIDs = s.closedIDs[1:]
			delete(s.closed, oldest)
		}
	}
	s.mu.Unlock()
}

func addStreamID(result json.RawMessage, streamID uint32) (json.RawMessage, error) {
	var value map[string]any
	if streamID == 0 || json.Unmarshal(result, &value) != nil || value == nil {
		return nil, errors.New("invalid terminal attachment response")
	}
	value["stream_id"] = streamID
	return json.Marshal(value)
}
