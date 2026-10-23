package browserterminal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

type guestTestAuthorizer struct {
	mu                       sync.Mutex
	decision                 Decision
	accountCalls, guestCalls int
}

func (a *guestTestAuthorizer) ResolveRole(_ context.Context, user, terminal string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accountCalls++
	d := a.decision
	d.GuestSessionID = ""
	d.UserID = user
	d.TerminalSessionID = terminal
	return d, nil
}
func (a *guestTestAuthorizer) ResolveGuestRole(_ context.Context, guest, terminal string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.guestCalls++
	return a.decision, nil
}
func (*guestTestAuthorizer) SnapshotProcess(context.Context, Decision) (int64, error) { return 3, nil }

type guestTestTickets struct {
	edgeTestTickets
	accountChecks int
	issued        Ticket
}

func (s *guestTestTickets) Issue(_ context.Context, in Ticket) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.issued = in
	in.Value = "issued"
	return in, nil
}
func (s *guestTestTickets) GuestSessionActive(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, nil
}
func (s *guestTestTickets) SessionActive(context.Context, string, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountChecks++
	return false, nil
}
func guestTestDecision() Decision {
	return Decision{WorkspaceID: "personal", GuestSessionID: "guest_1", UserID: "guest_1", OwnerAccountID: "owner_1", TerminalSessionID: "term_1", MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge.example.test", Role: "viewer", PolicyGeneration: 1}
}
func TestGuestGatewayUsesExplicitIdentityBranch(t *testing.T) {
	a := &guestTestAuthorizer{decision: guestTestDecision()}
	tickets := &guestTestTickets{}
	g := &Gateway{Authorizer: a, Tickets: tickets, AllowedOrigins: []string{"https://dashboard.example.test"}}
	ticket, stream, d, err := g.IssueGuest(context.Background(), "guest_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest)
	if err != nil || ticket.GuestSessionID != "guest_1" || ticket.UserID != "" || ticket.BrowserSessionID != "" || d.Role != "viewer" || stream != "wss://edge.example.test/v1/browser-terminal/machine.runtime.example.test" {
		t.Fatalf("guest issue: ticket=%+v decision=%+v stream=%q err=%v", ticket, d, stream, err)
	}
	if a.accountCalls != 0 || a.guestCalls != 1 {
		t.Fatalf("wrong authorization branch: account=%d guest=%d", a.accountCalls, a.guestCalls)
	}
	account, _, _, err := g.Issue(context.Background(), "account_1", "session_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest)
	if err != nil || account.GuestSessionID != "" || account.UserID != "account_1" || account.BrowserSessionID != "session_1" || a.accountCalls != 1 || a.guestCalls != 1 {
		t.Fatalf("account branch changed: %+v %v", account, err)
	}
	a.decision.Role = "interactive"
	controlled, _, _, err := g.IssueGuest(context.Background(), "guest_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest)
	if err != nil || controlled.Role != "interactive" {
		t.Fatalf("explicit guest control: %+v %v", controlled, err)
	}
	a.decision.Role = "owner"
	if _, _, _, err := g.IssueGuest(context.Background(), "guest_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest); !errors.Is(err, ErrForbidden) {
		t.Fatalf("guest owner accepted: %v", err)
	}
	a.decision.Role = "viewer"
	a.decision.GuestSessionID = "other_guest"
	if _, _, _, err := g.IssueGuest(context.Background(), "guest_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest); !errors.Is(err, ErrForbidden) {
		t.Fatalf("wrong guest accepted: %v", err)
	}
	// Optional guest interfaces must fail closed on an account-only implementation.
	g.Authorizer = &testBrowserAuthorizer{decision: guestTestDecision()}
	if _, _, _, err := g.IssueGuest(context.Background(), "guest_1", "term_1", g.AllowedOrigins[0], testBrowserKeyDigest); !errors.Is(err, ErrForbidden) {
		t.Fatalf("account-only authorizer accepted guest: %v", err)
	}
	g.Tickets = &edgeTestTickets{active: true}
	if active, err := g.ticketSessionActive(context.Background(), Ticket{GuestSessionID: "guest_1"}); active || !errors.Is(err, ErrForbidden) {
		t.Fatalf("account-only session authority accepted guest: %v %v", active, err)
	}
}
func TestGuestEdgeDisconnectsWhenGuestSessionWithdrawn(t *testing.T) {
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision := guestTestDecision()
	ticket := Ticket{GuestSessionID: decision.GuestSessionID, TerminalSessionID: decision.TerminalSessionID, Origin: "https://dashboard.example.test", Role: "viewer", PolicyGeneration: 1, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &guestTestTickets{edgeTestTickets: edgeTestTickets{active: true, ticket: ticket, value: "guest-ticket"}}
	a := &guestTestAuthorizer{decision: decision}
	g := &Gateway{Tickets: tickets, Authorizer: a, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { g.ServeEdgeControl(w, r, "edge_1", "epoch_1") }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(server.URL, "http://"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	raw, _ := json.Marshal(EdgeControlRequest{Ticket: "guest-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
	if err = conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatal(err)
	}
	_, raw, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var admission struct {
		Credential string `json:"credential"`
	}
	if err = json.Unmarshal(raw, &admission); err != nil {
		t.Fatal(err)
	}
	claims, err := signer.VerifyCredential(admission.Credential, g.Issuer, "browser_terminal_operation", time.Now())
	if err != nil || claims.Subject != ticket.GuestSessionID || claims.UserID != ticket.GuestSessionID || len(claims.Scopes) != 1 || claims.Scopes[0] != "terminal:view" || claims.BrowserPublicKeySHA256 != testBrowserKeyDigest {
		t.Fatalf("guest credential binding: %+v %v", claims, err)
	}
	tickets.mu.Lock()
	tickets.active = false
	tickets.mu.Unlock()
	if _, _, err = conn.Read(ctx); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("guest withdrawal did not revoke live edge: %v", err)
	}
	a.mu.Lock()
	accounts := a.accountCalls
	a.mu.Unlock()
	tickets.mu.Lock()
	accountChecks := tickets.accountChecks
	tickets.mu.Unlock()
	if accounts != 0 || accountChecks != 0 {
		t.Fatalf("guest crossed account authorization: role=%d session=%d", accounts, accountChecks)
	}
}
