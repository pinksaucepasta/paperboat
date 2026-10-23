package browserterminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

type edgeTestTickets struct {
	mu     sync.Mutex
	used   bool
	active bool
	ticket Ticket
	value  string
}

type testBrowserAuthorizer struct {
	mu          sync.Mutex
	decision    Decision
	snapshotErr error
}

func (a *testBrowserAuthorizer) ResolveRole(context.Context, string, string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.decision, nil
}
func (a *testBrowserAuthorizer) SnapshotProcess(context.Context, Decision) (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return 3, a.snapshotErr
}

type edgeReportStore struct {
	mu      sync.Mutex
	records []supportreport.Record
}

func (s *edgeReportStore) Save(_ context.Context, record supportreport.Record) error {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	return nil
}
func (*edgeReportStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (*edgeReportStore) Cleanup(context.Context) error { return nil }

func (s *edgeTestTickets) Issue(context.Context, Ticket) (Ticket, error) {
	return Ticket{}, ErrTicketInvalid
}
func (s *edgeTestTickets) Consume(context.Context, string, string, string) (Ticket, error) {
	return Ticket{}, ErrTicketInvalid
}
func (s *edgeTestTickets) ConsumeEdge(_ context.Context, value, origin string) (Ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used || value != s.value || origin != s.ticket.Origin {
		return Ticket{}, ErrTicketInvalid
	}
	s.used = true
	return s.ticket, nil
}
func (s *edgeTestTickets) SessionActive(context.Context, string, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active, nil
}

func TestEdgeControlAdmitsExactNodeAndRevokesLiveAttachment(t *testing.T) {
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: "usr_1", BrowserSessionID: "ses_1", TerminalSessionID: "term_1", Origin: "https://dashboard.example.test", Role: "interactive", PolicyGeneration: 1, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "wrong-node-ticket"}
	decision := Decision{WorkspaceID: "personal", UserID: ticket.UserID, OwnerAccountID: ticket.UserID, TerminalSessionID: ticket.TerminalSessionID, MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", Role: ticket.Role, PolicyGeneration: 1}
	authorizer := &testBrowserAuthorizer{decision: decision}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gateway.ServeEdgeControl(w, r, r.Header.Get("X-Node"), r.Header.Get("X-Epoch"))
	}))
	defer server.Close()
	connect := func(node string) (*websocket.Conn, error) {
		header := http.Header{"X-Node": {node}, "X-Epoch": {"epoch_1"}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		connection, _, dialErr := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(server.URL, "http://"), &websocket.DialOptions{HTTPHeader: header})
		return connection, dialErr
	}
	first, err := connect("edge_2")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(EdgeControlRequest{Ticket: "wrong-node-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	if err := first.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.Read(ctx); err == nil {
		t.Fatal("wrong edge node obtained admission")
	}
	first.CloseNow()
	// The failed admission consumed its one-use ticket. A fresh ticket models a
	// new dashboard request; replay of the old value cannot regain admission.
	tickets.mu.Lock()
	tickets.used = false
	tickets.value = "fresh-ticket"
	tickets.mu.Unlock()
	request, _ = json.Marshal(EdgeControlRequest{Ticket: "fresh-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
	connection, err := connect("edge_1")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.CloseNow()
	if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	kind, payload, err := connection.Read(ctx)
	if err != nil || kind != websocket.MessageText {
		t.Fatalf("admission response: %v", err)
	}
	var admitted struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
		AttachmentID      string `json:"attachment_id"`
	}
	if err := json.Unmarshal(payload, &admitted); err != nil || admitted.Credential == "" || admitted.TerminalSessionID != ticket.TerminalSessionID || admitted.AttachmentID == "" {
		t.Fatalf("invalid admission: %v", err)
	}
	claims, err := signer.VerifyCredential(admitted.Credential, gateway.Issuer, "browser_terminal_operation", time.Now())
	if err != nil || claims.BrowserPublicKeySHA256 != ticket.BrowserPublicKeySHA256 || claims.SessionID != ticket.TerminalSessionID || claims.Scopes[0] != "terminal:control" || claims.WorkspaceID != decision.WorkspaceID || claims.ActorAccountID != ticket.UserID {
		t.Fatalf("credential binding: %v", err)
	}
	// Removing the sharing grant disconnects an attachment already in progress.
	authorizer.mu.Lock()
	authorizer.decision.PolicyGeneration++
	authorizer.mu.Unlock()
	if _, _, err := connection.Read(ctx); err == nil {
		t.Fatal("revoked attachment remained connected")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gateway.mu.Lock()
		active := gateway.activeTotal
		gateway.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("revoked attachment retained capacity")
}

func TestEdgeAdmissionCapturesOperationalCauseAndCanRecover(t *testing.T) {
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: "usr_1", BrowserSessionID: "ses_1", TerminalSessionID: "term_1", Origin: "https://dashboard.example.test", Role: "interactive", PolicyGeneration: 1, BrowserPublicKeySHA256: testBrowserKeyDigest}
	tickets := &edgeTestTickets{active: true, ticket: ticket, value: "failed-ticket"}
	secret := "private-helper-detail-do-not-log"
	authorizer := &testBrowserAuthorizer{
		decision:    Decision{WorkspaceID: "personal", UserID: ticket.UserID, OwnerAccountID: ticket.UserID, TerminalSessionID: ticket.TerminalSessionID, MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", Role: ticket.Role, PolicyGeneration: 1},
		snapshotErr: errors.Join(ErrForbidden, fmt.Errorf("helper returned %s: %w", secret, syscall.ECONNREFUSED)),
	}
	gateway := &Gateway{Tickets: tickets, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
	store := &edgeReportStore{}
	reports, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stopReports := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		_ = reports.Run(runCtx)
		close(runDone)
	}()
	defer func() {
		stopReports()
		<-runDone
	}()
	var logs bytes.Buffer
	reports.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	references := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reference := supportreport.NewReference()
		references <- reference
		gateway.ServeEdgeControl(w, r.WithContext(reports.Context(r.Context(), reference)), r.Header.Get("X-Node"), r.Header.Get("X-Epoch"))
	}))
	defer server.Close()
	connect := func() (*websocket.Conn, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		header := http.Header{"X-Node": {"edge_1"}, "X-Epoch": {"epoch_1"}}
		connection, _, dialErr := websocket.Dial(ctx, "ws://"+strings.TrimPrefix(server.URL, "http://"), &websocket.DialOptions{HTTPHeader: header})
		if dialErr != nil {
			cancel()
			return nil, nil, nil, dialErr
		}
		return connection, ctx, cancel, nil
	}
	request := func(value string) []byte {
		raw, marshalErr := json.Marshal(EdgeControlRequest{Ticket: value, Origin: ticket.Origin, Host: "machine.runtime.example.test"})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		return raw
	}

	failed, failedCtx, cancelFailed, err := connect()
	if err != nil {
		t.Fatal(err)
	}
	failedReference := <-references
	if err = failed.Write(failedCtx, websocket.MessageText, request("failed-ticket")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = failed.Read(failedCtx); err == nil {
		t.Fatal("operational admission failure opened a connection")
	}
	failed.CloseNow()
	cancelFailed()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		store.mu.Lock()
		captured := len(store.records) > 0
		store.mu.Unlock()
		if captured {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	store.mu.Lock()
	if len(store.records) != 1 {
		store.mu.Unlock()
		t.Fatalf("captured records = %d; want one", len(store.records))
	}
	record := store.records[0]
	store.mu.Unlock()
	if record.Reference != failedReference || record.Cause != "connection_refused" || record.Errno != int(syscall.ECONNREFUSED) {
		t.Fatalf("fault record lost correlation or cause: %+v", record)
	}
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), failedReference) || !strings.Contains(logs.String(), "connection_refused") {
		t.Fatalf("local fault log lost safe diagnostics or exposed helper detail: %s", logs.String())
	}

	authorizer.mu.Lock()
	authorizer.snapshotErr = nil
	authorizer.mu.Unlock()
	tickets.mu.Lock()
	tickets.used = false
	tickets.value = "fresh-ticket"
	tickets.mu.Unlock()
	recovered, recoveredCtx, cancelRecovered, err := connect()
	if err != nil {
		t.Fatal(err)
	}
	recoveredReference := <-references
	if recoveredReference == failedReference {
		t.Fatal("separate admission requests reused a support reference")
	}
	if err = recovered.Write(recoveredCtx, websocket.MessageText, request("fresh-ticket")); err != nil {
		t.Fatal(err)
	}
	_, payload, err := recovered.Read(recoveredCtx)
	if err != nil {
		t.Fatal("fresh admission did not recover:", err)
	}
	var admitted struct {
		Credential        string `json:"credential"`
		TerminalSessionID string `json:"terminal_session_id"`
	}
	if err = json.Unmarshal(payload, &admitted); err != nil || admitted.Credential == "" || admitted.TerminalSessionID != ticket.TerminalSessionID {
		t.Fatalf("recovered admission = %+v, decode error %v", admitted, err)
	}
	recovered.CloseNow()
	cancelRecovered()
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 {
		t.Fatalf("successful recovery added a failure record: %d", len(store.records))
	}
}

// A bandwidth refusal at admission is an expected rejection with its own reason, not an
// unexpected failure filed as a support report.
func TestBandwidthRefusalIsAnExpectedAdmissionRejection(t *testing.T) {
	err := fmt.Errorf("record usage admission: %w", &usermachines.BandwidthQuotaError{BillingURL: "https://console.example.test/billing"})
	if !expectedTerminalRejection(err) || edgeAdmissionReason(err) != "bandwidth_quota_exhausted" {
		t.Fatalf("expected=%v reason=%q", expectedTerminalRejection(err), edgeAdmissionReason(err))
	}
}
