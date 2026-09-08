package tailnet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"

	"github.com/tailscale/tailcat"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=virtual-udp-admission
	"tailscale.com/tailcfg"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=virtual-udp-admission
	"tailscale.com/types/key"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=virtual-udp-admission
	"tailscale.com/wgengine/filter"
)

// MaxFlows bounds queued and leased sockets per engine. The initial two-peer,
// eight-stream workload uses shared QUIC connections, leaving ample headroom.
const MaxFlows = 64

var ErrAdmission = errors.New("virtual UDP requires explicit peer and port admission")
var ErrFlowLimit = errors.New("virtual UDP flow limit reached")

// UDPServer serves one explicitly admitted application port. ListenUDP retains
// static transport admission; Authority.Listen installs verified, replaceable
// Paperboat authority. Application operation grants remain separate.
type UDPServer struct {
	server   *tailcat.Server
	mu       sync.Mutex
	closed   bool
	flows    map[*Packet]struct{}
	admitted map[netip.Addr]string
	slots    chan struct{}
	ready    chan *Packet
	done     chan struct{}
	handlers sync.WaitGroup
	once     sync.Once
}

func ListenUDP(region *tailcfg.DERPRegion, allowed []key.NodePublic, port uint16) (*UDPServer, error) {
	if region == nil || len(allowed) == 0 || port == 0 {
		return nil, ErrAdmission
	}
	for _, k := range allowed {
		if k.IsZero() {
			return nil, ErrAdmission
		}
	}
	return listenUDP(&tailcat.Server{Region: region, AllowedClients: append([]key.NodePublic(nil), allowed...), ServedUDPPorts: []filter.PortRange{{First: port, Last: port}}, Logf: func(string, ...any) {}}, port, nil)
}

func listenUDP(server *tailcat.Server, port uint16, admitted map[netip.Addr]string) (*UDPServer, error) {
	s := &UDPServer{server: server, admitted: admitted, flows: make(map[*Packet]struct{}), slots: make(chan struct{}, MaxFlows), ready: make(chan *Packet, MaxFlows), done: make(chan struct{})}
	s.server.OnUDP = func(p uint16) func(tailcat.ConnPacketConn) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed || p != port {
			return nil
		}
		select {
		case s.slots <- struct{}{}:
		default:
			return nil
		}
		s.handlers.Add(1)
		return func(c tailcat.ConnPacketConn) {
			defer s.handlers.Done()
			var packet *Packet
			packet = newPacket(c, func() { s.mu.Lock(); delete(s.flows, packet); s.mu.Unlock(); <-s.slots })
			s.mu.Lock()
			address, _ := netip.ParseAddrPort(packet.RemoteAddr().String())
			binding := s.admitted[address.Addr()]
			if s.closed || s.admitted != nil && binding == "" {
				s.mu.Unlock()
				_ = packet.Close()
				return
			}
			if s.admitted != nil {
				packet.permit = func() bool {
					s.mu.Lock()
					defer s.mu.Unlock()
					return !s.closed && binding != "" && s.admitted[address.Addr()] == binding
				}
			}
			s.flows[packet] = struct{}{}
			s.ready <- packet // One reserved slot per queued or leased socket.
			s.mu.Unlock()
		}
	}
	if err := s.server.Start(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *UDPServer) Address() tailcat.Addr { return s.server.ConnBlob() }

// Accept transfers one flow lease to the caller. Handle QUIC handshakes
// concurrently: late packets can reopen a retired UDP tuple without completing
// another QUIC handshake. Close every lease, including failed handshakes.
func (s *UDPServer) Accept(ctx context.Context) (*Packet, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, net.ErrClosed
	case p := <-s.ready:
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed || ctx.Err() != nil {
			_ = p.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, net.ErrClosed
		}
		if p.permit != nil && !p.permit() {
			_ = p.Close()
			return nil, ErrAdmission
		}
		return p, nil
	}
}
func (s *UDPServer) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		close(s.done)
		packets := make([]*Packet, 0, len(s.flows))
		for p := range s.flows {
			packets = append(packets, p)
		}
		s.mu.Unlock()
		for _, p := range packets {
			_ = p.Close()
		}
		s.server.Close()
		s.handlers.Wait()
		for {
			select {
			case <-s.ready:
			default:
				return
			}
		}
	})
	return nil
}

// UDPClient owns one Tailcat engine and a bounded set of outgoing socket leases.
// It admits only its configured server and application port.
type UDPClient struct {
	client      *tailcat.Client // standalone compatibility owner
	dial        func(context.Context) (tailcat.ConnPacketConn, error)
	slots       chan struct{}
	closeEngine bool
	port        uint16
	mu          sync.Mutex
	closed      bool
	flows       map[*Packet]struct{}
	count       int
	opens       sync.WaitGroup
	ctx         context.Context
	cancel      context.CancelFunc
	once        sync.Once
}

func NewUDPClient(server tailcat.Addr, private key.NodePrivate, port uint16) (*UDPClient, error) {
	if private.IsZero() || port == 0 {
		return nil, ErrAdmission
	}
	ctx, cancel := context.WithCancel(context.Background())
	engine := &tailcat.Client{Server: server, Key: private, Logf: func(string, ...any) {}}
	return &UDPClient{client: engine, dial: func(ctx context.Context) (tailcat.ConnPacketConn, error) { return engine.DialUDPPort(ctx, port) }, slots: make(chan struct{}, MaxFlows), closeEngine: true, port: port, flows: make(map[*Packet]struct{}), ctx: ctx, cancel: cancel}, nil
}
func (c *UDPClient) Dial(ctx context.Context) (*Packet, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, net.ErrClosed
	}
	select {
	case c.slots <- struct{}{}:
	default:
		c.mu.Unlock()
		return nil, ErrFlowLimit
	}
	c.count++
	c.opens.Add(1)
	c.mu.Unlock()
	defer c.opens.Done()
	op, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	defer cancel()
	conn, err := c.dial(op)
	if err != nil {
		c.mu.Lock()
		c.count--
		c.mu.Unlock()
		<-c.slots
		return nil, err
	}
	var p *Packet
	p = newPacket(conn, func() { c.mu.Lock(); delete(c.flows, p); c.count--; c.mu.Unlock(); <-c.slots })
	c.mu.Lock()
	if c.closed || op.Err() != nil {
		c.mu.Unlock()
		_ = p.Close()
		if err := op.Err(); err != nil {
			return nil, err
		}
		return nil, net.ErrClosed
	}
	c.flows[p] = struct{}{}
	c.mu.Unlock()
	return p, nil
}
func (c *UDPClient) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.cancel()
		c.mu.Unlock()
		c.opens.Wait()
		c.mu.Lock()
		packets := make([]*Packet, 0, len(c.flows))
		for p := range c.flows {
			packets = append(packets, p)
		}
		c.mu.Unlock()
		for _, p := range packets {
			_ = p.Close()
		}
		if c.closeEngine && c.client != nil {
			c.client.Close()
		}
	})
	return nil
}
