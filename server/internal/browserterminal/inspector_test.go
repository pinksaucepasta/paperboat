package browserterminal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
)

type inspectorTestAuthorizer struct{ testBrowserAuthorizer }

func (a *inspectorTestAuthorizer) ResolveInspector(_ context.Context, userID, credentialID string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if userID != a.decision.UserID || credentialID != a.decision.InspectorCredentialID {
		return Decision{}, ErrForbidden
	}
	return a.decision, nil
}

type recordingTickets struct {
	issueTestTickets
	issued Ticket
}

func (r *recordingTickets) Issue(ctx context.Context, ticket Ticket) (Ticket, error) {
	r.issued = ticket
	return r.issueTestTickets.Issue(ctx, ticket)
}

func inspectorDecision() Decision {
	return Decision{WorkspaceID: "personal", UserID: "usr_1", OwnerAccountID: "usr_owner", InspectorCredentialID: "credential_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: RoleInspector, PolicyGeneration: 7}
}

func TestGatewayIssuesInspectorTicketBoundToCredential(t *testing.T) {
	tickets := &recordingTickets{}
	gateway := &Gateway{Tickets: tickets, Authorizer: &inspectorTestAuthorizer{testBrowserAuthorizer{decision: inspectorDecision()}}, AllowedOrigins: []string{"https://dashboard.example.test"}}
	ticket, streamURL, _, err := gateway.IssueInspector(context.Background(), "usr_1", "browser_1", "credential_1", "https://dashboard.example.test", testBrowserKeyDigest)
	if err != nil {
		t.Fatal(err)
	}
	if streamURL != "wss://edge-one.example.test/v1/browser-terminal/machine.runtime.example.test" || ticket.Role != RoleInspector {
		t.Fatalf("stream %q role %q", streamURL, ticket.Role)
	}
	if got := tickets.issued; got.InspectorCredentialID != "credential_1" || got.TerminalSessionID != "" || got.PolicyGeneration != 7 || got.UserID != "usr_1" || got.BrowserSessionID != "browser_1" {
		t.Fatalf("stored ticket bindings = %#v", got)
	}
	if _, _, _, err := gateway.IssueInspector(context.Background(), "usr_2", "browser_2", "credential_1", "https://dashboard.example.test", testBrowserKeyDigest); err != ErrForbidden {
		t.Fatalf("another user's credential: %v", err)
	}
	if _, _, _, err := gateway.IssueInspector(context.Background(), "usr_1", "browser_1", "", "https://dashboard.example.test", testBrowserKeyDigest); err != ErrForbidden {
		t.Fatalf("missing credential: %v", err)
	}
	// A terminal-only authorizer never admits inspector tickets.
	plain := &Gateway{Tickets: tickets, Authorizer: &testBrowserAuthorizer{decision: inspectorDecision()}, AllowedOrigins: []string{"https://dashboard.example.test"}}
	if _, _, _, err := plain.IssueInspector(context.Background(), "usr_1", "browser_1", "credential_1", "https://dashboard.example.test", testBrowserKeyDigest); err != ErrForbidden {
		t.Fatalf("terminal authorizer admitted an inspector ticket: %v", err)
	}
}

func TestEdgeAdmitsInspectorChannelAndClosesItWhenTheCredentialIsRevoked(t *testing.T) {
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: "usr_1", BrowserSessionID: "ses_1", InspectorCredentialID: "credential_1", Origin: "https://dashboard.example.test", Role: RoleInspector, PolicyGeneration: 7, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "inspector-ticket"}
	authorizer := &inspectorTestAuthorizer{testBrowserAuthorizer{decision: inspectorDecision()}}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.ServeEdgeControl(w, r, "edge_1", "epoch_1")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(server.URL, "http://"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	request, _ := json.Marshal(EdgeControlRequest{Ticket: "inspector-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
	if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var admitted struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}
	if err := json.Unmarshal(payload, &admitted); err != nil || admitted.AttachmentID == "" || admitted.TerminalSessionID != admitted.AttachmentID {
		t.Fatalf("admission %s: %v", payload, err)
	}
	claims, err := signer.VerifyCredential(admitted.Credential, gateway.Issuer, "browser_inspector_operation", time.Now())
	if err != nil || claims.MachineID != "mch_1" || claims.UserID != "usr_1" || claims.SessionID != "" || claims.BrowserAttachmentID != admitted.AttachmentID || claims.BrowserPublicKeySHA256 != testBrowserKeyDigest || claims.Scopes[0] != "inspector:use" {
		t.Fatalf("inspector credential claims %#v: %v", claims, err)
	}
	if _, err := signer.VerifyCredential(admitted.Credential, gateway.Issuer, "browser_terminal_operation", time.Now()); err == nil {
		t.Fatal("inspector channel credential verified as a terminal credential")
	}
	// Revoking the inspector credential ends the open channel.
	authorizer.mu.Lock()
	authorizer.decision.InspectorCredentialID = "credential_revoked"
	authorizer.mu.Unlock()
	for {
		if _, data, err := connection.Read(ctx); err != nil {
			break
		} else if string(data) != "ok" {
			t.Fatalf("unexpected control message %q", data)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("revoked inspector channel stayed open")
	}
}
