//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"fmt"
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
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
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
	// standby is an update's candidate: it prepares everything except the
	// machine's network, which StartNetwork brings up once the serving carrier
	// released it. primaryLock is the serving carrier's slot 0 lock, taken first.
	standby       bool
	primaryLock   string
	mu            sync.Mutex
	runCtx        context.Context
	networkUp     bool
	released      bool
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
// Only the active generation in slot 0 starts the machine's network; every
// other generation is standby (an update's candidate) until StartNetwork.
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
	standby := false
	if generation.Name != "" {
		active, err := generation.Active(ctx)
		if err != nil {
			return nil, err
		}
		standby = !active || slot != 0
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
		ownerCatalog: environ("PAPERBOAT_SESSION_OWNER_CATALOG"), stateRoot: config.StateRoot, machineID: identity.MachineID, generation: uint64(base.generation),
		transport: transport, identity: machineIdentity, keys: keys, dispatch: dispatch, ssh: ssh, usage: usage, offlineAudit: audit,
		networkEvents: events.Subscribe(carrierRenewalTopics...),
	})
	if err != nil {
		return nil, err
	}
	carrier := &ProductionCarrier{peer: peer, authorization: serviceGroup{events, &jwksRefreshService{cache: keys}, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second), newOfflineAuditService(audit, controlURL, identity.MachineID, config.StateRoot, transport, tokens, operationID)}, usage: usageService, ssh: ssh, unlock: unlock}
	carrier.standby = standby
	if slot != 0 {
		carrier.primaryLock = filepath.Join(config.StateRoot, "runtime", "carrier-0.lock")
	}
	return carrier, nil
}

// lockCarrierSlot takes the first free slot of carrierSlots.
func lockCarrierSlot(root string) (int, func() error, error) {
	var failures error
	for slot := range carrierSlots {
		unlock, err := lockTerminalCarrier(filepath.Join(root, fmt.Sprintf("carrier-%d.lock", slot)))
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
	c.runCtx = ctx
	if c.standby {
		return nil
	}
	if err := c.startPeer(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, c.authorization.Shutdown(cleanupCtx), c.usage.Shutdown(cleanupCtx))
	}
	return nil
}

func (c *ProductionCarrier) startPeer(ctx context.Context) error {
	if err := c.peer.Start(ctx); err != nil {
		return err
	}
	c.networkUp = true
	return nil
}

// StartNetwork brings a standby carrier's network up on the machine's own
// identity once the serving carrier released it (ReleaseNetwork frees slot 0's
// lock). It waits for that release until ctx ends, and is idempotent.
func (c *ProductionCarrier) StartNetwork(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.networkUp {
		return nil
	}
	if c.runCtx == nil {
		return ErrProductionInvalid
	}
	for c.primaryLock != "" && c.unlockPrimary == nil {
		unlock, err := lockTerminalCarrier(c.primaryLock)
		if err == nil {
			c.unlockPrimary = unlock
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(ctx.Err(), err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := c.startPeer(c.runCtx); err != nil {
		return errors.Join(err, c.unlockPrimaryRole())
	}
	diagnosticlog.TryInfo("carrier generation", "stage", "network_started")
	return nil
}

// ReleaseNetwork takes the serving carrier's network down and frees the
// primary role for the standby carrier. The process exits afterwards.
func (c *ProductionCarrier) ReleaseNetwork(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = true
	err := c.peer.Shutdown(ctx)
	c.networkUp = false
	if c.unlock != nil {
		err = errors.Join(err, c.unlock())
		c.unlock = nil
	}
	diagnosticlog.TryInfo("carrier generation", "stage", "network_released", "failed", err != nil)
	return errors.Join(err, c.unlockPrimaryRole())
}

func (c *ProductionCarrier) unlockPrimaryRole() error {
	if c.unlockPrimary == nil {
		return nil
	}
	err := c.unlockPrimary()
	c.unlockPrimary = nil
	return err
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
	peerErr := c.peer.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "peer_complete", "failed", peerErr != nil)
	authErr := c.authorization.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "authorization_complete", "failed", authErr != nil)
	// A replacement in the same slot resumes a checkpoint. A released carrier's replacement already runs in
	// another slot, so its file waits for a later process in this slot: deliver its sessions' bytes now.
	var usageErr error
	if c.released {
		usageErr = c.usage.Shutdown(ctx)
	} else {
		usageErr = c.usage.checkpoint(ctx)
	}
	diagnosticlog.TryInfo("carrier shutdown", "stage", "usage_checkpoint_complete", "failed", usageErr != nil)
	// The primary role is released only after the primary listener closed.
	unlockErr := c.unlockPrimaryRole()
	if c.unlock != nil {
		unlockErr = errors.Join(unlockErr, c.unlock())
	}
	result := errors.Join(peerErr, authErr, usageErr, unlockErr)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "complete", "failed", result != nil)
	return result
}

func (c *ProductionCarrier) MoveTerminals(ctx context.Context, encoded []byte) error {
	offer, err := native.ParseCarrierOffer(encoded)
	if err != nil {
		return err
	}
	c.peer.mu.Lock()
	generation := c.peer.current
	c.peer.mu.Unlock()
	if generation == nil {
		return ErrProductionInvalid
	}
	return generation.owner.OfferCarrier(ctx, offer)
}
