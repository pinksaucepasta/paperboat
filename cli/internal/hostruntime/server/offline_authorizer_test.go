package server

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativeproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

type offlineTestConn struct {
	net.Conn
	consumer string
}

func (c offlineTestConn) OfflinePeer() (string, string, string, string, time.Time) {
	return "acct_peer", "cli_peer", c.consumer, "offline-op-1", time.Now().Add(time.Minute)
}

// A stream admitted offline is authorized from its peer for its one session only, and an
// online stream never runs without a credential (F21-11).
func TestOfflineStreamsAuthorizeOnlyTheirOwnSession(t *testing.T) {
	m := &NativeAssociationManager{config: NativeAssociationConfig{Authorizer: func(token string) (Authorizer, error) {
		return &offlineAuthorizer{consumer: "exec", operationID: "online:" + token, accountID: "a", endpointID: "e", deadline: time.Now().Add(time.Minute)}, nil
	}}}
	plain, _ := net.Pipe()
	defer plain.Close()
	if _, err := m.controlAuthorizer(plain, nativeproto.Preface{}); err == nil {
		t.Fatal("an online stream without a credential got an authorizer")
	}
	if _, err := m.controlAuthorizer(offlineTestConn{Conn: plain, consumer: "exec"}, nativeproto.Preface{Token: "smuggled"}); err == nil {
		t.Fatal("an offline stream with a credential got an authorizer")
	}
	execAuth, err := m.controlAuthorizer(offlineTestConn{Conn: plain, consumer: "exec"}, nativeproto.Preface{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := execAuth.Authorize(ctx, protocol.Frame{Capability: "exec.v1", OperationID: "offline-op-1"})
	if err != nil || got.AccountID != "acct_peer" || got.ClientID != "cli_peer" || got.JournalBinding == "" {
		t.Fatalf("offline exec authorization = %+v, %v", got, err)
	}
	for _, frame := range []protocol.Frame{{Capability: "exec.v1", OperationID: "other-op"}, {Capability: "terminal.v1"}, {Capability: "file-transfer.v1"}, {Capability: "preview.launch.v1"}} {
		if _, err := execAuth.Authorize(ctx, frame); err == nil {
			t.Fatalf("an offline exec stream authorized %+v", frame)
		}
	}
	terminalAuth, err := m.controlAuthorizer(offlineTestConn{Conn: plain, consumer: "terminal"}, nativeproto.Preface{})
	if err != nil {
		t.Fatal(err)
	}
	got, err = terminalAuth.Authorize(ctx, protocol.Frame{Capability: "terminal.v1"})
	if err != nil || got.SessionID != "offline-op-1" || got.TerminalRole != TerminalRoleOwner {
		t.Fatalf("offline terminal authorization = %+v, %v (it must be bound to its own new session)", got, err)
	}
	if _, err := terminalAuth.Authorize(ctx, protocol.Frame{Capability: "exec.v1", OperationID: "offline-op-1"}); err == nil {
		t.Fatal("an offline terminal stream authorized exec")
	}
	if _, err := m.controlAuthorizer(offlineTestConn{Conn: plain, consumer: "ssh"}, nativeproto.Preface{}); err == nil {
		t.Fatal("an offline ssh stream reached the hostd protocol")
	}
}

// A machine whose clock is 10 minutes ahead judges the peer's deadline, which the peer stamped on the server's clock, on
// the server's clock too: a deadline a minute away is open and one a minute gone is closed (X-27).
func TestOfflineAuthorizerJudgesTheDeadlineOnTheServerClock(t *testing.T) {
	sent := time.Now()
	serverNow := sent.Add(-10 * time.Minute) // this machine's clock is 10 minutes ahead of the server's
	serverclock.Observe(serverNow.UTC().Format(http.TimeFormat), sent, sent)
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	frame := protocol.Frame{Capability: "exec.v1", OperationID: "op"}
	open := &offlineAuthorizer{consumer: "exec", operationID: "op", accountID: "a", endpointID: "e", deadline: serverNow.Add(time.Minute)}
	if _, err := open.Authorize(context.Background(), frame); err != nil {
		t.Fatalf("a deadline one minute away on the server was refused: %v", err)
	}
	closed := &offlineAuthorizer{consumer: "exec", operationID: "op", accountID: "a", endpointID: "e", deadline: serverNow.Add(-time.Minute)}
	if _, err := closed.Authorize(context.Background(), frame); err == nil {
		t.Fatal("a deadline one minute gone on the server was accepted")
	}
}
