package native

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// carrierRoute is a client's active carrier generation for one machine: once a
// generation that serves every application accepted this client, new
// connections go to it, and connections already open stay where they are until
// they close (Teleport's DrainConnections advises reconnects without closing
// open connections, ~/ref/teleport/lib/reversetunnel/srv.go:671). The
// route ends at the delegation's expiry or at the first failed dial.
type carrierRoute struct {
	offer   CarrierOffer
	network *tailnet.CarrierNetwork
	release func()
	expiry  *time.Timer
}

// setRoute is called with a connected session that holds a reference to the
// offer's network; the route takes its own.
func (o *Owner) setRoute(ctx context.Context, peerID string, offer CarrierOffer) {
	network, release, err := o.getCarrierNetwork(ctx, offer)
	if err != nil {
		return
	}
	route := &carrierRoute{offer: offer, network: network, release: release}
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		release()
		return
	}
	if o.routes == nil {
		o.routes = make(map[string]*carrierRoute)
	}
	previous := o.routes[peerID]
	o.routes[peerID] = route
	route.expiry = time.AfterFunc(time.Until(time.Unix(offer.Claims.ExpiresUnix, 0)), func() { o.dropRoute(peerID, route) })
	o.mu.Unlock()
	if previous != nil {
		previous.expiry.Stop()
		previous.release()
	}
}

func (o *Owner) dropRoute(peerID string, route *carrierRoute) {
	o.mu.Lock()
	current := o.routes[peerID] == route
	if current {
		delete(o.routes, peerID)
	}
	o.mu.Unlock()
	if current {
		route.expiry.Stop()
		route.release()
	}
}

func (o *Owner) hasRoute(peerID string, offer CarrierOffer) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	route := o.routes[peerID]
	return route != nil && route.offer.Namespace() == offer.Namespace()
}

// dialRoute opens a new connection on peerID's active generation, or returns
// nil so the caller dials as before. want restricts it to that generation. A
// generation that does not answer within one attempt is dropped: the caller's
// own path is then the fallback, never a second wait.
func (o *Owner) dialRoute(ctx context.Context, peerID string, want *CarrierOffer, class peerquic.Class) *Session {
	o.mu.Lock()
	route := o.routes[peerID]
	o.mu.Unlock()
	if route == nil || want != nil && route.offer.Namespace() != want.Namespace() {
		return nil
	}
	peer, err := o.authority.Peer(peerID)
	if err != nil || route.offer.verify(peer, time.Now()) != nil {
		o.dropRoute(peerID, route)
		return nil
	}
	network, release, err := o.getCarrierNetwork(ctx, route.offer)
	if err != nil {
		o.dropRoute(peerID, route)
		return nil
	}
	attempt, cancel := context.WithTimeout(ctx, carrierAttemptLimit)
	defer cancel()
	transport, err := o.dialCarrierPacket(attempt, network, peerID, o.carrierTLS(peerID, peer, route.offer), class)
	if err != nil {
		release()
		if ctx.Err() == nil {
			o.dropRoute(peerID, route)
		}
		diagnosticlog.TryInfo("carrier route", "stage", "dial", "failed", true, "error_type", moveErrorType(err))
		return nil
	}
	session, err := o.carrierSession(peerID, peer, route.offer, transport, network, release)
	if err != nil {
		o.dropRoute(peerID, route)
		return nil
	}
	return session
}
