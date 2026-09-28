package datacarrier

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/connectorprotocol"
)

// acceptDataStream uses the carrier's single data-stream accept loop so
// independent edge services cannot steal one another's host-initiated streams.
func (s *Server) acceptDataStream(ctx context.Context, terminalOutput bool) (*Stream, StreamOpen, error) {
	if s == nil || ctx == nil {
		return nil, StreamOpen{}, ErrInvalidConfig
	}
	s.dataAcceptOnce.Do(func() { go s.acceptDataStreams() })
	queue := s.dataAccess
	if terminalOutput {
		queue = s.dataTerminal
	}
	for {
		select {
		case <-ctx.Done():
			return nil, StreamOpen{}, ctx.Err()
		case <-s.done:
			return nil, StreamOpen{}, ErrCarrierClosed
		case result := <-queue:
			if result.err != nil {
				return nil, StreamOpen{}, result.err
			}
			return result.stream, result.open, nil
		}
	}
}

func (s *Server) acceptDataStreams() {
	for {
		raw, err := s.acceptInboundRaw(s.ctx)
		if err != nil {
			if errors.Is(err, ErrStreamLimit) {
				continue
			}
			if !s.closed() && s.ctx.Err() == nil {
				s.shutdown(err)
			}
			return
		}
		result := s.readInboundDataStream(raw)
		if result.open.Kind == "" {
			// An unparseable preface has no service owner. Close and discard it
			// without interrupting either valid stream consumer.
			continue
		}
		queue := s.dataAccess
		if result.open.Kind == BrowserTerminalOutputStream {
			queue = s.dataTerminal
		}
		select {
		case queue <- result:
		case <-s.done:
			if result.stream != nil {
				_ = result.stream.Close()
			}
			return
		default:
			if result.stream != nil {
				_ = result.stream.Close()
			}
		}
	}
}

func (s *Server) readInboundDataStream(raw StreamLink) acceptResult {
	deadline := time.Now().Add(s.config.StreamOpenLimit)
	if contextDeadline, ok := s.ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := raw.SetReadDeadline(deadline); err != nil {
		_ = raw.Close()
		s.releasePermit()
		return acceptResult{err: ErrInvalidPreface}
	}
	open, err := connectorprotocol.ReadStreamOpen(raw)
	_ = raw.SetReadDeadline(time.Time{})
	if err == nil && !s.config.Identity.matches(open) {
		err = ErrAccessStreamIdentity
	}
	if err == nil {
		admissionContext, cancel := context.WithTimeout(s.ctx, s.config.StreamOpenLimit)
		err = s.config.Authorize.AuthorizeStream(admissionContext, s.config.Identity, open)
		cancel()
		if err != nil {
			err = errors.Join(ErrRouteDenied, err)
		}
	}
	if err != nil {
		_ = raw.Close()
		s.releasePermit()
		return acceptResult{open: open, err: err}
	}
	return acceptResult{stream: wrapStream(raw, context.Background(), s.releasePermit, open), open: open}
}
