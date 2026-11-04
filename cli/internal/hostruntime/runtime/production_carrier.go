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
}

// CarrierGeneration places a carrier process among the account's carrier
// generations (Linux, macOS); Windows runs one carrier and leaves Name empty.
type CarrierGeneration struct {
	// Name is this generation (its source SHA-256 prefix).
	Name string
	// Active says the account's descriptor names this generation; an update's
	// candidate is not active yet.
	Active bool
	// PreparePeer readies another generation for a client of this one moving to it.
	PreparePeer func(context.Context, string, native.CarrierExchange) (native.CarrierOffer, error)
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
	if generation.Name != "" && (!generation.Active || slot != 0) {
		delegated = generation.Name
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
		networkEvents:     events.Subscribe(carrierRenewalTopics...),
		carrierGeneration: delegated, prepareGeneration: generation.PreparePeer,
	})
	if err != nil {
		return nil, err
	}
	return &ProductionCarrier{peer: peer, authorization: serviceGroup{events, &jwksRefreshService{cache: keys}, newPeerEnrollmentRuntimeService(peerEnrollment, 2*time.Second), newOfflineAuditService(audit, controlURL, identity.MachineID, config.StateRoot, transport, tokens, operationID)}, usage: usageService, ssh: ssh, unlock: unlock}, nil
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
	if err := c.peer.Start(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return errors.Join(err, c.authorization.Shutdown(cleanupCtx), c.usage.Shutdown(cleanupCtx))
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
	peerErr := c.peer.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "peer_complete", "failed", peerErr != nil)
	authErr := c.authorization.Shutdown(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "authorization_complete", "failed", authErr != nil)
	usageErr := c.usage.checkpoint(ctx)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "usage_checkpoint_complete", "failed", usageErr != nil)
	var unlockErr error
	if c.unlock != nil {
		unlockErr = c.unlock()
	}
	result := errors.Join(peerErr, authErr, usageErr, unlockErr)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "complete", "failed", result != nil)
	return result
}

// Drain lets a retired generation finish: new SSH is fenced, and it returns
// once no SSH bridge or other application stream is open, or at
// managedssh.CarrierDrainTimeout. Clients already open new connections on the
// newer generation it offered (MoveTerminals).
func (c *ProductionCarrier) Drain(ctx context.Context) error {
	ssh := make(chan error, 1)
	go func() { ssh <- c.ssh.Drain(ctx) }()
	streams := c.peer.drainStreams(ctx, managedssh.CarrierDrainTimeout)
	return errors.Join(streams, <-ssh)
}

// GenerationOffer is this delegated generation's signed offer.
func (c *ProductionCarrier) GenerationOffer() ([]byte, error) {
	offer, err := c.peer.generationOffer()
	if err != nil {
		return nil, err
	}
	return offer.MarshalBinary()
}

// PreparePeer readies this delegated generation for a client moving to it.
func (c *ProductionCarrier) PreparePeer(ctx context.Context, peerID string, exchange []byte) ([]byte, error) {
	offer, err := c.peer.preparePeer(ctx, peerID, exchange)
	if err != nil {
		return nil, err
	}
	return offer.MarshalBinary()
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
