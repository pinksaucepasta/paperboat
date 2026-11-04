//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
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
// network, as staged terminal carriers do (terminal_carrier_start.go), and
// keeps its signed offer current for clients that dial it.
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
	reportCarrierGeneration(run, identity.Current())
	renew, stopRenew := context.WithCancel(run)
	g.stopRenew = stopRenew
	g.done.Add(2)
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
			reportCarrierGeneration(renew, offer)
		}
	}()
	return nil
}

// generationNetwork is a delegated generation's control: read-only, as for
// staged terminal carriers, until the generation takes the primary role.
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
	reportCarrierGeneration(ctx, native.CarrierOffer{})
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

func reportCarrierGeneration(ctx context.Context, offer native.CarrierOffer) {
	report, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := nativegen.Report(report, offer); err != nil {
		diagnosticlog.TryWarn("carrier generation report failed", "stage", "report_generation", "error_type", fmt.Sprintf("%T", err))
	}
}

// generationOffer is the delegated generation's current signed offer.
func (s *productionNativePeerService) generationOffer() (native.CarrierOffer, error) {
	s.mu.Lock()
	generation := s.current
	s.mu.Unlock()
	if generation == nil || generation.carrier == nil {
		return native.CarrierOffer{}, ErrProductionInvalid
	}
	return generation.carrier.Current(), nil
}

// preparePeer readies this delegated generation for peerID, whose candidate
// exchange reached an older generation over the client's authenticated link.
func (s *productionNativePeerService) preparePeer(ctx context.Context, peerID string, encoded []byte) (native.CarrierOffer, error) {
	s.mu.Lock()
	generation := s.current
	s.mu.Unlock()
	var exchange native.CarrierExchange
	if generation == nil || generation.carrier == nil || peerID == "" || json.Unmarshal(encoded, &exchange) != nil {
		return native.CarrierOffer{}, ErrProductionInvalid
	}
	offer := generation.carrier.Current()
	if exchange.Offer.Namespace() != offer.Namespace() {
		return native.CarrierOffer{}, ErrProductionInvalid
	}
	if err := readyCarrierPeer(ctx, generation.network, peerID, exchange); err != nil {
		return native.CarrierOffer{}, err
	}
	return offer, nil
}

// readyCarrierPeer adds the client's candidates to a generation's network and
// punches toward it, so the client's dial finds a path.
func readyCarrierPeer(ctx context.Context, network *tailnet.CarrierNetwork, peerID string, exchange native.CarrierExchange) error {
	if err := network.AddPeer(peerID, exchange.DiscoPublicKey, exchange.Endpoints); err != nil {
		diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_add", "failed", true, "error_type", fmt.Sprintf("%T", err))
		return err
	}
	// The relay is the fallback path; a refused relay registration must not
	// block a direct (punched) move. Dial below decides (pbvm4 run3b).
	if err := network.PreparePeerRelay(ctx, peerID, exchange.RelayNodeID); err != nil {
		diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_relay", "failed", true, "error_type", fmt.Sprintf("%T", err), "reason", relayReason(exchange.RelayNodeID))
	}
	socket, err := network.Dial(ctx, peerID)
	diagnosticlog.TryInfo("carrier shutdown", "stage", "prepare_peer_dial", "failed", err != nil, "error_type", fmt.Sprintf("%T", err), "reason", relayReason(exchange.RelayNodeID))
	if err != nil {
		return err
	}
	socket.Close()
	return nil
}

// prepareCarrierPeer routes a moving client's candidates: a generation that
// serves every application is another carrier process, a terminal-only one a
// session owner's staged carrier.
func prepareCarrierPeer(terminal, generation func(context.Context, string, native.CarrierExchange) (native.CarrierOffer, error)) func(context.Context, string, native.CarrierExchange) (native.CarrierOffer, error) {
	return func(ctx context.Context, peer string, exchange native.CarrierExchange) (native.CarrierOffer, error) {
		if !exchange.Offer.Claims.AllApplications {
			return terminal(ctx, peer, exchange)
		}
		if generation == nil {
			return native.CarrierOffer{}, ErrProductionInvalid
		}
		return generation(ctx, peer, exchange)
	}
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
