package localdaemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/admission"
	"net"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
)

func TunnelPeerStreamOpener(peerTunnel *tunnel.PeerTerminalTunnel) func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error) {
	return func(ctx context.Context, _ localapi.Peer, request localapi.PeerStreamRequest) (net.Conn, error) {
		if ctx == nil || peerTunnel == nil || request.Validate(peerTunnelNow()) != nil {
			return nil, ErrInvalidInventoryConfig
		}
		var terminalPayload localapi.PeerTerminalPayload
		if len(request.Payload) > 0 && request.Consumer != "config_compare" && request.Consumer != "exec" && request.Consumer != "private_preview" {
			if err := json.Unmarshal(request.Payload, &terminalPayload); err != nil {
				return nil, err
			}
		}
		cursor := &tunnel.LocalPeerCursorBridge{}
		target := &resolver.TerminalTarget{Protocol: terminalPayload.Protocol, Debug: terminalPayload.Debug, EnvironmentID: request.EnvironmentID, Auth: resolver.AuthTarget{Scopes: terminalPayload.Scopes, Token: request.Credential, ExpiresAt: request.Deadline.UTC().Format("2006-01-02T15:04:05Z07:00"), ResourceID: request.AccessSessionID, UsageSessionID: request.UsageSessionID}, SessionID: terminalPayload.SessionID, CWD: terminalPayload.CWD, Env: terminalPayload.Environment, Cols: terminalPayload.Columns, Rows: terminalPayload.Rows, RestartIfNotRunning: terminalPayload.RestartIfNotRunning, ReplayHistory: terminalPayload.ReplayHistory, AfterSequence: terminalPayload.AfterSequence, InputAttachmentID: terminalPayload.InputAttachmentID, SequenceSink: cursor.RecordSequence, ReplayGapSink: cursor.RecordReplayGap, LaunchWarningSink: cursor.RecordLaunchWarnings}
		target.QUICEndpoint, target.WSSEndpoint = request.QUICEndpoint, request.WSSEndpoint
		info := resolver.ConnectInfo{TargetKind: "machine", MachineID: request.MachineID, MachineGeneration: request.MachineGeneration, Terminal: target}
		// Setup is part of the local API request and must stop when the caller
		// cancels or its deadline expires. Once the HTTP handler upgrades, the
		// returned stream becomes daemon-owned and is governed by its lease.
		staged, err := admission.Stage(ctx, request.Deadline)
		if err != nil {
			return nil, err
		}
		lifetime := staged.Context()
		var remote tunnel.Conn
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
			case "config_compare":
				var value configsync.ConflictComparisonRequest
				if json.Unmarshal(request.Payload, &value) != nil {
					return nil, ErrInvalidInventoryConfig
				}
				return peerTunnel.DialConfigComparison(lifetime, info, request.OperationID, value)
			case "ssh":
				return peerTunnel.DialSSH(lifetime, info, request.OperationID)
			default:
				return nil, ErrInvalidInventoryConfig
			}
		}
		remote, err = dial()
		if errors.Is(err, ErrInvalidInventoryConfig) {
			staged.Abort()
			return nil, ErrInvalidInventoryConfig
		}
		if err != nil {
			staged.Abort()
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "peer_stream", "peer_connect", "transport_failed", err)
			return nil, err
		}
		if _, err := staged.Accept(); err != nil {
			staged.Abort()
			_ = remote.Close()
			if ctx.Err() == nil && request.Consumer == "terminal" && (errors.Is(err, admission.ErrExpired) || errors.Is(err, context.DeadlineExceeded)) {
				return nil, terminalCredentialExpiredError{}
			}
			return nil, err
		}
		if recorder := diagnostics.FromContext(ctx); recorder != nil {
			_ = recorder.RecordWithSupportReference("stream_open", "peer_stream_opened", "info", supportref.FromContext(ctx), map[string]string{"component": "paperboat-daemon", "operation": "peer_stream", "outcome": "success"})
		}
		if request.Consumer == "ssh" || request.Consumer == "config_compare" {
			return &rawPeerConn{Conn: remote, cancel: func() { staged.Abort() }}, nil
		}
		client, server := net.Pipe()
		go func() {
			defer staged.Abort()
			if request.Consumer == "terminal" && terminalPayload.Debug {
				reportLocalPeerBridgeFailure(lifetime, tunnel.ServeLocalPeerDebugTerminalConn(lifetime, server, remote, cursor))
			} else {
				reportLocalPeerBridgeFailure(lifetime, tunnel.ServeLocalPeerTerminalConn(lifetime, server, remote, cursor))
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
			return localapi.PeerProbeResult{}, peerProbeFailure(ctx, err)
		}
		return localapi.PeerProbeResult{Path: result.Path, ConnectionNanoseconds: result.Connection.Nanoseconds()}, nil
	}
}

// inspectorBroker returns nil when the daemon was given no inspector support, so the local API
// refuses the route instead of calling a nil function.
func inspectorBroker(call func(context.Context, localapi.Peer, localapi.InspectorRequest) (localapi.InspectorResult, error)) localapi.InspectorBroker {
	if call == nil {
		return nil
	}
	return localapi.InspectorBrokerFunc(call)
}

// TunnelInspectorRequest runs an inspector call on the daemon's own native peer session.
func TunnelInspectorRequest(peerTunnel *tunnel.PeerTerminalTunnel) func(context.Context, localapi.Peer, localapi.InspectorRequest) (localapi.InspectorResult, error) {
	return func(ctx context.Context, _ localapi.Peer, request localapi.InspectorRequest) (localapi.InspectorResult, error) {
		if ctx == nil || peerTunnel == nil {
			return localapi.InspectorResult{}, ErrInvalidInventoryConfig
		}
		query, err := url.ParseQuery(request.Query)
		if err != nil {
			return localapi.InspectorResult{}, ErrInvalidInventoryConfig
		}
		payload, status, err := peerTunnel.InspectorRequest(ctx, request.Access, request.Grant, request.Method, request.Path, request.Body, query)
		if err != nil {
			return localapi.InspectorResult{}, err
		}
		return localapi.InspectorResult{Status: status, Payload: payload}, nil
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

// A terminal setup may renew its authorized descriptor after expiry. Exec and
// other mutation outcomes retain their existing authoritative classification.
type terminalCredentialExpiredError struct{}

func (terminalCredentialExpiredError) Error() string {
	return "terminal access credential expired during setup"
}
func (terminalCredentialExpiredError) Unwrap() error            { return context.DeadlineExceeded }
func (terminalCredentialExpiredError) TransportRetryable() bool { return true }
