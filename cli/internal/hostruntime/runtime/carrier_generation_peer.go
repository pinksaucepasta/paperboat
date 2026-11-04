//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

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
func (s *productionNativePeerService) startDelegatedGeneration(run, daemon context.Context, g *productionNativePeerGeneration) error {
	identity, err := g.owner.PrepareCarrierIdentity()
	if err != nil {
		return err
	}
	identity.Offer.Claims.AllApplications = true
	apps := g.apps
	g.network, err = g.owner.StartCarrierNetwork(run, identity, s.config.carrierGeneration, func(serveCtx context.Context, session *native.Session) error {
		return apps.Serve(nativePeerApplicationContext(serveCtx, daemon), session)
	})
	if err != nil {
		return err
	}
	g.carrier = identity
	reportCarrierGeneration(run, identity.Current())
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
			case <-run.Done():
				return
			case <-ticker.C:
			}
			offer, err := g.owner.RenewCarrierIdentity(identity, g.network)
			if err != nil {
				// A delegation that cannot be renewed admits nobody new: restart the generation.
				g.errors <- err
				return
			}
			reportCarrierGeneration(run, offer)
		}
	}()
	return nil
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
