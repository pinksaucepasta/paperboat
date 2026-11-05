//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/bandwidth"
	clientconfig "github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/offlineaudit"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
	"github.com/pinksaucepasta/paperboat/internal/managedssh"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// ProductionCarrier owns one native connection carrier generation of the
// account. It neither boots a feature worker nor owns terminal, exec, or
// transfer state.
type ProductionCarrier struct {
	peer          *productionNativePeerService
	authorization Service
	usage         *hostBandwidthService
	ssh           *managedssh.Host
	unlock        func() error
	// A delegated generation takes the primary role when the generation holding
	// it exits (awaitPrimary); unlockPrimary then releases it at shutdown.
	slot          int
	primaryLock   string
	active        func(context.Context) (bool, error)
	stopTakeover  context.CancelFunc
	takeoverDone  chan struct{}
	unlockPrimary func() error
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

// carrierSlots bounds the generations serving at once (one active, the rest
// draining), and so the state files they keep. Each slot's files have one
// writer, its lock holder; a later generation in the slot takes over what is
// still pending (usage, audit backlog).
const carrierSlots = 4

// NewProductionCarrier loads an already enrolled machine identity. Initial
// enrollment and current SSH target publication remain installer/worker work.
// Only the active generation in slot 0 serves the machine's primary network;
// every other generation is delegated (an update's candidate, or a restart while
// an older generation still drains on the primary network).
func NewProductionCarrier(ctx context.Context, version string, environ func(string) string, dispatch *server.NativeDispatchClient, ssh *managedssh.Host, generation CarrierGeneration) (_ *ProductionCarrier, resultErr error) {
	if ctx == nil || environ == nil || dispatch == nil || ssh == nil {
		return nil, ErrProductionInvalid
	}
	base, err := loadCarrierIdentity(ctx, version, environ)
	if err != nil {
		return nil, err
	}
	slot, unlock := 0, func() error { return nil }
	if generation.Name != "" {
		slot, unlock, err = lockCarrierSlot(filepath.Join(base.config.StateRoot, "runtime"))
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, unlock())
		}
	}()
	delegated := ""
	if generation.Name != "" {
		active, err := generation.Active(ctx)
		if err != nil {
			return nil, err
		}
		if !active || slot != 0 {
			delegated = generation.Name
		}
	}
	name := fmt.Sprintf("carrier-%d", slot)
	config, controlURL, transport := base.config, base.controlURL, base.transport
	identity, machineIdentity := base.identity, base.machineIdentity
	tokens, operationID := base.tokens, base.operationID
	events, peerEnrollment := base.events, base.peerEnrollment
	if err := allowPendingPeerEnrollment(ctx, peerEnrollment); err != nil {
		return nil, err
	}
	// Each process owns its persisted key cache.
	keys, err := productionJWKSCache(controlURL, transport, filepath.Join(config.StateRoot, "authorization", name+"-jwks.json"))
	if err != nil {
		return nil, err
	}
	usageControl := clientapi.New(controlURL.String(), clientconfig.Credential{}, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	usageControl.SetMachineAuth(machineIdentity)
	usage, err := bandwidth.Open(filepath.Join(config.StateRoot, name+"-bandwidth-usage.json"), usageControl, newBandwidthObservation(ctx))
	if err != nil {
		return nil, err
	}
	usageService := &hostBandwidthService{recorder: usage}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, usageService.Shutdown(context.WithoutCancel(ctx)))
		}
	}()
	audit, err := auditCarrierSSH(filepath.Join(config.StateRoot, name+"-offline-audit.json"), ssh)
	if err != nil {
		return nil, errors.Join(ErrProductionInvalid, err)
	}
	peer, err := newProductionNativePeerService(productionNativePeerConfig{
		controlURL: controlURL.String(), issuer: strings.TrimRight(valueOrRuntime(environ("PAPERBOAT_CONTROL_ISSUER"), controlURL.String()), "/"),
		stateRoot: config.StateRoot, machineID: identity.MachineID, generation: uint64(base.generation),
		transport: transport, identity: machineIdentity, keys: keys, dispatch: dispatch, ssh: ssh, usage: usage, offlineAudit: audit,
		networkEvents:     events.Subscribe(carrierRenewalTopics...),
		carrierGeneration: delegated,
	})
	if err != nil {
		return nil, err
	}
	carrier := &ProductionCarrier{peer: peer, authorization: serviceGroup{events, &jwksRefreshService{cache: keys}, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second), newOfflineAuditService(audit, controlURL, identity.MachineID, config.StateRoot, transport, tokens, operationID)}, usage: usageService, ssh: ssh, unlock: unlock}
	if delegated != "" {
		carrier.slot, carrier.active = slot, generation.Active
		carrier.primaryLock = filepath.Join(config.StateRoot, "runtime", "carrier-0.lock")
	}
	return carrier, nil
}

// primaryTakeoverInterval is how often a delegated generation tries the primary
// role, as netbird polls a non-blocking flock (~/ref/netbird/proxy/internal/flock/flock_unix.go:39).
const primaryTakeoverInterval = time.Second

// awaitPrimary takes the primary role (slot 0's lock) once the generation that
// holds it exits, if this generation is the active one and not retired, then
// promotes the peer service in place. Only the slot 0 holder serves the
// primary network, so the role is never contested; a retired generation never
// takes it.
func (c *ProductionCarrier) awaitPrimary(ctx context.Context) {
	defer close(c.takeoverDone)
	ticker := time.NewTicker(primaryTakeoverInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if c.peer.retired.Load() {
			return
		}
		unlock := func() error { return nil }
		if c.slot != 0 {
			var err error
			if unlock, err = lockCarrier(c.primaryLock); err != nil {
				continue
			}
		}
		active, err := c.active(ctx)
		if err != nil || !active || c.peer.retired.Load() {
			_ = unlock()
			continue
		}
		c.unlockPrimary = unlock
		diagnosticlog.TryInfo("carrier generation", "stage", "primary_role_taken")
		c.peer.becomePrimary()
		return
	}
}

// lockCarrierSlot takes the first free slot of carrierSlots.
func lockCarrierSlot(root string) (int, func() error, error) {
	var failures error
	for slot := range carrierSlots {
		unlock, err := lockCarrier(filepath.Join(root, fmt.Sprintf("carrier-%d.lock", slot)))
		if err == nil {
			return slot, unlock, nil
		}
		failures = errors.Join(failures, err)
	}
	return 0, nil, errors.Join(ErrCarrierSlotsBusy, failures)
}

// ErrCarrierSlotsBusy means carrierSlots older generations still drain open streams.
var ErrCarrierSlotsBusy = errors.New("every carrier generation slot is serving; wait for an older generation's open streams to close")

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
	if err := c.authorization.Start(ctx); err != nil {
		return errors.Join(err, c.usage.Shutdown(context.WithoutCancel(ctx)))
	}
	if err := c.peer.Start(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, c.authorization.Shutdown(cleanupCtx), c.usage.Shutdown(cleanupCtx))
	}
	if c.primaryLock != "" {
		var takeover context.Context
		takeover, c.stopTakeover = context.WithCancel(ctx)
		c.takeoverDone = make(chan struct{})
		go c.awaitPrimary(takeover)
	}
	return nil
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
	// Close listeners and bridges before their identity refresh and usage owner.
	diagnosticlog.TryInfo("carrier shutdown", "stage", "start")
	if c.stopTakeover != nil {
		c.stopTakeover()
		<-c.takeoverDone
	}
	peerErr := c.peer.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "peer_complete", "failed", peerErr != nil)
	authErr := c.authorization.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "authorization_complete", "failed", authErr != nil)
	// A replacement in the same slot resumes a checkpoint. A retired generation's replacement already runs in
	// another slot, so its file waits for a later process in this slot: deliver the drained sessions' bytes now.
	var usageErr error
	if c.peer.retired.Load() {
		usageErr = c.usage.Shutdown(ctx)
	} else {
		usageErr = c.usage.checkpoint(ctx)
	}
	diagnosticlog.TryInfo("carrier shutdown", "stage", "usage_checkpoint_complete", "failed", usageErr != nil)
	// The primary role is released only after the primary listener closed.
	var unlockErr error
	if c.unlockPrimary != nil {
		unlockErr = c.unlockPrimary()
	}
	if c.unlock != nil {
		unlockErr = errors.Join(unlockErr, c.unlock())
	}
	result := errors.Join(peerErr, authErr, usageErr, unlockErr)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "complete", "failed", result != nil)
	return result
}

// Drain lets a retired generation finish, as nginx's old workers and Teleport's
// sshserver Shutdown do (~/ref/nginx/src/os/unix/ngx_process_cycle.c,
// ~/ref/teleport/lib/srv/regular/sshserver.go:449): it serves every connection it
// holds, and still admits new ones (a client its signed configuration cannot yet
// take to the new generation keeps a path), until the last stream ends or the same
// cap as SSH. What remains then is closed with the restart code and reattaches on
// the new generation with sequence replay.
func (c *ProductionCarrier) Drain(ctx context.Context) error {
	return c.drain(ctx, runtimewire.DrainLimit)
}

func (c *ProductionCarrier) drain(ctx context.Context, limit time.Duration) error {
	c.peer.retired.Store(true)
	ssh := make(chan error, 1)
	go func() { ssh <- c.ssh.Drain(ctx) }()
	streams := c.peer.drainStreams(ctx, limit)
	return errors.Join(streams, <-ssh, c.peer.restartSessions())
}
