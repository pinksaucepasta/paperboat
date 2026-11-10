//go:build windows

package machineguard

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// One host gateway routed by the caller's OS user (PLAN 3b multi-account hosts, F23-01), as on Linux and macOS.
// The guard owns one listener per gateway port. Each user's daemon registers a route for the port; every accepted
// connection is attributed to the connecting process's user (connectionOwnerSID, fail closed) and queued on that
// user's route, which its accept broker (transport_windows.go) serves like any protected listener.
const gatewayRouting = true

type sharedGateway struct {
	port     int
	listener net.Listener
	mu       sync.Mutex
	routes   map[string]*gatewayRoute // by user SID
	slots    chan struct{}
	owner    func(net.Conn) (string, error)
	crl      func(path string) []byte // the CA's revocation list for its CRL path, answered by the guard on port 80
	release  func(*sharedGateway)     // called once the last route is closed, under the guard lock
}

// gatewayRoute is one user's registration on a gateway port: the listener of its lease.
type gatewayRoute struct {
	gateway *sharedGateway
	sid     string
	mu      sync.Mutex
	closed  bool
	conns   chan net.Conn
}

// routeGateway registers the user sid on the gateway port, opening the shared listener on first use. Called under g.mu.
func (g *guardServer) routeGateway(ctx context.Context, sid string, port int) (*gatewayRoute, error) {
	gateway := g.gateways[port]
	if gateway == nil {
		listener, err := listenProtected(ctx, net.JoinHostPort(splitdns.BrowserGatewayIP, strconv.Itoa(port)), g.cfg.LoopbackCIDR)
		if err != nil {
			return nil, err
		}
		gateway = &sharedGateway{port: port, listener: listener, routes: map[string]*gatewayRoute{}, slots: make(chan struct{}, maxGatewayAttributions), owner: connectionOwnerSID, crl: g.gatewayRevocationList, release: func(s *sharedGateway) {
			if g.gateways[s.port] == s {
				delete(g.gateways, s.port)
			}
		}}
		if g.gateways == nil {
			g.gateways = map[int]*sharedGateway{}
		}
		g.gateways[port] = gateway
		go gateway.serve()
	}
	return gateway.addRoute(sid), nil
}

func (s *sharedGateway) addRoute(sid string) *gatewayRoute {
	route := &gatewayRoute{gateway: s, sid: sid, conns: make(chan net.Conn, maxGatewayAttributions)}
	s.mu.Lock()
	s.routes[sid] = route
	s.mu.Unlock()
	return route
}

func (s *sharedGateway) closeIfUnused() {
	s.mu.Lock()
	empty := len(s.routes) == 0
	s.mu.Unlock()
	if empty {
		_ = s.listener.Close()
		s.release(s)
	}
}

// handoff attributes one accepted connection and queues it for its user's daemon; anything else is refused.
func (s *sharedGateway) handoff(accepted net.Conn) {
	if s.port == 80 && s.crl != nil {
		var served bool
		if accepted, served = serveGatewayCRL(accepted, s.crl); served {
			return
		}
	}
	sid, err := s.owner(accepted)
	if err == nil {
		s.mu.Lock()
		route := s.routes[sid]
		s.mu.Unlock()
		if route == nil {
			err = errAttributionNoDaemon
		} else if route.deliver(accepted) {
			return
		} else {
			countGatewayRefusal(refusalHandoffFailed, s.port, nil) // the daemon stopped accepting, or is gone
			accepted.Close()
			return
		}
	}
	countGatewayRefusal(attributionRefusalReason(err), s.port, err)
	if s.port == 80 {
		refuseGatewayHTTP(accepted)
	}
	accepted.Close()
}

// deliver queues conn for the daemon's next Accept; false when the route is closed or its queue is full.
func (r *gatewayRoute) deliver(conn net.Conn) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	select {
	case r.conns <- conn:
		return true
	default:
		return false
	}
}

func (r *gatewayRoute) Accept() (net.Conn, error) {
	conn, ok := <-r.conns
	if !ok {
		return nil, net.ErrClosed
	}
	return conn, nil
}

// Close withdraws the route and closes the connections nobody accepted. shutdownListener then closes the shared
// listener when this was the port's last route.
func (r *gatewayRoute) Close() error {
	r.gateway.mu.Lock()
	if r.gateway.routes[r.sid] == r {
		delete(r.gateway.routes, r.sid)
	}
	r.gateway.mu.Unlock()
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.conns)
	}
	r.mu.Unlock()
	for conn := range r.conns {
		conn.Close()
	}
	return nil
}

func (r *gatewayRoute) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(splitdns.BrowserGatewayIP), Port: r.gateway.port}
}

// gatewayRevocationList is the CA's revocation list when path is its CRL path, else nil (an unknown path falls through
// to attribution like any other request). The list is public; it is renewed here as certificate issuance renews it.
func (g *guardServer) gatewayRevocationList(path string) []byte {
	g.mu.Lock()
	ca := g.ca
	g.mu.Unlock()
	if ca == nil || ca.CRLPath() != path {
		return nil
	}
	der, err := ca.RevocationList(time.Now())
	if err != nil {
		return nil
	}
	return der
}
