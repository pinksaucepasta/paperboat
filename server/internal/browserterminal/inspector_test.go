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
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

type inspectorTestAuthorizer struct {
	testBrowserAuthorizer
	resolveErr error
}

func (a *inspectorTestAuthorizer) ResolveInspector(_ context.Context, userID, credentialID string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolveErr != nil {
		return Decision{}, a.resolveErr
	}
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
	gateway := &Gateway{Tickets: tickets, Authorizer: &inspectorTestAuthorizer{testBrowserAuthorizer: testBrowserAuthorizer{decision: inspectorDecision()}}, AllowedOrigins: []string{"https://dashboard.example.test"}}
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
	authorizer := &inspectorTestAuthorizer{testBrowserAuthorizer: testBrowserAuthorizer{decision: inspectorDecision()}}
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

func TestEdgeKeepsAnAttachmentThroughABriefAuthorityOutageButNotARefusal(t *testing.T) {
	grace := authorityOutageGrace
	authorityOutageGrace = 3 * time.Second
	defer func() { authorityOutageGrace = grace }()
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: "usr_1", BrowserSessionID: "ses_1", InspectorCredentialID: "credential_1", Origin: "https://dashboard.example.test", Role: RoleInspector, PolicyGeneration: 7, BrowserPublicKeySHA256: testBrowserKeyDigest}
	authorizer := &inspectorTestAuthorizer{testBrowserAuthorizer: testBrowserAuthorizer{decision: inspectorDecision()}}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "inspector-ticket"}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.ServeEdgeControl(w, r, "edge_1", "epoch_1")
	}))
	defer server.Close()
	admit := func() *websocket.Conn {
		t.Helper()
		tickets.mu.Lock()
		tickets.used = false
		tickets.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		connection, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(server.URL, "http://"), nil)
		if err != nil {
			t.Fatal(err)
		}
		request, _ := json.Marshal(EdgeControlRequest{Ticket: "inspector-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
		if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
			t.Fatal(err)
		}
		if _, _, err := connection.Read(ctx); err != nil {
			t.Fatal(err)
		}
		return connection
	}
	setErr := func(err error) {
		authorizer.mu.Lock()
		authorizer.resolveErr = err
		authorizer.mu.Unlock()
	}
	// watch reads one attachment in the background; heartbeats counts its "ok"
	// messages inside a window and reports whether it closed.
	type watched struct {
		ok     chan struct{}
		closed chan struct{}
	}
	watch := func(connection *websocket.Conn) watched {
		w := watched{ok: make(chan struct{}, 64), closed: make(chan struct{})}
		go func() {
			defer close(w.closed)
			for {
				if _, _, err := connection.Read(context.Background()); err != nil {
					return
				}
				w.ok <- struct{}{}
			}
		}()
		return w
	}
	heartbeats := func(w watched, window time.Duration) (int, bool) {
		deadline := time.After(window)
		n := 0
		for {
			select {
			case <-w.ok:
				n++
			case <-w.closed:
				return n, true
			case <-deadline:
				return n, false
			}
		}
	}

	connection := admit()
	defer connection.CloseNow()
	live := watch(connection)
	// One unanswered check (well inside the grace), then the authority recovers.
	setErr(context.DeadlineExceeded)
	if n, closed := heartbeats(live, 2500*time.Millisecond); closed || n < 1 {
		t.Fatalf("brief outage closed the attachment (heartbeats %d, closed %v)", n, closed)
	}
	setErr(nil)
	if n, closed := heartbeats(live, 2500*time.Millisecond); closed || n < 1 {
		t.Fatalf("attachment did not recover after the outage (heartbeats %d, closed %v)", n, closed)
	}
	// An outage longer than the grace ends it.
	setErr(context.DeadlineExceeded)
	if _, closed := heartbeats(live, 9*time.Second); !closed {
		t.Fatal("attachment outlived the authority outage grace")
	}

	// An answered refusal ends a fresh attachment on the next check.
	setErr(nil)
	refused := admit()
	defer refused.CloseNow()
	refusedWatch := watch(refused)
	setErr(ErrForbidden)
	start := time.Now()
	if _, closed := heartbeats(refusedWatch, 5*time.Second); !closed || time.Since(start) > 3*time.Second {
		t.Fatalf("refusal did not end the attachment promptly (closed %v after %s)", closed, time.Since(start))
	}
}
