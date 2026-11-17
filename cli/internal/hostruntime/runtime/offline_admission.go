package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pinksaucepasta/paperboat/cli/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/cli/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/sessionname"
)

// offlineAdmission records every offline session decision before the host acts on it (F21-11):
// a start that cannot be recorded is refused, so audit fails closed. The decision is the carrier's own, from the
// signed scopes it holds; a carrier that dispatches to a per-account worker forwards the admitted peer with the stream.
func offlineAdmission(queue *offlineaudit.Queue, online func() bool) func(context.Context, native.OfflineAdmission) error {
	if queue == nil {
		return nil
	}
	return func(_ context.Context, request native.OfflineAdmission) error {
		if request.Capability == sshCapability {
			return admitOfflineSSH(queue, request, online())
		}
		// Every exec is admitted here, online too (no per-command access session); Origin says which.
		origin := offlineaudit.OriginOffline
		if online() {
			origin = offlineaudit.OriginOnline
		}
		event := offlineaudit.Event{Kind: offlineaudit.KindStarted, Capability: request.Capability, PeerEndpointID: request.Peer.EndpointID, PeerAccountID: request.Peer.AccountID, OperationID: request.Header.OperationID, Origin: origin}
		var refusal error
		if !request.Allowed {
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
		if err == nil && refusal == nil {
			rememberOfflineTerminal(queue.Adopt, request)
		}
		return errors.Join(refusal, err)
	}
}

// rememberOfflineTerminal queues a terminal session opened without Paperboat (its id says so) for registration: the host
// tells the server once it answers, so the session is listed even when the client that opened it is gone. Failing to
// queue it never refuses the session.
func rememberOfflineTerminal(adopt *offlineaudit.Queue, request native.OfflineAdmission) {
	id := request.Header.OperationID
	if adopt == nil || !sessionname.IsOfflineID(id) || adopt.HasOperation(id) {
		return
	}
	event := offlineaudit.Event{Kind: offlineaudit.KindStarted, Capability: request.Capability, PeerEndpointID: request.Peer.EndpointID, PeerAccountID: request.Peer.AccountID, OperationID: id}
	if err := adopt.Record(event); err != nil {
		diagnosticlog.TryWarn("an offline terminal session could not be queued for registration with Paperboat", "error", err)
	}
}

// newOfflineAuditService uploads queue to POST /v1/machines/{id}/audit-backlog and registers the offline terminal
// sessions of queue.Adopt with POST /v1/machines/{id}/offline-terminal-sessions, both with the machine's own credential.
func newOfflineAuditService(queue *offlineaudit.Queue, controlURL *url.URL, machineID, stateRoot string, transport http.RoundTripper, tokens *enrollment.RenewingTokenSource, operationID func() (string, error), reachable <-chan struct{}) *offlineAuditService {
	serverURL := strings.TrimSuffix(controlURL.String(), "/")
	client := &http.Client{Transport: errorreport.TransportOperation(transport, controlURL.String(), "offline_audit"), CheckRedirect: rejectRuntimePolicyRedirect}
	proofs := enrollment.ProofSource{StateRoot: stateRoot}
	return &offlineAuditService{reachable: reachable,
		uploader: &offlineaudit.Uploader{Queue: queue, ServerURL: serverURL, MachineID: machineID, Client: client,
			Tokens: tokens, Proofs: proofs, OperationID: operationID, MinInterval: offlineaudit.UploadInterval},
		adopter: &offlineaudit.Adopter{Queue: queue.Adopt, ServerURL: serverURL, MachineID: machineID, Client: client,
			Tokens: tokens, Proofs: proofs, OperationID: operationID}}
}

// offlineAuditService uploads the offline session backlog whenever events are queued and registers the sessions opened
// without Paperboat once it answers.
type offlineAuditService struct {
	uploader  *offlineaudit.Uploader
	adopter   *offlineaudit.Adopter
	reachable <-chan struct{}
	cancel    context.CancelFunc
	done      chan struct{}
}

func (s *offlineAuditService) Start(context.Context) error {
	if s.cancel != nil {
		return ErrProductionInvalid
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	// The control plane being reachable again wakes both loops: one signal reaches each.
	uploadWake, adoptWake := make(chan struct{}, 1), make(chan struct{}, 1)
	s.uploader.Wake, s.adopter.Wake = uploadWake, adoptWake
	var running sync.WaitGroup
	running.Add(3)
	go func() { defer running.Done(); s.uploader.Run(ctx) }()
	go func() { defer running.Done(); s.adopter.Run(ctx) }()
	go func() {
		defer running.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.reachable:
				for _, wake := range []chan struct{}{uploadWake, adoptWake} {
					select {
					case wake <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	go func() { running.Wait(); close(s.done) }()
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

// admitOfflineSSH decides a managed SSH stream from the host's signed rules, which admit every SSH stream (D1). A
// refusal is recorded as an ssh_session_refused (origin offline once Paperboat has not answered past the signed network's refresh hint); an
// admitted session is recorded by managedssh.Host.Serve, which refuses an offline one whose start cannot be recorded.
func admitOfflineSSH(queue *offlineaudit.Queue, request native.OfflineAdmission, online bool) error {
	if request.Allowed {
		return nil
	}
	refusal := errors.New("no signed scope allows this session")
	origin := offlineaudit.OriginOffline
	if online {
		origin = offlineaudit.OriginOnline
	}
	err := queue.Record(offlineaudit.Event{Kind: offlineaudit.KindSSHRefused, PeerEndpointID: request.Peer.EndpointID, PeerAccountID: request.Peer.AccountID, OperationID: request.Header.OperationID, Origin: origin, Reason: managedssh.RefusedNoAccess})
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

// ExecEndAudit queues the end of every peer-started exec (outcome, exit code, duration) for upload with the starts the
// carrier records. The start and end pair by operation id. A full queue drops the end: the start is on record already.
func ExecEndAudit(queue *offlineaudit.Queue) server.ExecEndRecorder {
	if queue == nil {
		return nil
	}
	return func(end server.ExecEnd) {
		origin := offlineaudit.OriginOnline
		if end.Offline {
			origin = offlineaudit.OriginOffline
		}
		event := offlineaudit.Event{Kind: offlineaudit.KindEnded, Capability: "exec", PeerEndpointID: end.EndpointID, PeerAccountID: end.AccountID, OperationID: end.OperationID, Origin: origin, Outcome: string(end.Snapshot.State)}
		if r := end.Snapshot.Result; r != nil {
			code := r.Code
			event.ExitCode = &code
			if started := end.Snapshot.StartedAt; started != nil && r.ExitedAt.After(*started) {
				event.DurationMS = r.ExitedAt.Sub(*started).Milliseconds()
			}
		}
		if err := queue.Record(event); err != nil {
			diagnosticlog.TryWarn("exec end could not be queued for audit", "error", err)
		}
	}
}
