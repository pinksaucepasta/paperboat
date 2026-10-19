package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func (s *Server) startRequest(frame protocol.Frame, writer *lockedWriter, authorizer Authorizer, connectionCtx context.Context, conn Connection, terminalState *terminalConnectionState) {
	select {
	case s.sem <- struct{}{}:
	default:
		s.recordOperation("protocol", "rejected")
		_ = writer.write(errorFrame(frame.RequestID, "resource_limit", "operation concurrency limit reached", true))
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.sem }()
		deadline := time.Duration(frame.DeadlineMS) * time.Millisecond
		if deadline > s.config.MutationDeadline {
			deadline = s.config.MutationDeadline
		}
		operationParent := s.ctx
		if frame.Capability == "config.compare.v1" {
			operationParent = connectionCtx
		}
		opCtx, cancel := context.WithTimeout(operationParent, deadline)
		authorization, err := authorizer.Authorize(opCtx, frame)
		if err != nil {
			if reportAuthorizationFailure(opCtx, err) {
				_ = writer.write(errorFrame(frame.RequestID, "unavailable", "authorization is temporarily unavailable", true))
			} else {
				_ = writer.write(errorFrame(frame.RequestID, "not_found_or_forbidden", "resource was not found or is not available", false))
			}
			cancel()
			return
		}
		if authorization.JournalBinding == "" {
			cancel()
			s.recordOperation("auth", "rejected")
			_ = writer.write(errorFrame(frame.RequestID, "not_found_or_forbidden", "resource was not found or is not available", false))
			return
		}
		journalOperationID := requestJournalOperationID(frame)
		activeKey := operationKey(authorization.JournalBinding, journalOperationID)
		owner, ok := s.registerOperation(activeKey, cancel)
		if !ok {
			cancel()
			_ = writer.write(errorFrame(frame.RequestID, "unavailable", "server is stopping", true))
			return
		}
		defer func() {
			if owner {
				s.unregisterOperation(activeKey)
			}
			cancel()
		}()
		journalRequest, err := json.Marshal(struct {
			Binding    string          `json:"binding"`
			Capability string          `json:"capability"`
			Payload    json.RawMessage `json:"payload"`
		}{Binding: authorization.JournalBinding, Capability: frame.Capability, Payload: frame.Payload})
		if err != nil {
			_ = writer.write(errorFrame(frame.RequestID, "invalid_request", "invalid operation", false))
			return
		}
		execute := func(ctx context.Context) operation.Outcome {
			if handler, ok := s.config.Handler.(OperationHandler); ok {
				return handler.HandleOperation(ctx, authorization, frame.Capability, frame.OperationID, frame.Payload)
			}
			return s.config.Handler.Handle(ctx, authorization, frame.Capability, frame.Payload)
		}
		var outcome operation.Outcome
		var replay bool
		if frame.Capability == "config.compare.v1" {
			outcome = execute(opCtx)
		} else {
			outcome, replay, err = s.config.Journal.Execute(opCtx, journalOperationID, journalRequest, execute)
		}
		if err != nil {
			code := operationErrorCode(err)
			s.recordOperation(componentForCapability(frame.Capability), metricResult(code, false))
			_ = writer.write(errorFrame(frame.RequestID, code, operationErrorMessage(code), errors.Is(err, context.DeadlineExceeded)))
			return
		}
		if outcome.ErrorCode != "" {
			s.recordOperation(componentForCapability(frame.Capability), metricResult(outcome.ErrorCode, false))
			_ = writer.write(errorFrameWithDetails(frame.RequestID, outcome.ErrorCode, "operation failed", false, outcome.Result))
			return
		}
		stream, hasStream, streamErr := s.openStream(connectionCtx, authorization, frame, outcome, replay)
		if streamErr != nil {
			reportStreamOpenFailure(connectionCtx, frame.Capability, streamErr)
			_ = writer.write(errorFrame(frame.RequestID, "unavailable", "output stream could not be opened", true))
			return
		}
		s.recordOperation(componentForCapability(frame.Capability), metricResult("", replay))
		var streamID uint32
		if hasStream {
			streamID, streamErr = terminalState.bind(authorization, frame, outcome)
			if streamErr != nil {
				reportStreamCloseFailure(connectionCtx, frame.Capability, stream.Close())
				_ = writer.write(errorFrame(frame.RequestID, "invalid_request", "terminal stream could not be bound", false))
				return
			}
			outcome.Result, streamErr = addStreamID(outcome.Result, streamID)
			if streamErr != nil {
				terminalState.remove(streamID)
				reportStreamCloseFailure(connectionCtx, frame.Capability, stream.Close())
				_ = writer.write(errorFrame(frame.RequestID, "unavailable", "terminal stream could not be opened", true))
				return
			}
		}
		responsePayload, _ := json.Marshal(struct {
			Result json.RawMessage `json:"result"`
			Replay bool            `json:"replay"`
		}{Result: outcome.Result, Replay: replay})
		if err := writer.write(protocol.Frame{Type: "response", RequestID: frame.RequestID, Version: protocol.ProtocolVersion, OperationID: frame.OperationID, Payload: responsePayload}); err != nil {
			if hasStream {
				terminalState.remove(streamID)
				reportStreamCloseFailure(connectionCtx, frame.Capability, stream.Close())
			}
			return
		}
		if hasStream {
			s.wg.Add(1)
			go s.stream(connectionCtx, writer, conn, stream, terminalState, streamID, frame.Capability)
		}
	}()
}

func requestJournalOperationID(frame protocol.Frame) string {
	if frame.Capability != "exec.v1" || frame.RequestID == "" {
		return frame.OperationID
	}
	var request struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(frame.Payload, &request) != nil || request.Action == "start" || request.Action == "" {
		return frame.OperationID
	}
	digest := sha256.Sum256([]byte(frame.OperationID + "\x00" + frame.RequestID))
	return "op_exec_control_" + hex.EncodeToString(digest[:16])
}

func (s *Server) stream(ctx context.Context, writer *lockedWriter, conn Connection, stream OutputStream, state *terminalConnectionState, streamID uint32, capability string) {
	defer s.wg.Done()
	defer state.remove(streamID)
	var streamFailure error
	failureStage := "delivery"
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			if streamFailure == nil {
				streamFailure = closeErr
				failureStage = "lifecycle"
			} else {
				streamFailure = errors.Join(streamFailure, closeErr)
			}
		}
		reportStreamFailure(ctx, capability, failureStage, streamFailure)
	}()
	for {
		frame, err := stream.Next(ctx)
		if err != nil {
			var streamEnd *StreamEnd
			if errors.As(err, &streamEnd) {
				if capability == "config.compare.v1" {
					streamEnd.Payload, _ = addStreamID(streamEnd.Payload, streamID)
				}
				_ = writer.write(protocol.Frame{Type: "event", RequestID: "stream", Version: protocol.ProtocolVersion, Capability: capability, Payload: streamEnd.Payload})
				return
			}
			var streamError *StreamError
			if errors.As(err, &streamError) {
				if streamError.Cause != nil {
					streamFailure = err
				}
				_ = writer.write(errorFrameWithDetails("stream", streamError.Code, "output stream closed", false, streamError.Details))
				if closer, ok := conn.(ProtocolCloser); ok {
					_ = closer.CloseProtocol(streamError.CloseCode, streamError.Code)
				} else {
					_ = conn.Close()
				}
				return
			}
			streamFailure = err
			if ctx.Err() == nil && !errors.Is(err, ErrStreamClosed) {
				_ = conn.Close()
			}
			return
		}
		started := time.Now()
		writeErr := writer.writeTerminalOutput(streamID, frame)
		if frame.Release != nil {
			frame.Release()
		}
		if writeErr != nil {
			streamFailure = writeErr
			_ = conn.Close()
			return
		}
		if metrics, ok := s.config.Metrics.(terminalMetricRecorder); ok {
			metrics.RecordTerminalStage("pty_to_socket", time.Since(started), len(frame.Data))
		}
	}
}

type helloPayload struct {
	MinVersion   string   `json:"min_version"`
	MaxVersion   string   `json:"max_version"`
	Capabilities []string `json:"capabilities"`
}

func (p helloPayload) validate() error {
	if p.MinVersion == "" || p.MaxVersion == "" || len(p.Capabilities) > 64 {
		return ErrHandshakeRequired
	}
	seen := make(map[string]bool, len(p.Capabilities))
	for _, capability := range p.Capabilities {
		if seen[capability] {
			return ErrHandshakeRequired
		}
		seen[capability] = true
	}
	return nil
}

func (s *Server) recordOperation(component, result string) {
	if s.config.Metrics != nil {
		_ = s.config.Metrics.Record("paperboat_runtime_operations_total", 1, map[string]string{"component": component, "result": result})
	}
}

func componentForCapability(capability string) string {
	switch capability {
	case "terminal.v1":
		return "session"
	case "update.tuf.v1":
		return "update"
	default:
		return "protocol"
	}
}

func metricResult(code string, replay bool) string {
	if replay {
		return "replayed"
	}
	switch code {
	case "":
		return "ok"
	case "operation_id_conflict", "input_id_conflict":
		return "conflict"
	case "operation_canceled":
		return "canceled"
	case "deadline_exceeded":
		return "deadline"
	case "unavailable", "operation_uncertain", "storage_unavailable", "resource_limit":
		return "unavailable"
	default:
		return "rejected"
	}
}
