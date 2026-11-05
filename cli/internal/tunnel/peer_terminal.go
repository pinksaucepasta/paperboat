package tunnel

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
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
	// LocalMachineID is the machine the daemon runs on, if enrolled (see tailnet.AuthorityOptions).
	LocalMachineID string
	// Generation is this daemon generation (32 lowercase hex, its unit's name). Each generation has its own network
	// endpoint (peernetwork.DaemonEndpointID), so an upgraded daemon serves new connects while the old one drains.
	Generation string
	// OnIdentityRevoked is called once when the server refuses this generation's identity: "generation_evicted" (a
	// newer generation pushed it out of the session's limit) or "revoked" (session, account or drain cap). Every
	// stream it held ends then. Never called after Withdraw.
	OnIdentityRevoked func(reason string)
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
	revokedOnce    sync.Once
	withdrawn      atomic.Bool
	// retiring is set by BeginDrain, Withdraw or a refused identity: this generation keeps the runtime it has but
	// never starts or registers a new one, so a retired generation cannot revive itself and evict a newer one.
	retiring atomic.Bool
}

func NewPeerTerminalTunnel(config PeerTerminalConfig) (*PeerTerminalTunnel, error) {
	if config.Issuer == "" || config.Store.Path == "" || config.Store.Secrets == nil || config.Auth == nil || config.TLS == nil || !peernetwork.ValidDaemonGeneration(config.Generation) {
		return nil, ErrPeerTerminalInvalid
	}
	if config.Now == nil {
		config.Now = func() time.Time { return serverclock.Now().UTC() }
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

// networkAPI is the server's peer-network API as this daemon generation.
func (t *PeerTerminalTunnel) networkAPI() cliNativeNetworkAPI {
	return cliNativeNetworkAPI{issuer: t.config.Issuer, auth: t.config.Auth, http: t.config.HTTPClient, generation: t.config.Generation, refused: t.identityRefused}
}

func (t *PeerTerminalTunnel) identityRefused(reason string) {
	if t.withdrawn.Load() {
		return
	}
	t.retiring.Store(true)
	t.revokedOnce.Do(func() {
		diagnosticlog.TryWarn("this pb daemon generation's network identity was refused; the streams it held end", "reason", reason, "generation", t.config.Generation)
		if t.config.OnIdentityRevoked != nil {
			t.config.OnIdentityRevoked(reason)
		}
	})
}

// BeginDrain tells the server this generation stopped accepting and drains what it holds (for at most
// runtimeattachment.DrainLimit). The tunnel keeps renewing its network configuration meanwhile, so held streams stay
// admitted until they end, Withdraw, or a revocation (OnIdentityRevoked). Repeating it is harmless.
func (t *PeerTerminalTunnel) BeginDrain(ctx context.Context) error {
	if t == nil {
		return ErrPeerTerminalInvalid
	}
	t.retiring.Store(true)
	return t.generationStep(ctx, (*api.Client).BeginPeerNetworkDrain)
}

// Withdraw revokes this generation's network identity once it finished draining (its machines drop it) and removes
// the generation's local key custody and cached configuration. It returns nil when the identity is already revoked
// (withdrawn before, evicted, or the session ended).
func (t *PeerTerminalTunnel) Withdraw(ctx context.Context) error {
	if t == nil {
		return ErrPeerTerminalInvalid
	}
	t.withdrawn.Store(true)
	t.retiring.Store(true)
	err := t.generationStep(ctx, (*api.Client).WithdrawPeerNetwork)
	if err != nil && refusalReason(err) == "" {
		return err
	}
	// The identity is gone on the server: its key custody and cached configuration go too, so an upgrade leaves
	// nothing of the old generation behind.
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return err
	}
	endpointID, ok := peernetwork.DaemonEndpointID(profile.CLIClientSessionID, t.config.Generation)
	if !ok {
		return ErrPeerTerminalInvalid
	}
	return t.config.Store.DeletePeerNetworkState(t.config.Issuer, profile.Account.ID, endpointID)
}

func (t *PeerTerminalTunnel) generationStep(ctx context.Context, step func(*api.Client, context.Context, api.PeerNetworkGeneration) error) error {
	if t == nil || ctx == nil {
		return ErrPeerTerminalInvalid
	}
	client, err := t.networkAPI().client()
	if err != nil {
		return err
	}
	return step(client, ctx, api.PeerNetworkGeneration{DaemonGeneration: t.config.Generation})
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
	authorityEnd := diagnostics.ReconnectStep(ctx, "local_authority")
	authority, err := t.authorities.ResolveLocal(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, Now: t.config.Now().UTC()})
	authorityEnd(err)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contextOperationError(ctx)
		}
		return nil, err
	}
	runtimeEnd := diagnostics.ReconnectStep(ctx, "runtime_acquire")
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, authority, hostRulesAdmission(info.Terminal, info.MachineID, nativeCapability(consumer)))
	runtimeEnd(err)
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

// Reuse signed, live peer authority like Tailscale's cached netmap. The new
// stream still checks its credential, scope, peer identity and revocation fence.
// Each reconnect grant has a new access-session resource ID (server
// usermachines/service_03.go); a cached peer cannot authorize that future ID.
func hostRulesAdmission(target *resolver.TerminalTarget, machineID, capability string) func(*tailnet.Authority) bool {
	if target == nil {
		return nil
	}
	if target.Auth.Offline {
		return func(*tailnet.Authority) bool { return true }
	}
	return func(network *tailnet.Authority) bool {
		_, err := network.Descriptor(machineID)
		if err != nil {
			diagnosticlog.TryInfo("native cache admission", "reason", "descriptor_rejected", "error_type", strings.TrimPrefix(fmt.Sprintf("%T", err), "*"))
			return false
		}
		allowed := network.Allows(machineID, target.Auth.ResourceID, capability, "dial")
		diagnosticlog.TryInfo("native cache admission", "reason", "exact_scope_check", "allowed", allowed, "capability", capability, "resource_present", target.Auth.ResourceID != "")
		return allowed
	}
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
