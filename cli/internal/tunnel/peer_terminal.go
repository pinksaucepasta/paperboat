package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

var ErrPeerTerminalInvalid = errors.New("invalid peer terminal tunnel configuration")

type PeerTerminalConfig struct {
	Issuer            string
	Store             config.ProfileStore
	Auth              config.AuthSource
	TLS               *tls.Config
	HTTPClient        *http.Client
	OutputQueueChunks int
	Now               func() time.Time
}

// PeerTerminalTunnel owns native client authority and sessions for the daemon.
type PeerTerminalTunnel struct {
	config         PeerTerminalConfig
	authorities    *clientauthority.Cache
	nativeMu       sync.Mutex
	nativeRuntime  *cliNativeRuntime
	nativeClosed   bool
	lifetime       context.Context
	cancelLifetime context.CancelFunc
	warmWG         sync.WaitGroup
}

func NewPeerTerminalTunnel(config PeerTerminalConfig) (*PeerTerminalTunnel, error) {
	if config.Issuer == "" || config.Store.Path == "" || config.Store.Secrets == nil || config.Auth == nil || config.TLS == nil {
		return nil, ErrPeerTerminalInvalid
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &PeerTerminalTunnel{config: config, authorities: clientauthority.NewCache(), lifetime: lifetime, cancelLifetime: cancel}, nil
}

// WarmMachines refreshes bounded, reusable authority metadata for online machines.
func (t *PeerTerminalTunnel) WarmMachines(ctx context.Context, machines []api.UserMachine) error {
	if t == nil || ctx == nil {
		return ErrPeerTerminalInvalid
	}
	t.nativeMu.Lock()
	if t.nativeClosed {
		t.nativeMu.Unlock()
		return net.ErrClosed
	}
	t.warmWG.Add(1)
	t.nativeMu.Unlock()
	defer t.warmWG.Done()
	credential, err := t.config.Auth.Credential()
	if err != nil {
		return err
	}
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return err
	}
	client := api.New(t.config.Issuer, credential, t.config.HTTPClient)
	warmCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	stopClose := context.AfterFunc(t.lifetime, cancel)
	defer func() { stopClose(); cancel() }()
	reachable := false
	for _, machine := range machines {
		if machine.Online && machine.InstallationGeneration > 0 && machine.State != "revoked" && machine.State != "deleted" {
			reachable = true
			break
		}
	}
	if !reachable {
		return nil
	}
	// Remote peers are authenticated by the native network bindings, so one verified local
	// identity serves every machine; warming it is the only lookup to make.
	authority, err := t.authorities.ResolveLocal(warmCtx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, Now: t.config.Now().UTC()})
	if err != nil {
		return err
	}
	authority.Clear()
	return warmCtx.Err()
}

func (t *PeerTerminalTunnel) Close() error {
	if t == nil {
		return nil
	}
	t.nativeMu.Lock()
	if t.nativeClosed {
		t.nativeMu.Unlock()
		return nil
	}
	t.nativeClosed = true
	runtime := t.nativeRuntime
	t.nativeRuntime = nil
	t.nativeMu.Unlock()
	if t.cancelLifetime != nil {
		t.cancelLifetime()
	}
	t.warmWG.Wait()
	var err error
	if runtime != nil {
		err = runtime.Close()
	}
	if t.authorities != nil {
		t.authorities.Close()
	}
	return err
}

func (t *PeerTerminalTunnel) InvalidateMachine(machineID string) {
	if t != nil && machineID != "" && t.authorities != nil {
		t.authorities.InvalidateMachine(machineID)
	}
}

func (t *PeerTerminalTunnel) Dial(ctx context.Context, info resolver.ConnectInfo) (Conn, error) {
	return t.dial(ctx, info, "terminal", peerApplication{helper: func(attachCtx context.Context, message helperMessageConnection, target *resolver.TerminalTarget) (Conn, error) {
		return newInitializedHelperTerminalConn(attachCtx, message, target, t.outputQueueChunks())
	}})
}

func (t *PeerTerminalTunnel) dial(ctx context.Context, info resolver.ConnectInfo, consumer string, application peerApplication) (Conn, error) {
	if t == nil || ctx == nil || info.TargetKind != "machine" || info.MachineID == "" || info.MachineGeneration == 0 || info.Terminal == nil || info.Terminal.EnvironmentID == "" || info.Terminal.Auth.ResourceID == "" && !info.Terminal.Auth.Offline || consumer == "" || (application.helper == nil) == (application.raw == nil) || application.raw != nil && application.stream == "" || application.helper != nil && application.stream != "" {
		return nil, ErrPeerTerminalInvalid
	}
	credential, err := t.config.Auth.Credential()
	if err != nil {
		return nil, err
	}
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return nil, err
	}
	client := api.New(t.config.Issuer, credential, t.config.HTTPClient)
	authority, err := t.authorities.ResolveLocal(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, Now: t.config.Now().UTC()})
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextOperationError(ctx)
		}
		return nil, err
	}
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, authority, hostRulesAdmission(info.Terminal))
	if !consumed {
		authority.Clear()
	}
	if err != nil {
		return nil, &terminalTransportError{transport: "native runtime", cause: err}
	}
	connection, err := runtime.openApplication(ctx, info.MachineID, consumer, application, info.Terminal, t.config.Now)
	if err != nil {
		return nil, &terminalTransportError{transport: "native application", cause: err}
	}
	return connection, nil
}

// hostRulesAdmission lets a stream the host admits from its own signed rules (every SSH stream, D1; any stream
// while Paperboat is unreachable) reuse the running engine as is: no descriptor created a new grant, so the
// per-stream network refresh would only cost a control request (F5-07) and fail offline (F5-08).
func hostRulesAdmission(target *resolver.TerminalTarget) func(*tailnet.Authority) bool {
	if target == nil || !target.Auth.Offline {
		return nil
	}
	return func(*tailnet.Authority) bool { return true }
}

// DirectTransferStreamOpener exposes only an authorized application stream.
type DirectTransferStreamOpener interface {
	OpenTransferStream(context.Context) (net.Conn, error)
}

type peerApplicationFactory func(context.Context, helperMessageConnection, *resolver.TerminalTarget) (Conn, error)
type peerRawApplicationFactory func(context.Context, io.ReadWriteCloser) (Conn, error)

type peerApplication struct {
	helper      peerApplicationFactory
	raw         peerRawApplicationFactory
	stream      string
	operationID string
}

func (a peerApplication) authorizationHeader(target *resolver.TerminalTarget, consumer, fallbackOperation, streamID string, now time.Time) (streamauth.Header, error) {
	if target == nil || (target.Auth.Token == "") != target.Auth.Offline {
		return streamauth.Header{}, ErrPeerTerminalInvalid
	}
	operationID := a.operationID
	if operationID == "" {
		operationID = fallbackOperation
	}
	deadline, err := time.Parse(time.RFC3339, target.Auth.ExpiresAt)
	if err != nil || !deadline.After(now) {
		return streamauth.Header{}, ErrPeerTerminalInvalid
	}
	maximum := uint64(1 << 40)
	if consumer == "config_compare" {
		maximum = 220 << 20
	}
	var header streamauth.Header
	if target.Auth.Offline {
		header, err = streamauth.NewOffline(operationID, consumer, streamID, deadline, maximum)
	} else {
		header, err = streamauth.New(operationID, consumer, streamID, target.Auth.Token, deadline, maximum)
	}
	if err == nil {
		header.Resumable = consumer != "config_compare"
		if consumer == "config_compare" {
			header.UsageSessionID = target.Auth.UsageSessionID
		}
	}
	return header, err
}

func (t *PeerTerminalTunnel) outputQueueChunks() int {
	if t.config.OutputQueueChunks > 0 {
		return t.config.OutputQueueChunks
	}
	return terminalOutputQueueChunks
}
