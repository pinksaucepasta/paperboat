package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// TailnetTerminalTunnel adapts the real terminal protocol to a direct native
// session. DialSession owns network refresh/reconnect policy in the daemon.
type TailnetTerminalTunnel struct {
	DialSession       func(context.Context, resolver.ConnectInfo) (*native.Session, error)
	OutputQueueChunks int
}

func (t TailnetTerminalTunnel) Dial(ctx context.Context, info resolver.ConnectInfo) (Conn, error) {
	if t.DialSession == nil || info.Terminal == nil || info.Terminal.Auth.Token == "" || info.Terminal.Auth.ResourceID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	session, err := t.DialSession(ctx, info)
	if err != nil {
		return nil, err
	}
	operationID := info.Terminal.SessionID
	if operationID == "" {
		operationID = "operation_terminal_attach"
	}
	group := &tailnetTerminalStreams{session: session, operationID: operationID, consumer: "terminal", credential: info.Terminal.Auth.Token, resourceID: info.Terminal.Auth.ResourceID, deadline: info.Terminal.Auth.ExpiresAt}
	message, err := authenticateUnifiedNativeStream(ctx, group, info.Terminal, "direct Tailnet")
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	queue := t.OutputQueueChunks
	if queue < 1 {
		queue = terminalOutputQueueChunks
	}
	connection, err := newInitializedHelperTerminalConn(ctx, message, info.Terminal, queue)
	if err != nil {
		return nil, errors.Join(err, message.Close())
	}
	return connection, nil
}

// DialExec runs the exec application protocol over the same native session
// adapter used by terminal integration. Production callers normally reach this
// path through the local daemon, but keeping this adapter complete lets the
// real native-path acceptance exercise exec without bypassing its framing or
// authorization boundary.
func (t TailnetTerminalTunnel) DialExec(ctx context.Context, info resolver.ConnectInfo, request ExecRequest) (ExecConn, error) {
	if t.DialSession == nil || info.Terminal == nil || info.Terminal.Auth.Token == "" || info.Terminal.Auth.ResourceID == "" || request.OperationID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	session, err := t.DialSession(ctx, info)
	if err != nil {
		return nil, err
	}
	group := &tailnetTerminalStreams{session: session, operationID: request.OperationID, consumer: "exec", credential: info.Terminal.Auth.Token, resourceID: info.Terminal.Auth.ResourceID, deadline: info.Terminal.Auth.ExpiresAt}
	message, err := authenticateUnifiedNativeStream(ctx, group, info.Terminal, "direct Tailnet")
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	exec := &helperExecConn{message: message, target: info.Terminal, request: request, events: make(chan ExecEvent, 256), done: make(chan struct{}), pending: make(map[string]chan helperFrame)}
	if err := exec.initialize(ctx); err != nil {
		return nil, errors.Join(err, message.Close())
	}
	return exec, nil
}

// DialSSH carries the system OpenSSH byte stream over one authorized native
// stream. Host-key verification, authentication, forwarding, SCP and SFTP all
// remain owned by OpenSSH on either side of this adapter.
func (t TailnetTerminalTunnel) DialSSH(ctx context.Context, info resolver.ConnectInfo, operationID string) (Conn, error) {
	if t.DialSession == nil || info.Terminal == nil || info.Terminal.Auth.Token == "" || info.Terminal.Auth.ResourceID == "" || operationID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	session, err := t.DialSession(ctx, info)
	if err != nil {
		return nil, err
	}
	group := &tailnetTerminalStreams{session: session, operationID: operationID, consumer: "ssh", credential: info.Terminal.Auth.Token, resourceID: info.Terminal.Auth.ResourceID, deadline: info.Terminal.Auth.ExpiresAt}
	stream, err := group.OpenStream(ctx)
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return &sshStreamConn{ReadWriteCloser: stream}, nil
}

// DialCodexHTTP opens one authenticated HTTP/WebSocket connection through the
// unified native session. Codex reconnects by opening a fresh connection with
// the same server-owned session ID; an indeterminate stream is never replayed.
func (t TailnetTerminalTunnel) DialCodexHTTP(ctx context.Context, info resolver.ConnectInfo) (net.Conn, error) {
	if t.DialSession == nil || info.Terminal == nil || info.Terminal.SessionID == "" || info.Terminal.Auth.Token == "" || info.Terminal.Auth.ResourceID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	session, err := t.DialSession(ctx, info)
	if err != nil {
		return nil, err
	}
	group := &tailnetTerminalStreams{session: session, operationID: info.Terminal.SessionID, consumer: "codex", credential: info.Terminal.Auth.Token, resourceID: info.Terminal.Auth.ResourceID, deadline: info.Terminal.Auth.ExpiresAt}
	stream, err := group.OpenStream(ctx)
	if err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return &codexHTTPConn{Conn: &sshStreamConn{ReadWriteCloser: stream}}, nil
}

type tailnetTerminalStreams struct {
	session                                                 *native.Session
	operationID, consumer, credential, resourceID, deadline string
}

func (g *tailnetTerminalStreams) OpenStream(ctx context.Context) (nativeStream, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return nil, err
	}
	deadline, err := time.Parse(time.RFC3339, g.deadline)
	if err != nil {
		return nil, err
	}
	header, err := streamauth.New(g.operationID, g.consumer, hex.EncodeToString(random[:]), g.credential, deadline, 1<<40)
	if err != nil {
		return nil, err
	}
	capability := g.consumer
	if capability == "exec" || capability == "ssh" {
		capability = "terminal"
	}
	connection, err := g.session.OpenAuthorized(ctx, header, g.resourceID, capability)
	if err != nil {
		return nil, err
	}
	return connection, nil
}
func (g *tailnetTerminalStreams) Close() error { return g.session.Close() }

var _ Tunnel = TailnetTerminalTunnel{}
