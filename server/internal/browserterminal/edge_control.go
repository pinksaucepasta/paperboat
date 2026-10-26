package browserterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"github.com/pinksaucepasta/paperboat/shared/terminalend"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

// authorityOutageGrace bounds how long an admitted browser attachment survives
// while its live authority check cannot get an answer. Revocations answered by
// the authority still end the attachment on the next 2-second check.
var authorityOutageGrace = 6 * time.Second

// EdgeControlRequest travels on a node-authenticated control socket. Terminal
// content never uses this connection.
type EdgeControlRequest struct {
	Ticket string `json:"ticket"`
	Origin string `json:"origin"`
	Host   string `json:"host"`
}

type edgeAdmission struct {
	Ticket       Ticket
	Decision     Decision
	Credential   string
	AttachmentID string
}

func (g *Gateway) admitEdge(ctx context.Context, input EdgeControlRequest, nodeID, processEpoch string) (edgeAdmission, func(), error) {
	if g == nil || g.Authorizer == nil || g.Signer == nil || nodeID == "" || processEpoch == "" || input.Host == "" || !slices.Contains(g.AllowedOrigins, input.Origin) {
		return edgeAdmission{}, nil, ErrForbidden
	}
	urlValue, err := url.Parse("wss://" + input.Host + "/v1/browser-terminal")
	if err != nil || urlValue.Host != input.Host || urlValue.Hostname() == "" || urlValue.Port() != "" || urlValue.User != nil || urlValue.RawQuery != "" || urlValue.Fragment != "" {
		return edgeAdmission{}, nil, ErrForbidden
	}
	ticket, err := g.Tickets.ConsumeEdge(ctx, input.Ticket, input.Origin)
	if err != nil {
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, refused(terminalend.Expired, ErrForbidden)
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !g.acquire(ticket.principalSessionID()) {
		return edgeAdmission{}, nil, refused(terminalend.TooManyPending, ErrForbidden)
	}
	release := func() { g.release(ticket.principalSessionID()) }
	decision, err := g.resolveTicketRole(ctx, ticket)
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, refused(rejectionReason(err), ErrForbidden)
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !validDecision(decision, ticket.subjectID(), ticket.TerminalSessionID) || decision.Role != ticket.Role || decision.PolicyGeneration != ticket.PolicyGeneration || decision.TeamGeneration != ticket.TeamGeneration || decision.MembershipGeneration != ticket.MembershipGeneration || decision.BindingGeneration != ticket.BindingGeneration || decision.HostWSSURL != urlValue.String() || decision.EdgeNodeID != nodeID || decision.EdgeProcessEpoch != processEpoch {
		release()
		return edgeAdmission{}, nil, refused(terminalend.NoAccess, ErrForbidden)
	}
	if ticket.Role == RoleInspector {
		admitted, err := g.admitInspectorEdge(ticket, decision)
		if err != nil {
			release()
			return edgeAdmission{}, nil, err
		}
		return admitted, release, nil
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	processGeneration, err := g.Authorizer.SnapshotProcess(snapshotCtx, decision)
	cancel()
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, refused(rejectionReason(err), ErrForbidden)
		}
		return edgeAdmission{}, nil, refused(terminalend.MachineUnavailable, unavailableCause(err))
	}
	if processGeneration < 1 {
		release()
		return edgeAdmission{}, nil, refused(terminalend.MachineUnavailable, ErrHostUnavailable)
	}
	current, err := g.resolveTicketRole(ctx, ticket)
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, refused(rejectionReason(err), ErrForbidden)
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !sameDecision(decision, current) {
		release()
		return edgeAdmission{}, nil, refused(terminalend.NoAccess, ErrForbidden)
	}
	now := time.Now().UTC()
	attachmentID, err := secureID("attachment")
	if err != nil {
		release()
		return edgeAdmission{}, nil, err
	}
	jti, err := secureID("token")
	if err != nil {
		release()
		return edgeAdmission{}, nil, err
	}
	actorAccountID := ticket.UserID
	if ticket.GuestSessionID != "" {
		actorAccountID = ""
	}
	issuer, err := admission.New(g.Signer, g.AdmissionLifetime)
	if err != nil {
		release()
		return edgeAdmission{}, nil, err
	}
	credential, err := issuer.Issue(mint.CredentialInput{Issuer: g.Issuer, Audience: "paperboat-machine", Subject: ticket.subjectID(), JTI: jti, IssuedAt: now, CredentialClass: "browser_terminal_operation", WorkspaceID: decision.WorkspaceID, ActorAccountID: actorAccountID, Scopes: []string{scopeForRole(decision.Role)}, EnvironmentID: decision.EnvironmentID, AccountID: ticket.subjectID(), MachineID: decision.MachineID, UserID: ticket.subjectID(), SessionID: ticket.TerminalSessionID, BrowserAttachmentID: attachmentID, BrowserPublicKeySHA256: ticket.BrowserPublicKeySHA256, ExpectedGeneration: processGeneration, PolicyGeneration: decision.PolicyGeneration})
	if err != nil {
		release()
		return edgeAdmission{}, nil, err
	}
	if g.RecordUsageAdmission != nil {
		if err := g.RecordUsageAdmission(ctx, attachmentID, decision.MachineID, ticket.TerminalSessionID, nodeID, actorAccountID, credential.ExpiresAt); err != nil {
			release()
			return edgeAdmission{}, nil, err
		}
	}
	return edgeAdmission{Ticket: ticket, Decision: decision, Credential: credential.Token, AttachmentID: attachmentID}, release, nil
}

// ServeEdgeControl holds the admission reservation and checks the browser
// session and exact terminal grant throughout the edge's data connection.
func (g *Gateway) ServeEdgeControl(w http.ResponseWriter, r *http.Request, nodeID, processEpoch string) {
	if r.Method != http.MethodGet || nodeID == "" || processEpoch == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer connection.CloseNow()
	connection.SetReadLimit(16 << 10)
	firstCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	kind, raw, err := connection.Read(firstCtx)
	cancel()
	if err != nil || kind != websocket.MessageText {
		_ = connection.Close(websocket.StatusPolicyViolation, "invalid admission")
		return
	}
	var input EdgeControlRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF {
		_ = connection.Close(websocket.StatusPolicyViolation, "invalid admission")
		return
	}
	admission, release, err := g.admitEdge(r.Context(), input, nodeID, processEpoch)
	if err != nil {
		reportEdgeAdmissionFailure(r.Context(), err)
		// The edge closes the browser's WebSocket with the code of this reason.
		_ = connection.Close(websocket.StatusPolicyViolation, string(admissionRefusalReason(err)))
		return
	}
	defer release()
	response, err := json.Marshal(struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}{admission.Credential, edgeHubKey(admission), admission.AttachmentID})
	if err != nil || connection.Write(r.Context(), websocket.MessageText, response) != nil {
		return
	}
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		// A read error or any unsolicited terminal data ends control ownership.
		_, _, _ = connection.Read(r.Context())
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var outageSince time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-disconnected:
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			active, checkErr := g.ticketSessionActive(checkCtx, admission.Ticket)
			if active && checkErr == nil {
				var current Decision
				current, checkErr = g.resolveTicketRole(checkCtx, admission.Ticket)
				active = checkErr == nil && sameDecision(admission.Decision, current)
			}
			cancel()
			if checkErr != nil {
				reportEdgeAdmissionFailure(r.Context(), checkErr)
			}
			if checkErr != nil && !expectedTerminalRejection(checkErr) {
				// The authority could not answer (database or host overload). Keep
				// the attachment for a short bounded grace; any answered refusal
				// still ends it at once.
				if outageSince.IsZero() {
					outageSince = time.Now()
				}
				if time.Since(outageSince) > authorityOutageGrace {
					_ = connection.Close(websocket.StatusPolicyViolation, string(terminalend.EdgeUnavailable))
					return
				}
			} else if !active || checkErr != nil {
				_ = connection.Close(websocket.StatusPolicyViolation, string(liveCheckReason(checkErr)))
				return
			} else {
				outageSince = time.Time{}
			}
			heartbeatCtx, stop := context.WithTimeout(r.Context(), 2*time.Second)
			err = connection.Write(heartbeatCtx, websocket.MessageText, []byte("ok"))
			stop()
			if err != nil {
				return
			}
		}
	}
}

// refusal is an admission refusal that names why, for the edge to tell the browser. It unwraps to the sentinel the
// admission has always returned, so the expected-rejection classification is unchanged.
type refusal struct {
	reason terminalend.Reason
	cause  error
}

func (e refusal) Error() string { return e.cause.Error() }
func (e refusal) Unwrap() error { return e.cause }

func refused(reason terminalend.Reason, cause error) error {
	return refusal{reason: reason, cause: cause}
}

// rejectionReason names an expected rejection of a ticket's terminal.
func rejectionReason(err error) terminalend.Reason {
	switch {
	case errors.Is(err, usermachines.ErrBandwidthQuotaExhausted):
		return terminalend.BandwidthQuotaExhausted
	case errors.Is(err, usermachines.ErrTerminalBrowserSessionClosed):
		return terminalend.Ended
	case errors.Is(err, ErrTicketInvalid):
		return terminalend.Expired
	}
	return terminalend.NoAccess
}

// admissionRefusalReason is the reason the control channel closes with when the server refuses to admit.
func admissionRefusalReason(err error) terminalend.Reason {
	var named refusal
	switch {
	case errors.As(err, &named):
		return named.reason
	case errors.Is(err, usermachines.ErrBandwidthQuotaExhausted):
		return terminalend.BandwidthQuotaExhausted
	case errors.Is(err, ErrForbidden):
		return terminalend.NoAccess
	case errors.Is(err, ErrHostUnavailable):
		return terminalend.MachineUnavailable
	}
	return terminalend.EdgeUnavailable
}

// liveCheckReason names why an attached terminal lost its access: its terminal ended, or its access was revoked.
func liveCheckReason(err error) terminalend.Reason {
	if errors.Is(err, usermachines.ErrTerminalBrowserSessionClosed) {
		return terminalend.Ended
	}
	return terminalend.Revoked
}

func reportEdgeAdmissionFailure(ctx context.Context, err error) {
	if err == nil || pureTerminalCancellation(err) {
		return
	}
	if expectedTerminalRejection(err) {
		serverdiagnostics.LoggerWithRequest(ctx, slog.Default()).WarnContext(ctx, "browser terminal edge admission denied", "reason", edgeAdmissionReason(err))
		return
	}
	supportreport.CaptureFromContext(ctx, "paperboat-server", "http_terminal", "service_error", "terminal_host_unavailable", err)
}

func edgeAdmissionReason(err error) string {
	if errors.Is(err, ErrForbidden) {
		return "forbidden"
	}
	if errors.Is(err, ErrHostUnavailable) {
		return "host_unavailable"
	}
	if errors.Is(err, usermachines.ErrBandwidthQuotaExhausted) {
		return "bandwidth_quota_exhausted"
	}
	return "internal_unavailable"
}

// edgeHubKey is the edge's broadcast key for this connection: the terminal
// session, or the attachment itself for an inspector channel (nothing is
// broadcast to it).
func edgeHubKey(a edgeAdmission) string {
	if a.Ticket.Role == RoleInspector {
		return a.AttachmentID
	}
	return a.Ticket.TerminalSessionID
}
