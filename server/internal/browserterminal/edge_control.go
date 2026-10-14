package browserterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/coder/websocket"
	serverdiagnostics "github.com/pinksaucepasta/paperboat-server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/supportreport"
)

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
			return edgeAdmission{}, nil, ErrForbidden
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !g.acquire(ticket.principalSessionID()) {
		return edgeAdmission{}, nil, ErrForbidden
	}
	release := func() { g.release(ticket.principalSessionID()) }
	decision, err := g.resolveTicketRole(ctx, ticket)
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, ErrForbidden
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !validDecision(decision, ticket.subjectID(), ticket.TerminalSessionID) || decision.Role != ticket.Role || decision.PolicyGeneration != ticket.PolicyGeneration || decision.TeamGeneration != ticket.TeamGeneration || decision.MembershipGeneration != ticket.MembershipGeneration || decision.BindingGeneration != ticket.BindingGeneration || decision.HostWSSURL != urlValue.String() || decision.EdgeNodeID != nodeID || decision.EdgeProcessEpoch != processEpoch {
		release()
		return edgeAdmission{}, nil, ErrForbidden
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	processGeneration, err := g.Authorizer.SnapshotProcess(snapshotCtx, decision)
	cancel()
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, ErrForbidden
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if processGeneration < 1 {
		release()
		return edgeAdmission{}, nil, ErrHostUnavailable
	}
	current, err := g.resolveTicketRole(ctx, ticket)
	if err != nil {
		release()
		if expectedTerminalRejection(err) {
			return edgeAdmission{}, nil, ErrForbidden
		}
		return edgeAdmission{}, nil, unavailableCause(err)
	}
	if !sameDecision(decision, current) {
		release()
		return edgeAdmission{}, nil, ErrForbidden
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
	credential, err := g.Signer.SignCredential(mint.CredentialInput{Issuer: g.Issuer, Audience: "paperboat-machine", Subject: ticket.subjectID(), JTI: jti, IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), CredentialClass: "browser_terminal_operation", WorkspaceID: decision.WorkspaceID, ActorAccountID: actorAccountID, Scopes: []string{scopeForRole(decision.Role)}, EnvironmentID: decision.EnvironmentID, AccountID: ticket.subjectID(), MachineID: decision.MachineID, UserID: ticket.subjectID(), SessionID: ticket.TerminalSessionID, BrowserAttachmentID: attachmentID, BrowserPublicKeySHA256: ticket.BrowserPublicKeySHA256, ExpectedGeneration: processGeneration, PolicyGeneration: decision.PolicyGeneration})
	if err != nil {
		release()
		return edgeAdmission{}, nil, err
	}
	if g.RecordUsageAdmission != nil {
		if err := g.RecordUsageAdmission(ctx, attachmentID, decision.MachineID, ticket.TerminalSessionID, nodeID, now.Add(5*time.Minute)); err != nil {
			release()
			return edgeAdmission{}, nil, err
		}
	}
	return edgeAdmission{Ticket: ticket, Decision: decision, Credential: credential, AttachmentID: attachmentID}, release, nil
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
		_ = connection.Close(websocket.StatusPolicyViolation, "admission denied")
		return
	}
	defer release()
	response, err := json.Marshal(struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}{admission.Credential, admission.Ticket.TerminalSessionID, admission.AttachmentID})
	if err != nil || connection.Write(r.Context(), websocket.MessageText, response) != nil {
		return
	}
	disconnected := make(chan struct{})
	go func() {
		defer close(disconnected)
		for {
			if _, _, err := connection.Read(r.Context()); err != nil {
				return
			}
			// The edge never sends terminal data on the control connection.
			return
		}
	}()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
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
			if !active || checkErr != nil {
				if checkErr != nil {
					reportEdgeAdmissionFailure(r.Context(), checkErr)
				}
				_ = connection.Close(websocket.StatusPolicyViolation, "access revoked")
				return
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
	return "internal_unavailable"
}
