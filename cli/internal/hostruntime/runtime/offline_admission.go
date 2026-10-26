package runtime

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// offlineAdmission records every offline session decision before the host acts on it (F21-11):
// a start that cannot be recorded is refused, so audit fails closed. A multi-account host
// dispatches streams to per-account workers that hold no peer state of their own here, so it
// refuses offline sessions.
func offlineAdmission(queue *offlineaudit.Queue, dispatching bool) func(context.Context, native.OfflineAdmission) error {
	if queue == nil {
		return nil
	}
	return func(_ context.Context, request native.OfflineAdmission) error {
		event := offlineaudit.Event{Kind: offlineaudit.KindStarted, Capability: request.Capability, PeerEndpointID: request.Peer.EndpointID, PeerAccountID: request.Peer.AccountID, OperationID: request.Header.OperationID}
		var refusal error
		switch {
		case dispatching:
			refusal, event.Reason = errors.New("this host does not start offline sessions"), offlineaudit.ReasonPeerUnknown
		case !request.Allowed:
			refusal, event.Reason = errors.New("no signed scope allows this session"), offlineaudit.ReasonNoScope
		}
		if refusal != nil {
			event.Kind = offlineaudit.KindRefused
		}
		err := queue.Record(event)
		if errors.Is(err, offlineaudit.ErrFull) && refusal == nil {
			// The refusal itself cannot be recorded either; the session stays refused.
			return err
		}
		return errors.Join(refusal, err)
	}
}

// offlineAuditService uploads the offline session backlog whenever events are queued.
type offlineAuditService struct {
	uploader *offlineaudit.Uploader
	cancel   context.CancelFunc
	done     chan struct{}
}

func (s *offlineAuditService) Start(context.Context) error {
	if s.cancel != nil {
		return ErrProductionInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() { defer close(s.done); s.uploader.Run(ctx) }()
	return nil
}

func (s *offlineAuditService) Shutdown(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
