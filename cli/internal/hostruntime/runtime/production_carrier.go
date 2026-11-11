//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// ProductionCarrier owns one native connection carrier generation of the
// account. It neither boots a feature worker nor owns terminal, exec, or
// transfer state.
type ProductionCarrier struct {
	ssh *managedssh.Host
	// assemble opens the state files and builds the serving parts; it runs
	// only under the role lock, so one carrier at a time owns that state.
	assemble func() (*servingCarrier, error)
	// roleLock is the file lock of the serving role (empty on Windows: one carrier).
	roleLock string
	// standby is an update's candidate: it prepares nothing that touches state
	// or the network until StartNetwork, after the serving carrier released the role.
	standby bool
	mu      sync.Mutex
	runCtx  context.Context
	serving *servingCarrier
}

// servingCarrier is what a carrier runs while it holds the serving role.
type servingCarrier struct {
	peer          *productionNativePeerService
	authorization Service
	usage         *hostBandwidthService
	unlock        func() error
}

// CarrierGeneration places a carrier process among the account's carrier
// generations (Linux, macOS); Windows runs one carrier and leaves Name empty.
type CarrierGeneration struct {
	// Name is this generation (its source SHA-256 prefix).
	Name string
	// Active says whether the account's descriptor names this generation now;
	// an update's candidate is not active until the update marks it.
	Active func(context.Context) (bool, error)
}

// carrierRole names the serving role's state files and lock: the files have one
// writer, the carrier holding the role, and the next carrier resumes them.
const carrierRole = "carrier-0"

// roleLockWait bounds how long a carrier that is meant to serve waits for the previous one to leave.
const roleLockWait = 30 * time.Second

// NewProductionCarrier loads an already enrolled machine identity. Initial
// enrollment and current SSH target publication remain installer/worker work.
// The active generation serves at once; any other is standby (an update's
// candidate) until StartNetwork.
func NewProductionCarrier(ctx context.Context, version string, environ func(string) string, dispatch *server.NativeDispatchClient, ssh *managedssh.Host, generation CarrierGeneration) (*ProductionCarrier, error) {
	if ctx == nil || environ == nil || dispatch == nil || ssh == nil {
		return nil, ErrProductionInvalid
	}
	base, err := loadCarrierIdentity(ctx, version, environ)
	if err != nil {
		return nil, err
	}
	carrier := &ProductionCarrier{ssh: ssh}
	if generation.Name != "" {
		active, err := generation.Active(ctx)
		if err != nil {
			return nil, err
		}
		carrier.standby = !active
		carrier.roleLock = filepath.Join(base.config.StateRoot, "runtime", carrierRole+".lock")
	}
	if err := allowPendingPeerEnrollment(ctx, base.peerEnrollment); err != nil {
		return nil, err
	}
	carrier.assemble = func() (*servingCarrier, error) { return assembleServing(ctx, base, environ, dispatch, ssh) }
	return carrier, nil
}

func assembleServing(ctx context.Context, base *carrierIdentity, environ func(string) string, dispatch *server.NativeDispatchClient, ssh *managedssh.Host) (_ *servingCarrier, resultErr error) {
	config, controlURL, transport := base.config, base.controlURL, base.transport
	keys, err := productionJWKSCache(controlURL, transport, filepath.Join(config.StateRoot, "authorization", carrierRole+"-jwks.json"))
	if err != nil {
		return nil, err
	}
	usageControl := clientapi.New(controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	usageControl.SetMachineAuth(base.machineIdentity)
	usage, err := bandwidth.Open(filepath.Join(config.StateRoot, carrierRole+"-bandwidth-usage.json"), usageControl, newBandwidthObservation(ctx))
	if err != nil {
		return nil, err
	}
	usageService := &hostBandwidthService{recorder: usage}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, usageService.Shutdown(context.WithoutCancel(ctx)))
		}
	}()
	audit, err := auditCarrierSSH(filepath.Join(config.StateRoot, carrierRole+"-offline-audit.json"), ssh)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	peer, err := newProductionNativePeerService(productionNativePeerConfig{
		controlURL: controlURL.String(), issuer: strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), controlURL.String()), "/"),
		stateRoot: config.StateRoot, machineID: base.identity.MachineID, generation: uint64(base.generation),
		transport: transport, identity: base.machineIdentity, keys: keys, dispatch: dispatch, ssh: ssh, usage: usage, offlineAudit: audit,
		networkEvents: base.events.Subscribe(carrierRenewalTopics...),
	})
	if err != nil {
		return nil, err
	}
	group := serviceGroup{base.events, &jwksRefreshService{cache: keys}, newPeerEnrollmentRuntimeService(base.peerEnrollment, 2*time.Second), newOfflineAuditService(audit, controlURL, base.identity.MachineID, config.StateRoot, transport, base.tokens, base.operationID, base.events.Recovered())}
	return &servingCarrier{peer: peer, authorization: group, usage: usageService}, nil
}

// awaitRoleLock takes the serving role's lock, waiting until ctx ends for the previous holder to leave. Any other
// failure returns at once: a refused lock file used to be retried silently until the deadline (W21, winpool-13).
func awaitRoleLock(ctx context.Context, path string) (func() error, error) {
	for {
		unlock, err := lockCarrierFile(path)
		if err == nil {
			return unlock, nil
		}
		if !carrierLockBusy(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ErrCarrierRoleBusy, ctx.Err(), err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// ErrCarrierRoleBusy means another carrier still serves the machine's network.
var ErrCarrierRoleBusy = errors.New("another carrier still serves this machine; it releases the network at the end of an update")

// auditCarrierSSH queues the SSH sessions the carrier serves (F5-12) in its own backlog file, as for the JWKS cache.
func auditCarrierSSH(path string, ssh *managedssh.Host) (*offlineaudit.Queue, error) {
	audit, err := offlineaudit.Open(path, nil)
	if err != nil {
		return nil, err
	}
	ssh.SetAudit(SSHSessionAudit(audit))
	return audit, nil
}

// carrierRenewalTopics wake the carrier's signed-network renewal. Only machine_access: it moves on grant, access
// session and CLI-session revocation, never because a renewal ran. peer_network and inventory move when a renewal
// reports endpoints (migration 186), so listening to them fed renewals back into each other (46aa1e11, reverted).
var carrierRenewalTopics = []machineevents.Topic{machineevents.TopicMachineAccess}

func (c *ProductionCarrier) Start(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrProductionInvalid
	}
	c.runCtx = ctx
	if c.standby {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, roleLockWait)
	defer cancel()
	return c.serve(wait)
}

// StartNetwork has a standby carrier take the serving role and bring up the
// machine's network on the same identity once the serving carrier released it
// (ReleaseNetwork). It waits for that until ctx ends, and is idempotent.
func (c *ProductionCarrier) StartNetwork(ctx context.Context) error {
	if c == nil || ctx == nil || c.runCtx == nil {
		return ErrProductionInvalid
	}
	return c.serve(ctx)
}

// ReleaseNetwork takes the serving carrier's network down, closes its state files
// and frees the role for the standby carrier. The process exits afterwards.
func (c *ProductionCarrier) ReleaseNetwork(ctx context.Context) error {
	err := c.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier generation", "stage", "network_released", "failed", err != nil)
	return err
}

// serve takes the role, assembles the serving parts and starts them.
func (c *ProductionCarrier) serve(wait context.Context) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.serving != nil {
		return nil
	}
	unlock := func() error { return nil }
	if c.roleLock != "" {
		if unlock, err = awaitRoleLock(wait, c.roleLock); err != nil {
			return err
		}
	}
	serving, err := c.assemble()
	if err != nil {
		return errors.Join(err, unlock())
	}
	serving.unlock = unlock
	if err := serving.start(c.runCtx); err != nil {
		return errors.Join(err, serving.stop(context.WithoutCancel(c.runCtx)))
	}
	c.serving = serving
	diagnosticlog.TryInfo("carrier generation", "stage", "network_started")
	return nil
}

func (s *servingCarrier) start(ctx context.Context) error {
	if err := s.authorization.Start(ctx); err != nil {
		return err
	}
	return s.peer.Start(ctx)
}

// stop closes the network and the services before the role is released, so the next holder owns quiet state.
func (s *servingCarrier) stop(ctx context.Context) error {
	// Close listeners and bridges before their identity refresh and usage owner.
	diagnosticlog.TryInfo("carrier shutdown", "stage", "start")
	peerErr := s.peer.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "peer_complete", "failed", peerErr != nil)
	authErr := s.authorization.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "authorization_complete", "failed", authErr != nil)
	// The next holder resumes the checkpoint; waiting for the control plane to take the bytes would hold the role.
	usageErr := s.usage.checkpoint(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "usage_checkpoint_complete", "failed", usageErr != nil)
	result := errors.Join(peerErr, authErr, usageErr, s.unlock())
	diagnosticlog.TryInfo("carrier shutdown", "stage", "complete", "failed", result != nil)
	return result
}

// PrepareSSHRetirement fences new SSH only when all accepted bridges ended.
// The native coordinator must also drain other application operations before
// replacing the carrier process.
func (c *ProductionCarrier) PrepareSSHRetirement() error {
	if c == nil {
		return ErrProductionInvalid
	}
	return c.ssh.Retire()
}

func (c *ProductionCarrier) Shutdown(ctx context.Context) error {
	if c == nil || ctx == nil {
		return ErrProductionInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	serving := c.serving
	c.serving = nil
	if serving == nil {
		return nil
	}
	return serving.stop(ctx)
}
