package mesh

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
	"tailscale.com/disco"
	"tailscale.com/net/batching"
	"tailscale.com/net/packet"
	"tailscale.com/types/nettype"
)

// Only packet type, size and an opaque socket label leave this boundary.
// In particular, neither disco bodies nor WireGuard keys/payloads are logged.
type underlayListener struct {
	nettype.PacketListener
	engineLabel string
}

func (l underlayListener) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	c, e := l.PacketListener.ListenPacket(ctx, network, address)
	if e != nil {
		return nil, e
	}
	trace := &underlayPacketConn{PacketConn: c, engineLabel: l.engineLabel}
	// Keep magicsock's recvmmsg/sendmmsg and UDP GSO/GRO fast path underneath
	// the metadata observer. A plain PacketConn wrapper hides *net.UDPConn.
	upgraded := batching.TryUpgradeToConn(c.(nettype.PacketConn), network, "magicsock_udp_rxq_overflows", nil)
	if batch, ok := upgraded.(batching.Conn); ok {
		return &underlayBatchedConn{underlayPacketConn: trace, batch: batch}, nil
	}
	return trace, nil
}

type underlayPacketConn struct {
	net.PacketConn
	engineLabel     string
	ingress, egress atomic.Int64
}

func (c *underlayPacketConn) trace(direction string, b []byte, address net.Addr, err error) {
	if len(b) < 4 {
		return
	}
	kind := binary.LittleEndian.Uint32(b[:4])
	if bytes.HasPrefix(b, []byte(disco.Magic)) {
		kind = 5
	}
	if kind < 1 || kind > 5 {
		return
	}
	if kind == 4 {
		last := &c.egress
		if direction == "ingress" {
			last = &c.ingress
		}
		now := time.Now().UnixNano()
		prior := last.Load()
		if now-prior < int64(100*time.Millisecond) || !last.CompareAndSwap(prior, now) {
			return
		}
	}
	label := ""
	if address != nil {
		label = underlayLabel(address.String())
	}
	diagnosticlog.TryInfo("native underlay packet", "engine_label", c.engineLabel, "direction", direction, "packet_type", kind, "bytes", len(b), "endpoint_label", label, "failed", err != nil, "observed_unix_us", time.Now().UnixMicro())
}
func (c *underlayPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, a, e := c.PacketConn.ReadFrom(b)
	if n > 0 {
		c.trace("ingress", b[:n], a, e)
	}
	return n, a, e
}
func (c *underlayPacketConn) WriteTo(b []byte, a net.Addr) (int, error) {
	n, e := c.PacketConn.WriteTo(b, a)
	c.trace("egress", b, a, e)
	return n, e
}

func (c *underlayPacketConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	n, a, e := c.PacketConn.(nettype.PacketConn).ReadFromUDPAddrPort(b)
	if n > 0 {
		c.trace("ingress", b[:n], net.UDPAddrFromAddrPort(a), e)
	}
	return n, a, e
}
func (c *underlayPacketConn) WriteToUDPAddrPort(b []byte, a netip.AddrPort) (int, error) {
	n, e := c.PacketConn.(nettype.PacketConn).WriteToUDPAddrPort(b, a)
	c.trace("egress", b, net.UDPAddrFromAddrPort(a), e)
	return n, e
}

func underlayLabel(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:8])
}

type underlayBatchedConn struct {
	*underlayPacketConn
	batch batching.Conn
}

func (c *underlayBatchedConn) Close() error { return c.batch.Close() }
func (c *underlayBatchedConn) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	return c.batch.ReadFromUDPAddrPort(b)
}
func (c *underlayBatchedConn) ReadBatch(slab []byte, packets []batching.ReceivedPacket) (int, error) {
	n, err := c.batch.ReadBatch(slab, packets)
	for _, p := range packets[:n] {
		c.trace("ingress", slab[p.Offset:p.Offset+p.Size], net.UDPAddrFromAddrPort(p.Source), err)
	}
	return n, err
}
func (c *underlayBatchedConn) WriteBatchTo(buffers [][]byte, address netip.AddrPort, geneve packet.GeneveHeader, offset int) error {
	err := c.batch.WriteBatchTo(buffers, address, geneve, offset)
	for _, b := range buffers {
		c.trace("egress", b[offset:], net.UDPAddrFromAddrPort(address), err)
	}
	return err
}
