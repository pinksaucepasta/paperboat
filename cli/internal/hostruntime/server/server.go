package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

type Server struct {
	config Config
	ctx    context.Context
	cancel context.CancelFunc
	sem    chan struct{}

	mu      sync.Mutex
	stopped bool
	running map[string]context.CancelFunc
	conns   map[Connection]struct{}
	wg      sync.WaitGroup
}

func New(config Config) (*Server, error) {
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = helperconfig.DefaultResources.MaxConcurrentOps
	}
	if config.Journal == nil || config.Handler == nil || config.MaxConcurrent < 1 ||
		config.HeartbeatInterval <= 0 ||
		config.MutationDeadline <= 0 || config.MutationDeadline > 5*time.Minute {
		return nil, ErrInvalidConfiguration
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{config: config, ctx: ctx, cancel: cancel, sem: make(chan struct{}, config.MaxConcurrent), running: make(map[string]context.CancelFunc), conns: make(map[Connection]struct{})}, nil
}

func (s *Server) Start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return ErrServerStopped
	}
	return nil
}

func (s *Server) Serve(conn Connection) error {
	return s.ServeAuthenticated(conn, s.config.Authorizer)
}

// ServeAuthenticated binds one already-established transport credential to one
// connection. HTTP/WSS adapters should use this entry point rather than sharing
// credential state between connections.
func (s *Server) ServeAuthenticated(conn Connection, authorizer Authorizer) (serveErr error) {
	if conn == nil {
		return ErrInvalidConfiguration
	}
	if authorizer == nil {
		_ = conn.Close()
		return ErrInvalidConfiguration
	}
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		_ = conn.Close()
		return ErrServerStopped
	}
	s.conns[conn] = struct{}{}
	s.mu.Unlock()
	defer func() {
		if closer, ok := authorizer.(AuthorizationCloser); ok {
			closer.CloseAuthorization()
		}
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		if closer, ok := conn.(ProtocolCloser); ok {
			_ = closer.CloseProtocol(connectionCloseCode(serveErr), closeReason(serveErr))
		} else {
			_ = conn.Close()
		}
	}()

	writer := newLockedWriter(conn)
	defer writer.close()
	connectionCtx, cancelConnection := context.WithCancel(s.ctx)
	defer cancelConnection()
	first, binaryMessage, err := readApplication(conn)
	if err != nil {
		return fmt.Errorf("native application read hello: %w", err)
	}
	if len(binaryMessage) != 0 || first.Type != "hello" {
		_ = writer.write(errorFrame(first.RequestID, "protocol_incompatible", "hello required", false))
		if len(binaryMessage) != 0 {
			return &protocol.Error{Code: protocol.InvalidFrame, Cause: errors.New("hello must be a structured frame")}
		}
		return ErrHandshakeRequired
	}
	var hello helloPayload
	if err := decodePayload(first.Payload, &hello); err != nil {
		_ = writer.write(errorFrame(first.RequestID, "invalid_request", "invalid hello", false))
		return err
	}
	if err := hello.validate(); err != nil {
		_ = writer.write(errorFrame(first.RequestID, "invalid_request", "invalid hello", false))
		return err
	}
	welcome, err := s.config.Negotiator.Negotiate(hello.MinVersion, hello.MaxVersion, hello.Capabilities)
	if err != nil {
		_ = writer.write(errorFrame(first.RequestID, protocolCode(err), "protocol negotiation failed", false))
		return err
	}
	welcomeFields := map[string]any{"version": welcome.Version, "capabilities": welcome.Capabilities}
	if augmenter, ok := conn.(WelcomeAugmenter); ok {
		augmenter.AugmentWelcome(welcomeFields)
	}
	payload, _ := json.Marshal(welcomeFields)
	if err := writer.write(protocol.Frame{Type: "welcome", RequestID: first.RequestID, Version: protocol.ProtocolVersion, Payload: payload}); err != nil {
		return fmt.Errorf("native application write welcome: %w", err)
	}
	selected := make(map[string]bool, len(welcome.Capabilities))
	for _, capability := range welcome.Capabilities {
		selected[capability] = true
	}
	var revocationFlag *atomic.Bool
	if revocation, ok := conn.(ConnectionRevocation); ok {
		revocationFlag = revocation.RevocationFlag()
	}
	terminalState := newTerminalConnectionState(revocationFlag)
	terminalState.done = connectionCtx.Done()
	terminalState.ctx = connectionCtx

	frames := make(chan readResult, 1)
	readerDone := make(chan struct{})
	defer close(readerDone)
	go readFrames(conn, frames, readerDone)
	// One worker handles frames in arrival order, so a PTY write stuck behind a program that does not read never
	// stops this loop reading: heartbeats are still answered and an interrupt key can overtake the stuck input.
	work := make(chan readResult, connectionWorkQueue)
	failed := make(chan error, 1)
	var busy atomic.Bool
	defer close(work)
	go func() {
		for result := range work {
			busy.Store(true)
			err := s.serveFrame(result, connectionCtx, writer, authorizer, conn, terminalState, selected)
			busy.Store(false)
			if err != nil {
				failed <- err
				cancelConnection()
				return
			}
		}
	}()
	heartbeat := time.NewTicker(s.config.HeartbeatInterval)
	defer heartbeat.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return ErrServerStopped
		case <-terminalState.expired:
			return &protocol.Error{Code: protocol.CredentialExpired, Cause: errors.New("terminal credential expired")}
		case err := <-failed:
			return err
		case <-heartbeat.C:
			requestID := "heartbeat"
			if err := writer.write(protocol.Frame{Type: "heartbeat", RequestID: requestID, Version: protocol.ProtocolVersion}); err != nil {
				return err
			}
		case result := <-frames:
			if result.err != nil {
				return result.err
			}
			if len(result.binary) == 0 && result.frame.Type == "heartbeat" {
				// A probe (a client checking its connection on resume, with its own deadline) gets the
				// same heartbeat back at once, ahead of queued output. Other heartbeats, including a
				// client's echo of ours, need no answer.
				if strings.HasPrefix(result.frame.RequestID, "probe_") {
					if err := writer.write(protocol.Frame{Type: "heartbeat", RequestID: result.frame.RequestID, Version: protocol.ProtocolVersion}); err != nil {
						return err
					}
				}
				continue
			}
			if opcode, err := protocol.TerminalOpcode(result.binary); len(result.binary) != 0 && err == nil && opcode == protocol.TerminalACKOpcode {
				// Output acknowledgements never wait behind input: under a flood into a program that does not read,
				// a stuck write would otherwise stall the output too.
				if err := s.handleTerminalDataWithWriter(connectionCtx, result.binary, terminalState, writer); err != nil {
					return err
				}
				continue
			}
			if len(result.binary) != 0 && busy.Load() {
				s.interruptTerminalInput(connectionCtx, result.binary, terminalState)
			}
			select {
			case work <- result:
			case err := <-failed:
				return err
			case <-s.ctx.Done():
				return ErrServerStopped
			}
		}
	}
}

// connectionWorkQueue holds more frames than a client leaves unanswered (256 input chunks), so only a client that
// ignores its own window fills it; the read loop then waits for the worker, as it always did.
const connectionWorkQueue = 512

// serveFrame handles one frame of a connection, in arrival order.
func (s *Server) serveFrame(result readResult, connectionCtx context.Context, writer *lockedWriter, authorizer Authorizer, conn Connection, terminalState *terminalConnectionState, selected map[string]bool) error {
	if len(result.binary) != 0 {
		return s.handleTerminalDataWithWriter(connectionCtx, result.binary, terminalState, writer)
	}
	switch result.frame.Type {
	case "ack", "detach":
		s.handleControl(result.frame, writer, authorizer)
	case "cancel":
		cancelCtx, cancel := context.WithTimeout(s.ctx, min(s.config.MutationDeadline, 5*time.Second))
		authorization, authErr := authorizer.Authorize(cancelCtx, result.frame)
		authorityUnavailable := reportAuthorizationFailure(cancelCtx, authErr)
		cancel()
		if authErr != nil {
			if authorityUnavailable {
				_ = writer.write(errorFrame(result.frame.RequestID, "unavailable", "authorization is temporarily unavailable", true))
			} else {
				_ = writer.write(errorFrame(result.frame.RequestID, "not_found_or_forbidden", "operation was not found or is not available", false))
			}
			return nil
		}
		if authorization.JournalBinding == "" {
			_ = writer.write(errorFrame(result.frame.RequestID, "not_found_or_forbidden", "operation was not found or is not available", false))
			return nil
		}
		s.cancelOperation(operationKey(authorization.JournalBinding, result.frame.OperationID))
		_ = writer.write(protocol.Frame{Type: "response", RequestID: result.frame.RequestID, Version: protocol.ProtocolVersion, OperationID: result.frame.OperationID, Payload: json.RawMessage(`{"canceled":true}`)})
	case "request":
		if !selected[result.frame.Capability] {
			_ = writer.write(errorFrame(result.frame.RequestID, "capability_required", "capability was not negotiated", false))
			return nil
		}
		s.startRequest(result.frame, writer, authorizer, connectionCtx, conn, terminalState)
	default:
		_ = writer.write(errorFrame(result.frame.RequestID, "invalid_request", "unexpected frame", false))
	}
	return nil
}

func (s *Server) handleControl(frame protocol.Frame, writer *lockedWriter, authorizer Authorizer) {
	handler, ok := s.config.Handler.(ControlHandler)
	if !ok {
		_ = writer.write(errorFrame(frame.RequestID, "invalid_request", "unsupported control frame", false))
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, min(s.config.MutationDeadline, 5*time.Second))
	defer cancel()
	authorization, err := authorizer.Authorize(ctx, frame)
	if err != nil {
		if reportAuthorizationFailure(ctx, err) {
			_ = writer.write(errorFrame(frame.RequestID, "unavailable", "authorization is temporarily unavailable", true))
		} else {
			_ = writer.write(errorFrame(frame.RequestID, "not_found_or_forbidden", "resource was not found or is not available", false))
		}
		return
	}
	if authorization.JournalBinding == "" {
		_ = writer.write(errorFrame(frame.RequestID, "not_found_or_forbidden", "resource was not found or is not available", false))
		return
	}
	outcome := handler.HandleControl(ctx, authorization, frame)
	if outcome.ErrorCode != "" {
		_ = writer.write(errorFrame(frame.RequestID, outcome.ErrorCode, "attachment control failed", false))
		return
	}
	_ = writer.write(protocol.Frame{Type: "response", RequestID: frame.RequestID, Version: protocol.ProtocolVersion, Payload: outcome.Result})
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		s.cancel()
		for conn := range s.conns {
			_ = conn.Close()
		}
		for _, cancel := range s.running {
			cancel()
		}
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) openStream(ctx context.Context, authorization Authorization, frame protocol.Frame, outcome operation.Outcome, replay bool) (OutputStream, bool, error) {
	handler, ok := s.config.Handler.(StreamHandler)
	if !ok {
		return nil, false, nil
	}
	return handler.OpenStream(ctx, authorization, frame.Capability, frame.Payload, outcome, replay)
}

func (s *Server) registerOperation(id string, cancel context.CancelFunc) (owner, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false, false
	}
	if s.running[id] != nil {
		return false, true
	}
	s.running[id] = cancel
	return true, true
}

func (s *Server) unregisterOperation(id string) {
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
}

func (s *Server) cancelOperation(id string) {
	s.mu.Lock()
	cancel := s.running[id]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func operationKey(binding, operationID string) string { return binding + "\x00" + operationID }
