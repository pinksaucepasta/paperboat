package native_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// This socket models a stateful NAT: unsolicited inbound datagrams have no
// mapping until this exact socket sends to their source. Both discovery and
// WireGuard packets use real UDP; neither QUIC nor its admission is mocked.
type punchPacketListener struct {
	stallNewSockets atomic.Bool
	stalled         atomic.Int64
}

func (f *punchPacketListener) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		return nil, err
	}
	conn := &punchPacketConn{UDPConn: socket.(*net.UDPConn), mappings: make(map[string]bool), listener: f}
	if f.stallNewSockets.Load() {
		conn.stallFor = 1200 * time.Millisecond
	}
	return conn, nil
}

type punchPacketConn struct {
	*net.UDPConn
	mu        sync.Mutex
	mappings  map[string]bool
	listener  *punchPacketListener
	stallFor  time.Duration
	firstData time.Time
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
		if allowed && c.stallFor != 0 && n >= 4 && binary.LittleEndian.Uint32(b[:n]) == 4 {
			if c.firstData.IsZero() {
				c.firstData = time.Now()
			}
			if time.Since(c.firstData) < c.stallFor {
				allowed = false
				c.listener.stalled.Add(1)
			}
		}
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
