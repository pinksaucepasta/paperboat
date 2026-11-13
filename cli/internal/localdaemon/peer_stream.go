package localdaemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"net"
	"net/url"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
)

// peerStreamOpenTimeout bounds non-terminal stream setup (exec, SSH, config compare), like Tailscale's 10 s DERP connect
// (~/ref/tailscale/derp/derphttp/derphttp_client.go:359); each caller's dial budget retries it.
// On a 30% loss link an open hung ~110 s without it (ltc-j1, peer_stream transport_failed).
const peerStreamOpenTimeout = 10 * time.Second

// An exec open that timed out after its start request was sent may have started the command, so it reads as
// start-uncertain: pb exec retries the same operation ID, which the host runs at most once. One that timed out
// before (the peer never answered the handshake: no path or no relay) started nothing and fails at once as a
// transport failure instead of waiting out the 90 s uncertain-start retry (canary B->A 2026-11-11).
type peerStreamOpenTimeoutError struct {
	cause error
	exec  bool
}

func (e peerStreamOpenTimeoutError) Error() string {
	return "the stream to the machine did not open within 10 s"
}
func (e peerStreamOpenTimeoutError) Unwrap() error            { return e.cause }
func (e peerStreamOpenTimeoutError) TransportRetryable() bool { return true }
func (e peerStreamOpenTimeoutError) LocalAPICode() string {
	if e.exec {
		return "exec_start_uncertain"
	}
	return ""
}

// dialWithin aborts a dial that has not returned after timeout; one that returns after the abort is closed.
func dialWithin(timeout time.Duration, consumer string, abort func(), dial func() (tunnel.Conn, error)) (tunnel.Conn, error) {
	// Terminal setup is bounded by its staged credential/caller context. Native
	// handshake recovery must survive the generic stream watchdog.
	if consumer == "terminal" {
		return dial()
	}
	watchdog := time.AfterFunc(timeout, abort)
	conn, err := dial()
	if watchdog.Stop() {
		return conn, err
	}
	if err == nil {
		_ = conn.Close()
	}
	var notStarted *tunnel.ExecNotStartedError
	return nil, peerStreamOpenTimeoutError{cause: err, exec: consumer == "exec" && !errors.As(err, &notStarted)}
}

// tracedDial gives an exec, ssh or config-compare dial the step records a terminal redial has (peer_descriptor,
// regional_prepare, carrier_dial, quic_handshake with durations), so a dial that stalls names its step. The staged
// lifetime carries neither the recorder nor a trace; a terminal sets up its own.
func tracedDial(request, lifetime context.Context, consumer string) context.Context {
	recorder := diagnostics.FromContext(request)
	if recorder == nil || consumer == "terminal" {
		return lifetime
	}
	lifetime = supportref.WithContext(diagnostics.WithRecorder(lifetime, recorder), supportref.FromContext(request))
	return diagnostics.WithReconnect(lifetime, rand.Text(), "target", rand.Text())
}

// applyResume makes target re-attach the terminal its owner held, with the host's resume secret and the access session
// that attachment was admitted under, instead of a credential. It reports false when this daemon holds no such secret.
func applyResume(target *resolver.TerminalTarget, machineID string, payload localapi.PeerTerminalPayload, now time.Time) bool {
	secret, resource, attachmentID, ok := terminalInputOwners.takeResume(payload.InputOwner, machineID, payload.SessionID, now)
	if !ok {
		return false
	}
	target.Resume, target.ResumeSecret, target.InputAttachmentID = true, secret, attachmentID
	target.Auth.Token, target.Auth.ResourceID = "", resource
	return true
}

// stopping ends when the daemon starts to stop, before it closes its transports.
func TunnelPeerStreamOpener(stopping context.Context, peerTunnel *tunnel.PeerTerminalTunnel) func(context.Context, localapi.Peer, localapi.PeerStreamRequest) (net.Conn, error) {
	return func(ctx context.Context, _ localapi.Peer, request localapi.PeerStreamRequest) (net.Conn, error) {
		if ctx == nil || peerTunnel == nil || request.Validate(peerTunnelNow()) != nil {
			return nil, ErrInvalidInventoryConfig
		}
		var terminalPayload localapi.PeerTerminalPayload
		if len(request.Payload) > 0 && request.Consumer != "config_compare" && request.Consumer != "exec" && request.Consumer != "recording" {
			if err := json.Unmarshal(request.Payload, &terminalPayload); err != nil {
				return nil, err
			}
		}
		cursor := &tunnel.LocalPeerCursorBridge{Stopping: stopping}
		target := &resolver.TerminalTarget{LoginUser: terminalPayload.LoginUser, Protocol: terminalPayload.Protocol, Debug: terminalPayload.Debug, EnvironmentID: request.EnvironmentID, Auth: resolver.AuthTarget{Scopes: terminalPayload.Scopes, Token: request.Credential, ExpiresAt: request.Deadline.UTC().Format("2006-01-02T15:04:05Z07:00"), ResourceID: request.AccessSessionID, UsageSessionID: request.UsageSessionID, Offline: request.Offline}, SessionID: terminalPayload.SessionID, CWD: terminalPayload.CWD, Env: terminalPayload.Environment, Cols: terminalPayload.Columns, Rows: terminalPayload.Rows, RestartIfNotRunning: terminalPayload.RestartIfNotRunning, ReplayHistory: terminalPayload.ReplayHistory, AfterSequence: terminalPayload.AfterSequence, InputAttachmentID: terminalPayload.InputAttachmentID, SequenceSink: cursor.RecordSequence, ReplayGapSink: cursor.RecordReplayGap, LaunchWarningSink: cursor.RecordLaunchWarnings}
		target.QUICEndpoint, target.WSSEndpoint = request.QUICEndpoint, request.WSSEndpoint
		if request.Resume {
			if !applyResume(target, request.MachineID, terminalPayload, time.Now()) {
				return nil, localapi.ResumeUnavailableError{}
			}
		}
		if request.Consumer == "terminal" {
			terminalInputOwners.adopt(terminalPayload.InputOwner, request.MachineID, target, time.Now())
		}
		info := resolver.ConnectInfo{TargetKind: "machine", MachineID: request.MachineID, MachineGeneration: request.MachineGeneration, Terminal: target}
		// Setup is part of the local API request and must stop when the caller
		// cancels or its deadline expires. Once the HTTP handler upgrades, the
		// returned stream becomes daemon-owned and is governed by its lease.
		staged, err := admission.Stage(ctx, request.Deadline)
		if err != nil {
			return nil, err
		}
		lifetime := tracedDial(ctx, staged.Context(), request.Consumer)
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
			case "recording":
				var value struct {
					RecordingID string `json:"recording_id"`
				}
				if json.Unmarshal(request.Payload, &value) != nil || value.RecordingID == "" || value.RecordingID != request.AccessSessionID {
					return nil, ErrInvalidInventoryConfig
				}
				return peerTunnel.DialRecording(lifetime, info, request.OperationID, value.RecordingID)
			case "ssh":
				return peerTunnel.DialSSH(lifetime, info, request.OperationID)
			default:
				return nil, ErrInvalidInventoryConfig
			}
		}
		remote, err = dialWithin(peerStreamOpenTimeout, request.Consumer, staged.Abort, dial)
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
		if request.Consumer == "ssh" || request.Consumer == "config_compare" || request.Consumer == "recording" {
			return &rawPeerConn{Conn: remote, cancel: func() { staged.Abort() }}, nil
		}
		client, server := net.Pipe()
		go func() {
			defer staged.Abort()
			if request.Consumer == "terminal" {
				defer func() { terminalInputOwners.finish(terminalPayload.InputOwner, request.MachineID, target, time.Now()) }()
			}
			if link, ok := remote.(tunnel.RecordingController); ok && request.Consumer == "terminal" {
				defer attachedTerminals.add(link)()
			}
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
		if ctx == nil || peerTunnel == nil || request.Consumer != "health_probe" || request.Validate(serverclock.Now().UTC()) != nil {
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

// inspectorFailure types a target's admission refusal (an invalid or unauthorized grant) as a
// permission failure so the local API answers 403, not a connectivity 503.
func inspectorFailure(err error) error {
	var refusal *native.AdmissionRefusal
	if errors.As(err, &refusal) && refusal.Code == native.AdmissionDenied {
		return localapi.PermissionFailure(err)
	}
	return err
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
			return localapi.InspectorResult{}, inspectorFailure(err)
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

func peerTunnelNow() time.Time { return serverclock.Now().UTC() }

// A terminal setup may renew its authorized descriptor after expiry. Exec and
// other mutation outcomes retain their existing authoritative classification.
type terminalCredentialExpiredError struct{}

func (terminalCredentialExpiredError) Error() string {
	return "terminal access credential expired during setup"
}
func (terminalCredentialExpiredError) Unwrap() error            { return context.DeadlineExceeded }
func (terminalCredentialExpiredError) TransportRetryable() bool { return true }
