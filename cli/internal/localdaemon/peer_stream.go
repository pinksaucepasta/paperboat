package localdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func TunnelPeerStreamOpener(peerTunnel *tunnel.PeerTerminalTunnel) func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error) {
	return func(ctx context.Context, _ localapi.Peer, request localapi.PeerStreamRequest) (net.Conn, error) {
		started := time.Now()
		if ctx == nil || peerTunnel == nil || request.Validate(peerTunnelNow()) != nil {
			return nil, ErrInvalidInventoryConfig
		}
		var terminalPayload localapi.PeerTerminalPayload
		if len(request.Payload) > 0 && request.Consumer != "exec" && request.Consumer != "private_preview" {
			if err := json.Unmarshal(request.Payload, &terminalPayload); err != nil {
				return nil, err
			}
		}
		cursor := &tunnel.LocalPeerCursorBridge{}
		target := &resolver.TerminalTarget{Protocol: terminalPayload.Protocol, Debug: terminalPayload.Debug, EnvironmentID: request.EnvironmentID, Auth: resolver.AuthTarget{Scopes: terminalPayload.Scopes, Token: request.Credential, ExpiresAt: request.Deadline.UTC().Format("2006-01-02T15:04:05Z07:00"), ResourceID: request.AccessSessionID}, ThreadID: terminalPayload.ThreadID, TerminalID: terminalPayload.TerminalID, SessionID: terminalPayload.SessionID, CWD: terminalPayload.CWD, Env: terminalPayload.Environment, Cols: terminalPayload.Columns, Rows: terminalPayload.Rows, RestartIfNotRunning: terminalPayload.RestartIfNotRunning, ReplayHistory: terminalPayload.ReplayHistory, AfterSequence: terminalPayload.AfterSequence, InputAttachmentID: terminalPayload.InputAttachmentID, SequenceSink: cursor.RecordSequence, ReplayGapSink: cursor.RecordReplayGap}
		target.QUICEndpoint, target.WSSEndpoint = request.QUICEndpoint, request.WSSEndpoint
		info := resolver.ConnectInfo{TargetKind: "machine", ProjectID: request.MachineID, MachineGeneration: request.MachineGeneration, Terminal: target}
		// Setup is part of the local API request and must stop when the caller
		// cancels or its deadline expires. Once the HTTP handler upgrades, the
		// returned stream becomes daemon-owned and is governed by its lease.
		lifetime, cancelLifetime := context.WithCancel(context.Background())
		var handoffMu sync.Mutex
		handedOff := false
		stopCallerCancel := context.AfterFunc(ctx, func() {
			handoffMu.Lock()
			defer handoffMu.Unlock()
			if !handedOff {
				cancelLifetime()
			}
		})
		deadlineTimer := time.AfterFunc(time.Until(request.Deadline), cancelLifetime)
		stopSetupCancellation := func() bool {
			handoffMu.Lock()
			handedOff = true
			handoffMu.Unlock()
			stopCallerCancel()
			return deadlineTimer.Stop() && ctx.Err() == nil && lifetime.Err() == nil && time.Now().Before(request.Deadline)
		}
		var remote tunnel.Conn
		var err error
		diagnosticlog.TryInfo("local peer dial starting", "consumer", request.Consumer, "machine_id", request.MachineID)
		dial := func() (tunnel.Conn, error) {
			switch request.Consumer {
			case "terminal":
				return peerTunnel.Dial(lifetime, info)
			case "exec":
				var value tunnel.ExecRequest
				if json.Unmarshal(request.Payload, &value) != nil || value.OperationID != request.OperationID {
					return nil, ErrInvalidInventoryConfig
				}
				return peerTunnel.DialExec(lifetime, info, value)
			case "ssh":
				return peerTunnel.DialSSH(lifetime, info, request.OperationID)
			default:
				return nil, ErrInvalidInventoryConfig
			}
		}
		remote, err = dial()
		if errors.Is(err, ErrInvalidInventoryConfig) {
			stopCallerCancel()
			deadlineTimer.Stop()
			cancelLifetime()
			return nil, ErrInvalidInventoryConfig
		}
		diagnosticlog.TryInfo("local peer dial finished", "consumer", request.Consumer, "machine_id", request.MachineID, "elapsed_ms", time.Since(started).Milliseconds(), "error", err)
		if err != nil {
			stopCallerCancel()
			deadlineTimer.Stop()
			cancelLifetime()
			diagnosticlog.TryInfo("local peer stream open failed", "consumer", request.Consumer, "machine_id", request.MachineID, "error", err)
			return nil, err
		}
		if !stopSetupCancellation() {
			cancelLifetime()
			_ = remote.Close()
			return nil, context.Canceled
		}
		diagnosticlog.TryInfo("local peer stream opened", "consumer", request.Consumer, "machine_id", request.MachineID, "elapsed_ms", time.Since(started).Milliseconds())
		if request.Consumer == "ssh" {
			return &rawPeerConn{Conn: remote, cancel: cancelLifetime}, nil
		}
		client, server := net.Pipe()
		go func() {
			defer cancelLifetime()
			if request.Consumer == "terminal" && terminalPayload.Debug {
				_ = tunnel.ServeLocalPeerDebugTerminalConn(lifetime, server, remote, cursor)
			} else {
				_ = tunnel.ServeLocalPeerTerminalConn(lifetime, server, remote, cursor)
			}
		}()
		return client, nil
	}
}

func TunnelPeerProbe(peerTunnel *tunnel.PeerTerminalTunnel) func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
	return func(ctx context.Context, _ localapi.Peer, request localapi.PeerStreamRequest) (localapi.PeerProbeResult, error) {
		if ctx == nil || peerTunnel == nil || request.Consumer != "health_probe" || request.Validate(time.Now().UTC()) != nil {
			return localapi.PeerProbeResult{}, ErrInvalidInventoryConfig
		}
		result, err := peerTunnel.ProbeNative(ctx, request.MachineID, request.MachineGeneration)
		if err != nil {
			diagnosticlog.TryInfo("local peer probe failed", "machine_id", request.MachineID, "error", err)
			if errors.Is(err, tailnet.ErrAdmission) || errors.Is(err, tailnet.ErrAuthority) {
				return localapi.PeerProbeResult{}, errors.Join(localapi.ErrPermission, err)
			}
			return localapi.PeerProbeResult{}, err
		}
		return localapi.PeerProbeResult{Path: result.Path, ConnectionNanoseconds: result.Connection.Nanoseconds()}, nil
	}
}

type rawPeerConn struct {
	tunnel.Conn
	cancel context.CancelFunc
}

func (*rawPeerConn) LocalAddr() net.Addr              { return rawPeerAddr("daemon") }
func (*rawPeerConn) RemoteAddr() net.Addr             { return rawPeerAddr("machine") }
func (*rawPeerConn) SetDeadline(time.Time) error      { return nil }
func (*rawPeerConn) SetReadDeadline(time.Time) error  { return nil }
func (*rawPeerConn) SetWriteDeadline(time.Time) error { return nil }
func (c *rawPeerConn) CloseWrite() error {
	if closer, ok := c.Conn.(tunnel.InputHalfCloser); ok {
		return closer.CloseWrite()
	}
	return net.ErrClosed
}
func (c *rawPeerConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

type rawPeerAddr string

func (a rawPeerAddr) Network() string { return "paperboat-peer" }
func (a rawPeerAddr) String() string  { return string(a) }

func peerTunnelNow() time.Time { return time.Now().UTC() }
