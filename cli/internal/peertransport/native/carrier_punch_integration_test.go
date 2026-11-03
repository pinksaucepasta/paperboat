package native_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
)

// This socket models a stateful NAT: unsolicited inbound datagrams have no
// mapping until this exact socket sends to their source. Both discovery and
// WireGuard packets use real UDP; neither QUIC nor its admission is mocked.
type punchPacketListener struct{}

func (*punchPacketListener) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return &punchPacketConn{UDPConn: socket.(*net.UDPConn), mappings: make(map[string]bool)}, nil
}

type punchPacketConn struct {
	*net.UDPConn
	mu       sync.Mutex
	mappings map[string]bool
}

func (c *punchPacketConn) WriteTo(b []byte, target net.Addr) (int, error) {
	c.mu.Lock()
	c.mappings[target.String()] = true
	c.mu.Unlock()
	return c.UDPConn.WriteTo(b, target)
}
func (c *punchPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, source, err := c.UDPConn.ReadFrom(b)
		if err != nil {
			return n, source, err
		}
		c.mu.Lock()
		allowed := c.mappings[source.String()]
		c.mu.Unlock()
		if allowed {
			return n, source, nil
		}
	}
}

func (c *punchPacketConn) WriteToUDPAddrPort(b []byte, target netip.AddrPort) (int, error) {
	return c.WriteTo(b, net.UDPAddrFromAddrPort(target))
}
func (c *punchPacketConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	n, source, err := c.ReadFrom(b)
	if err != nil {
		return n, netip.AddrPort{}, err
	}
	return n, source.(*net.UDPAddr).AddrPort(), nil
}
