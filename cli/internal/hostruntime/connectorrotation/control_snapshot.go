package connectorrotation

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/connectorprotocol"
)

type controlReadResult struct {
	frame connectorprotocol.Frame
	err   error
}

func readControlFrames(ctx context.Context, carrier io.Reader, results chan<- controlReadResult) {
	for {
		frame, err := connectorprotocol.ReadFrame(carrier)
		select {
		case results <- controlReadResult{frame: frame, err: err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

type snapshotReadyState struct {
	snapshot  connectorprotocol.Snapshot
	requestID string
	cancel    context.CancelFunc
	done      chan struct{}
}

type snapshotReadyResult struct {
	snapshot  connectorprotocol.Snapshot
	requestID string
	readiness connectorprotocol.Readiness
	err       error
}

func (s *snapshotReadyState) matches(result snapshotReadyResult) bool {
	return s != nil && s.requestID == result.requestID && s.snapshot.AccountID == result.snapshot.AccountID && s.snapshot.TunnelID == result.snapshot.TunnelID && s.snapshot.ConnectorID == result.snapshot.ConnectorID && s.snapshot.SessionID == result.snapshot.SessionID && s.snapshot.ProcessGeneration == result.snapshot.ProcessGeneration && s.snapshot.Generation == result.snapshot.Generation && s.snapshot.ContentHash == result.snapshot.ContentHash
}

func (s *ControlSession) waitSnapshotReady(ctx context.Context, state *snapshotReadyState, results chan<- snapshotReadyResult) {
	defer close(state.done)
	readiness, err := s.snapshotReadiness.WaitReady(ctx, state.snapshot)
	result := snapshotReadyResult{snapshot: state.snapshot, requestID: state.requestID, readiness: readiness, err: err}
	select {
	case results <- result:
	case <-ctx.Done():
	}
}

// stopSnapshotReadiness cancels an in-flight readiness probe and waits only a
// bounded interval for a cooperative provider to release its resources. The
// Serve loop never waits for a replacement probe while it is live, so a
// provider that fails to honor context cancellation cannot block heartbeats or
// reconnect cleanup indefinitely.
func stopSnapshotReadiness(state *snapshotReadyState) {
	if state == nil {
		return
	}
	if state.cancel != nil {
		state.cancel()
	}
	if state.done == nil {
		return
	}
	timer := time.NewTimer(readinessCleanupTimeout)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-state.done:
	case <-timer.C:
	}
}

func resetControlTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func credentialRenewalDelay(now, expiresAt time.Time, lead time.Duration) time.Duration {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return time.Millisecond
	}
	delay := remaining - lead
	if delay <= 0 {
		delay = remaining / 2
	}
	if delay < time.Millisecond {
		return time.Millisecond
	}
	return delay
}

func appliedSnapshot(frame connectorprotocol.Frame, responses []connectorprotocol.Frame) (connectorprotocol.Snapshot, bool) {
	if frame.Type != connectorprotocol.MessageSnapshot && frame.Type != connectorprotocol.MessageDelta {
		return connectorprotocol.Snapshot{}, false
	}
	wantedKind := connectorprotocol.AckSnapshot
	if frame.Type == connectorprotocol.MessageDelta {
		wantedKind = connectorprotocol.AckDelta
	}
	for _, response := range responses {
		if response.Type != connectorprotocol.MessageAck {
			continue
		}
		var ack connectorprotocol.Ack
		if response.DecodePayload(&ack) != nil {
			return connectorprotocol.Snapshot{}, false
		}
		if ack.Kind != wantedKind || ack.Status != connectorprotocol.AckApplied {
			return connectorprotocol.Snapshot{}, false
		}
		if frame.Type == connectorprotocol.MessageSnapshot {
			var snapshot connectorprotocol.Snapshot
			if frame.DecodePayload(&snapshot) != nil {
				return connectorprotocol.Snapshot{}, false
			}
			return snapshot, true
		}
		var delta connectorprotocol.Delta
		if frame.DecodePayload(&delta) != nil {
			return connectorprotocol.Snapshot{}, false
		}
		return connectorprotocol.Snapshot{AccountID: delta.AccountID, TunnelID: delta.TunnelID, ConnectorID: delta.ConnectorID, SessionID: delta.SessionID, ProcessGeneration: delta.ProcessGeneration, Generation: delta.Generation, ContentHash: delta.ContentHash, Payload: append([]byte(nil), delta.Payload...)}, true
	}
	return connectorprotocol.Snapshot{}, false
}

func validateSnapshotReadiness(snapshot connectorprotocol.Snapshot, readiness connectorprotocol.Readiness) error {
	if err := readiness.Validate(); err != nil || !readiness.EdgeReady || !readiness.RouteReady || !readiness.OriginReady || readiness.AccountID != snapshot.AccountID || readiness.TunnelID != snapshot.TunnelID || readiness.ConnectorID != snapshot.ConnectorID || readiness.SessionID != snapshot.SessionID || readiness.ProcessGeneration != snapshot.ProcessGeneration || readiness.Generation != snapshot.Generation || readiness.ContentHash != snapshot.ContentHash {
		return &connectorprotocol.Error{Code: connectorprotocol.CodeNotReady, Reason: connectorprotocol.ReasonSnapshotRejected, Retryable: true, Cause: errors.New("snapshot readiness is not bound to the staged snapshot")}
	}
	return nil
}
