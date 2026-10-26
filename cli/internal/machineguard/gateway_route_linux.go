package machineguard

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// One host gateway routed by the caller's uid (PLAN 3b multi-account hosts, F23-01). The guard owns one listener
// per gateway port. Each OS user's daemon registers a route for the port and receives one end of a socket pair;
// every accepted connection is attributed to the connecting socket's uid (attributeConnection, fail closed) and
// its descriptor is passed over that uid's pair. The daemon routes by Host/SNI among its own routes only.
const gatewayRouting = true

// maxGatewayAttributions bounds the connections being attributed at once; more are closed.
const maxGatewayAttributions = 64

type sharedGateway struct {
	port     int
	listener net.Listener
	mu       sync.Mutex
	routes   map[uint32]*gatewayRoute
	slots    chan struct{}
	lookup   lookupOwners
	release  func(*sharedGateway) // called once the last route is closed, under the guard lock
}

// gatewayRoute is one uid's registration on a gateway port. It stands in a lease as its listener: File gives the
// daemon its end of the pair once, Close withdraws the route.
type gatewayRoute struct {
	gateway   *sharedGateway
	uid       uint32
	conn      *net.UnixConn
	daemonEnd *os.File
	closeOnce sync.Once
}

// routeGateway registers uid on the gateway port, opening the shared listener on first use. Called under g.mu.
func (g *guardServer) routeGateway(ctx context.Context, uid string, port int) (*gatewayRoute, error) {
	owner, err := strconv.ParseUint(uid, 10, 32)
	if err != nil {
		return nil, errors.New("invalid Unix guard owner")
	}
	gateway := g.gateways[port]
	if gateway == nil {
		listener, err := listenProtected(ctx, net.JoinHostPort(splitdns.BrowserGatewayIP, strconv.Itoa(port)), g.cfg.LoopbackCIDR)
		if err != nil {
			return nil, err
		}
		gateway = &sharedGateway{port: port, listener: listener, routes: map[uint32]*gatewayRoute{}, slots: make(chan struct{}, maxGatewayAttributions), lookup: lookupOwnersINETDiag, release: func(s *sharedGateway) {
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
	route, err := gateway.addRoute(uint32(owner))
	if err != nil {
		gateway.closeIfUnused()
		return nil, err
	}
	return route, nil
}

// addRoute registers uid on the gateway with a new socket pair.
func (s *sharedGateway) addRoute(uid uint32) (*gatewayRoute, error) {
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	guardFile := os.NewFile(uintptr(pair[0]), "paperboat-gateway-route")
	defer guardFile.Close()
	conn, err := net.FileConn(guardFile)
	if err != nil {
		unix.Close(pair[1])
		return nil, err
	}
	route := &gatewayRoute{gateway: s, uid: uid, conn: conn.(*net.UnixConn), daemonEnd: os.NewFile(uintptr(pair[1]), "paperboat-gateway-daemon")}
	s.mu.Lock()
	s.routes[uid] = route
	s.mu.Unlock()
	return route, nil
}

func (s *sharedGateway) closeIfUnused() {
	s.mu.Lock()
	empty := len(s.routes) == 0
	s.mu.Unlock()
	if empty {
		_ = shutdownListener(s.listener)
		s.release(s)
	}
}

func (s *sharedGateway) route(uid uint32) (*gatewayRoute, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	route, ok := s.routes[uid]
	return route, ok
}

func (s *sharedGateway) serve() {
	for {
		accepted, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			var transient interface{ Temporary() bool }
			if errors.As(err, &transient) && transient.Temporary() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return
		}
		select {
		case s.slots <- struct{}{}:
			go func() {
				defer func() { <-s.slots }()
				s.handoff(accepted)
			}()
		default:
			accepted.Close()
		}
	}
}

// handoff attributes one accepted connection and passes it to its uid's daemon; anything else is refused.
func (s *sharedGateway) handoff(accepted net.Conn) {
	defer accepted.Close()
	_, route, err := attributeConnection(accepted, s.lookup, s.route)
	if err != nil {
		if s.port == 80 {
			refuseGatewayHTTP(accepted)
		}
		return
	}
	file, err := accepted.(*net.TCPConn).File()
	if err != nil {
		return
	}
	defer file.Close()
	if err := route.pass(file); err != nil {
		route.Close() // the daemon stopped reading its pair: it is gone
	}
}

func (r *gatewayRoute) pass(file *os.File) error {
	if err := r.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	n, _, err := r.conn.WriteMsgUnix([]byte{1}, unix.UnixRights(int(file.Fd())), nil)
	if err == nil && n != 1 {
		err = io.ErrShortWrite
	}
	return err
}

// refuseGatewayHTTP answers a plain-HTTP gateway connection from a user without a running Paperboat daemon.
func refuseGatewayHTTP(conn net.Conn) {
	const body = "This OS user has no running Paperboat daemon. Run `pb install` and `pb login` as this user.\n"
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = io.WriteString(conn, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/plain; charset=utf-8\r\nConnection: close\r\nContent-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body)
}

// File hands the daemon its end of the pair; the caller closes it after sending.
func (r *gatewayRoute) File() (*os.File, error) {
	if r.daemonEnd == nil {
		return nil, errors.New("gateway route was already handed over")
	}
	file := r.daemonEnd
	r.daemonEnd = nil
	return file, nil
}

func (r *gatewayRoute) Close() error {
	r.closeOnce.Do(func() {
		r.gateway.mu.Lock()
		if r.gateway.routes[r.uid] == r {
			delete(r.gateway.routes, r.uid)
		}
		r.gateway.mu.Unlock()
		_ = r.conn.Close()
		if r.daemonEnd != nil {
			_ = r.daemonEnd.Close()
		}
	})
	return nil
}

// release closes the route and, when it was the port's last, the shared listener. Called under g.mu.
func (r *gatewayRoute) release() error {
	err := r.Close()
	r.gateway.closeIfUnused()
	return err
}

func (r *gatewayRoute) Accept() (net.Conn, error) {
	return nil, errors.New("gateway route is not a listener")
}
func (r *gatewayRoute) Addr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP(splitdns.BrowserGatewayIP), Port: r.gateway.port}
}
