package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
)

// offlineAdmission records every offline session decision before the host acts on it (F21-11):
// a start that cannot be recorded is refused, so audit fails closed. A multi-account host
// dispatches streams to per-account workers that hold no peer state of their own here, so it
// refuses offline sessions, except SSH: the carrier serves SSH itself under its own signed rules.
func offlineAdmission(queue *offlineaudit.Queue, dispatching bool) func(context.Context, native.OfflineAdmission) error {
	if queue == nil {
		return nil
	}
	return func(_ context.Context, request native.OfflineAdmission) error {
		if request.Capability == sshCapability {
			return admitOfflineSSH(queue, request)
		}
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

// newOfflineAuditService uploads queue to POST /v1/machines/{id}/audit-backlog with the machine's own credential.
func newOfflineAuditService(queue *offlineaudit.Queue, controlURL *url.URL, machineID, stateRoot string, transport http.RoundTripper, tokens *enrollment.RenewingTokenSource, operationID func() (string, error)) *offlineAuditService {
	return &offlineAuditService{uploader: &offlineaudit.Uploader{Queue: queue, ServerURL: strings.TrimSuffix(controlURL.String(), "/"), MachineID: machineID,
		Client: &http.Client{Transport: errorreport.TransportOperation(transport, controlURL.String(), "offline_audit"), CheckRedirect: rejectRuntimePolicyRedirect},
		Tokens: tokens, Proofs: enrollment.ProofSource{StateRoot: stateRoot}, OperationID: operationID, MinInterval: offlineaudit.UploadInterval}}
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

const sshCapability = "managed_ssh"

// admitOfflineSSH is the offline decision for a managed SSH stream. A refusal is recorded as an
// ssh_session_refused (origin offline); an admitted session is recorded by managedssh.Host.Serve, which audits
// every SSH session online or offline and refuses an offline one whose start cannot be recorded.
func admitOfflineSSH(queue *offlineaudit.Queue, request native.OfflineAdmission) error {
	if request.Allowed {
		return nil
	}
	refusal := errors.New("no signed scope allows this session")
	err := queue.Record(offlineaudit.Event{Kind: offlineaudit.KindSSHRefused, PeerEndpointID: request.Peer.EndpointID, PeerAccountID: request.Peer.AccountID, OperationID: request.Header.OperationID, Origin: offlineaudit.OriginOffline, Reason: managedssh.RefusedNoAccess})
	return errors.Join(refusal, err)
}

// SSHSessionAudit queues the start, refusal and end of every managed SSH session for upload (F5-12).
func SSHSessionAudit(queue *offlineaudit.Queue) managedssh.SessionAudit {
	if queue == nil {
		return nil
	}
	return func(session managedssh.SessionEvent) error {
		origin := offlineaudit.OriginOnline
		if session.Peer.Offline {
			origin = offlineaudit.OriginOffline
		}
		event := offlineaudit.Event{PeerEndpointID: session.Peer.EndpointID, PeerAccountID: session.Peer.AccountID, OperationID: session.Peer.OperationID, Origin: origin, Generation: session.Generation}
		switch session.Kind {
		case managedssh.SessionStarted:
			event.Kind, event.EventID = offlineaudit.KindSSHStarted, session.SessionID
		case managedssh.SessionRefused:
			event.Kind, event.Reason = offlineaudit.KindSSHRefused, session.Reason
		case managedssh.SessionEnded:
			event.Kind, event.SessionID, event.Outcome, event.BytesIn, event.BytesOut = offlineaudit.KindSSHEnded, session.SessionID, session.Outcome, session.BytesIn, session.BytesOut
		default:
			return errors.New("unknown managed SSH session event")
		}
		return queue.Record(event)
	}
}
