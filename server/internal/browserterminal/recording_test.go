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
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

const testRecordingID = "rec_01234567-89ab-cdef-0123-456789abcdef"

type recordingTestAuthorizer struct {
	testBrowserAuthorizer
}

func (a *recordingTestAuthorizer) ResolveRecording(_ context.Context, userID, recordingID string) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if userID != a.decision.UserID || recordingID != a.decision.RecordingID {
		return Decision{}, ErrForbidden
	}
	return a.decision, nil
}

func recordingDecision() Decision {
	return Decision{WorkspaceID: "team_1", UserID: "usr_1", OwnerAccountID: "usr_1", RecordingID: testRecordingID, MachineID: "mch_1", EnvironmentID: "env_1", HostWSSURL: "wss://machine.runtime.example.test/v1/browser-terminal", EdgeNodeID: "edge_1", EdgeProcessEpoch: "epoch_1", EdgeEndpointHost: "edge-one.example.test", Role: RoleRecording, PolicyGeneration: 1}
}

// A recording replay channel: the ticket names one recording, the edge admission mints a recording_replay
// credential bound to the browser attachment and key, and losing replay permission closes the channel.
func TestEdgeAdmitsRecordingReplayChannelAndClosesItWhenAccessEnds(t *testing.T) {
	tickets := &recordingTickets{}
	authorizer := &recordingTestAuthorizer{testBrowserAuthorizer{decision: recordingDecision()}}
	issuing := &Gateway{Tickets: tickets, Authorizer: authorizer, AllowedOrigins: []string{"https://dashboard.example.test"}}
	issued, streamURL, _, err := issuing.IssueRecording(context.Background(), "usr_1", "browser_1", testRecordingID, "https://dashboard.example.test", testBrowserKeyDigest)
	if err != nil || issued.Role != RoleRecording || streamURL != "wss://edge-one.example.test/v1/browser-terminal/machine.runtime.example.test" {
		t.Fatalf("issue = %+v %q %v", issued, streamURL, err)
	}
	if got := tickets.issued; got.RecordingID != testRecordingID || got.TerminalSessionID != "" || got.InspectorCredentialID != "" {
		t.Fatalf("stored ticket bindings = %#v", got)
	}
	plain := &Gateway{Tickets: tickets, Authorizer: &testBrowserAuthorizer{decision: recordingDecision()}, AllowedOrigins: []string{"https://dashboard.example.test"}}
	if _, _, _, err := plain.IssueRecording(context.Background(), "usr_1", "browser_1", testRecordingID, "https://dashboard.example.test", testBrowserKeyDigest); err != ErrForbidden {
		t.Fatalf("terminal authorizer admitted a recording ticket: %v", err)
	}

	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ticket := Ticket{UserID: "usr_1", BrowserSessionID: "ses_1", RecordingID: testRecordingID, Origin: "https://dashboard.example.test", Role: RoleRecording, PolicyGeneration: 1, BrowserPublicKeySHA256: testBrowserKeyDigest}
	gateway := &Gateway{Tickets: &edgeTestTickets{active: true, ticket: ticket, value: "recording-ticket"}, Authorizer: authorizer, Signer: signer, Issuer: "https://api.example.test", AllowedOrigins: []string{ticket.Origin}}
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
	request, _ := json.Marshal(EdgeControlRequest{Ticket: "recording-ticket", Origin: ticket.Origin, Host: "machine.runtime.example.test"})
	if err := connection.Write(ctx, websocket.MessageText, request); err != nil {
		t.Fatal(err)
	}
	_, payload, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var admitted struct {
		Credential   string `json:"credential"`
		AttachmentID string `json:"attachment_id"`
	}
	if err := json.Unmarshal(payload, &admitted); err != nil || admitted.AttachmentID == "" {
		t.Fatalf("admission %s: %v", payload, err)
	}
	claims, err := signer.VerifyCredential(admitted.Credential, gateway.Issuer, recording.CredentialClass, time.Now())
	if err != nil || claims.MachineID != "mch_1" || claims.AccountID != "usr_1" || claims.WorkspaceID != "team_1" || claims.EnvironmentID != "env_1" ||
		claims.ResourceKind != recording.ResourceKind || claims.ResourceID != testRecordingID || claims.CLIClientSessionID != "" ||
		claims.BrowserAttachmentID != admitted.AttachmentID || claims.BrowserPublicKeySHA256 != testBrowserKeyDigest || claims.Scopes[0] != recording.Scope {
		t.Fatalf("recording credential claims %#v: %v", claims, err)
	}
	authorizer.mu.Lock()
	authorizer.decision.RecordingID = "rec_ffffffff-89ab-cdef-0123-456789abcdef" // replay permission gone
	authorizer.mu.Unlock()
	for {
		if _, data, err := connection.Read(ctx); err != nil {
			break
		} else if string(data) != "ok" {
			t.Fatalf("unexpected control message %q", data)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("revoked recording channel stayed open")
	}
}
