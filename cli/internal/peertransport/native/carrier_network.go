package native

import (
	"context"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

type carrierNetworkRef struct {
	network *tailnet.CarrierNetwork
	users   int
}

// A generation shares one engine across terminals, and releases it when its
// last pending dial or established connection ends. Certificate expiry bounds
// new admission; it never interrupts an established, renewed terminal.
func (o *Owner) getCarrierNetwork(ctx context.Context, offer CarrierOffer) (*tailnet.CarrierNetwork, func(), error) {
	o.carrierMu.Lock()
	defer o.carrierMu.Unlock()
	if err := o.ctx.Err(); err != nil {
		return nil, nil, err
	}
	namespace := offer.Namespace()
	entry := o.carrierNetworks[namespace]
	if entry == nil {
		network, err := o.authority.NewCarrierNetwork(ctx, namespace, nil)
		if err != nil {
			return nil, nil, err
		}
		entry = &carrierNetworkRef{network: network}
		o.carrierNetworks[namespace] = entry
		network.Watch(o.ctx, func(peers []string) { o.closeCarrierPeers(peers, network) })
	}
	entry.users++
	var once sync.Once
	release := func() {
		once.Do(func() {
			o.carrierMu.Lock()
			entry.users--
			last := entry.users == 0
			if last {
				delete(o.carrierNetworks, namespace)
			}
			o.carrierMu.Unlock()
			if last {
				entry.network.Close()
			}
		})
	}
	return entry.network, release, nil
}

func (o *Owner) closeCarrierNetworks() {
	o.carrierMu.Lock()
	var networks []*tailnet.CarrierNetwork
	for _, entry := range o.carrierNetworks {
		networks = append(networks, entry.network)
	}
	o.carrierMu.Unlock()
	for _, network := range networks {
		network.Close()
	}
}

// CarrierSessions counts the sessions open on network.
func (o *Owner) CarrierSessions(network *tailnet.CarrierNetwork) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	count := 0
	for session := range o.sessions {
		if session.carrierNetwork == network {
			count++
		}
	}
	return count
}

func (o *Owner) closeCarrierPeers(peers []string, network *tailnet.CarrierNetwork) {
	o.mu.Lock()
	var sessions []*Session
	for session := range o.sessions {
		if session.carrierNetwork != network {
			continue
		}
		removed := peers == nil
		for _, peer := range peers {
			removed = removed || peer == session.peerID
		}
		if removed {
			sessions = append(sessions, session)
		}
	}
	o.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}

// StartCarrierNetwork exposes only authenticated WG packets on the public UDP
// socket. QUIC and its admission listener live inside that existing transport.
func (o *Owner) StartCarrierNetwork(ctx context.Context, identity *CarrierIdentity, instance string, serve func(context.Context, *Session) error) (*tailnet.CarrierNetwork, error) {
	if o == nil || identity == nil || serve == nil {
		return nil, ErrInvalid
	}
	prepared := make(chan struct{})
	var network *tailnet.CarrierNetwork
	incoming := func(socket mesh.ConnPacketConn) {
		o.mu.Lock()
		if o.closed {
			o.mu.Unlock()
			socket.Close()
			return
		}
		o.workers.Add(1)
		o.mu.Unlock()
		go func() {
			defer o.workers.Done()
			select {
			case <-prepared:
			case <-ctx.Done():
				socket.Close()
				return
			}
			// Each virtual peer flow has its own QUIC transport. Accounting is bound
			// after TLS identifies the exact currently authorized root peer.
			_ = o.ListenCarrier(ctx, socket, identity, func(ctx context.Context, session *Session) error {
				session.setCarrierNetwork(network)
				return serve(ctx, session)
			}, make(chan struct{}))
		}()
	}
	var err error
	network, err = o.authority.NewCarrierNetwork(ctx, identity.Namespace(), incoming)
	if err != nil {
		return nil, err
	}
	if err := network.WaitEndpoints(ctx); err != nil {
		network.Close()
		return nil, err
	}
	if err := o.SignCarrierCandidates(identity, instance, network.DiscoPublic(), network.Endpoints(), network.RelayNodeIDs()); err != nil {
		network.Close()
		return nil, err
	}
	close(prepared)
	network.Watch(ctx, func(peers []string) { o.closeCarrierPeers(peers, network) })
	// Connect every signed relay before any client moves, as Tailscale keeps its
	// DERP home connected ahead of use (~/ref/tailscale/wgengine/magicsock/derp.go:301).
	// A cold relay from a slow host took over 9 s (dadape relay1d).
	for _, id := range network.RelayNodeIDs() {
		go func() {
			warm, cancel := context.WithTimeout(ctx, CarrierMoveBudget)
			defer cancel()
			_ = network.WarmRelay(warm, id)
		}()
	}
	return network, nil
}

func (s *Session) setCarrierNetwork(network *tailnet.CarrierNetwork) {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	s.carrierNetwork = network
	s.carrierPath = func(direction string) (string, string) {
		path := network.UsagePath(s.peerID, direction)
		return path.Mode, path.NodeID
	}
}
