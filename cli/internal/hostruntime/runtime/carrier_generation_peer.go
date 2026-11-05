//go:build darwin || linux || windows

package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/nativegen"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// carrierRenewInterval renews a delegated generation's offer at half its
// five-minute lifetime, so a published offer never runs out while it serves.
const carrierRenewInterval = 150 * time.Second

// startDelegatedGeneration serves every application on the generation's own
// network and keeps its signed offer current for clients that dial it.
func (s *productionNativePeerService) startDelegatedGeneration(run, daemon context.Context, g *productionNativePeerGeneration, name string) error {
	identity, err := g.owner.PrepareCarrierIdentity()
	if err != nil {
		return err
	}
	identity.Offer.Claims.AllApplications = true
	apps := g.apps
	g.run = run
	g.serve = func(serveCtx context.Context, session *native.Session) error {
		return apps.Serve(nativePeerApplicationContext(serveCtx, daemon), session)
	}
	g.network, err = g.owner.StartCarrierNetwork(run, identity, name, g.serve)
	if err != nil {
		return err
	}
	g.carrier = identity
	g.reports = make(chan native.CarrierOffer, 1)
	g.reportCarrierGeneration(identity.Current())
	renew, stopRenew := context.WithCancel(run)
	g.stopRenew = stopRenew
	g.done.Add(3)
	go func() {
		defer g.done.Done()
		g.sendCarrierReports(run)
	}()
	go func() {
		defer g.done.Done()
		g.errors <- g.authority.Run(run, g.control, nil)
	}()
	go func() {
		defer g.done.Done()
		ticker := time.NewTicker(carrierRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renew.Done():
				return
			case <-ticker.C:
			}
			offer, err := g.owner.RenewCarrierIdentity(identity, g.network)
			if err != nil {
				// A delegation that cannot be renewed admits nobody new: restart the generation.
				g.errors <- err
				return
			}
			g.reportCarrierGeneration(offer)
		}
	}()
	return nil
}

// generationNetwork is a delegated generation's control: read-only until the
// generation takes the primary role.
type generationNetwork struct {
	tailnet.NetworkAPI
	primary atomic.Bool
}

func (n *generationNetwork) RegisterPeerNetwork(ctx context.Context, r api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	if n.primary.Load() {
		return n.NetworkAPI.RegisterPeerNetwork(ctx, r)
	}
	return terminalCarrierNetwork{n.NetworkAPI}.RegisterPeerNetwork(ctx, r)
}

func (n *generationNetwork) PeerNetworkConfiguration(ctx context.Context, r api.PeerNetworkConfigurationRequest) (api.PeerNetworkConfigurationResult, error) {
	if n.primary.Load() {
		return n.NetworkAPI.PeerNetworkConfiguration(ctx, r)
	}
	return terminalCarrierNetwork{n.NetworkAPI}.PeerNetworkConfiguration(ctx, r)
}

// becomePrimary makes this delegated carrier the machine's primary once it holds
// the primary role (ProductionCarrier.awaitPrimary): the running generation is
// promoted in place, and every later start builds a primary generation.
func (s *productionNativePeerService) becomePrimary() {
	s.mu.Lock()
	s.config.carrierGeneration = ""
	s.mu.Unlock()
	select {
	case s.promote <- struct{}{}:
	default:
	}
}

// promoteGeneration starts the primary listener and endpoint reports on a
// delegated generation's own authority. Its delegated network keeps serving the
// clients on it: no new offer is published or renewed, so their routes end at
// the last offer's expiry and new connections reach the primary; the network
// closes once its last session ends.
func (s *productionNativePeerService) promoteGeneration(ctx context.Context, g *productionNativePeerGeneration) error {
	control, ok := g.control.(*generationNetwork)
	if g.carrier == nil || !ok || !control.primary.CompareAndSwap(false, true) {
		return nil
	}
	g.stopRenew()
	if err := g.listenPrimary(); err != nil {
		return err
	}
	if g.requestRefresh != nil {
		g.requestRefresh()
	}
	g.reportCarrierGeneration(native.CarrierOffer{})
	expires := time.Unix(g.carrier.Current().Claims.ExpiresUnix, 0)
	g.done.Add(1)
	go func() {
		defer g.done.Done()
		closeIdleCarrierNetwork(g.run, g.owner, g.network, expires)
	}()
	diagnosticlog.TryInfo("carrier generation", "stage", "primary_taken")
	return nil
}

// closeIdleCarrierNetwork closes a promoted generation's delegated network once
// no client can route to it (its last offer expired) and no session is open on it.
func closeIdleCarrierNetwork(ctx context.Context, owner *native.Owner, network *tailnet.CarrierNetwork, expires time.Time) {
	timer := time.NewTimer(time.Until(expires))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if owner.CarrierSessions(network) == 0 {
			_ = network.Close()
			return
		}
		timer.Reset(30 * time.Second)
	}
}

// reportCarrierGeneration hands offer to the generation's one reporter and
// returns at once: a slow control plane must never delay serving, revocation or
// promotion. Only the latest offer is kept, and once the zero offer (the
// withdrawal at promotion) is handed over, a renewal that raced it is dropped,
// so a stale offer is never published after the withdrawal.
func (g *productionNativePeerGeneration) reportCarrierGeneration(offer native.CarrierOffer) {
	g.reportMu.Lock()
	defer g.reportMu.Unlock()
	if g.withdrawn {
		return
	}
	g.withdrawn = len(offer.Signature) == 0
	select {
	case <-g.reports:
	default:
	}
	g.reports <- offer
}

// sendCarrierReports publishes each handed-over offer in order until ctx ends.
func (g *productionNativePeerGeneration) sendCarrierReports(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case offer := <-g.reports:
			report, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := nativegen.Report(report, offer)
			cancel()
			if err != nil {
				diagnosticlog.TryWarn("carrier generation report failed", "stage", "report_generation", "error_type", fmt.Sprintf("%T", err))
			}
		}
	}
}

// restartSessions closes the current generation's connections with the restart
// code, so what a retired generation still holds reattaches on the current one.
func (s *productionNativePeerService) restartSessions() error {
	s.mu.Lock()
	generation := s.current
	s.mu.Unlock()
	if generation == nil || generation.owner == nil {
		return nil
	}
	return generation.owner.CloseRestarting()
}

// drainStreams waits until no application stream is served, polling, for at
// most limit. A retired generation keeps serving its open streams meanwhile.
func (s *productionNativePeerService) drainStreams(ctx context.Context, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for s.streams.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
