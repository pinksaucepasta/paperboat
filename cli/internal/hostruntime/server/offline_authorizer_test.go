package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativeproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
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
