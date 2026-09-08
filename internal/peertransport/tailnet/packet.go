// Package tailnet owns Paperboat's virtual UDP boundary over Tailcat.
package tailnet

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tailcat"
)

const MaxPayload = tailcat.MaxUDPPayload

var (
	ErrWrongPeer       = errors.New("virtual UDP destination is not the authorized peer")
	ErrPayloadTooLarge = errors.New("virtual UDP payload exceeds MTU")
)

// Packet owns a connected Tailcat flow. It adds no queue and delegates deadlines
// and concurrent I/O to the underlying netstack socket.
type Packet struct {
	conn          tailcat.ConnPacketConn
	once          sync.Once
	closeErr      error
	release       func()
	permit        func() bool
	closed        atomic.Bool
	local, remote net.Addr
}

func newPacket(c tailcat.ConnPacketConn, release func()) *Packet {
	return &Packet{conn: c, release: release, local: c.LocalAddr(), remote: c.RemoteAddr()}
}
func (p *Packet) LocalAddr() net.Addr  { return p.local }
func (p *Packet) RemoteAddr() net.Addr { return p.remote }
func (*Packet) MaxPayload() uint16     { return MaxPayload }
func (p *Packet) Write(b []byte) (int, error) {
	if p.closed.Load() {
		return 0, net.ErrClosed
	}
	if p.permit != nil && !p.permit() {
		return 0, ErrAdmission
	}
	if len(b) > MaxPayload {
		return 0, ErrPayloadTooLarge
	}
	return p.conn.Write(b)
}
func (p *Packet) WriteTo(b []byte, addr net.Addr) (int, error) {
	peer := p.RemoteAddr()
	if addr == nil || addr.Network() != peer.Network() || addr.String() != peer.String() {
		return 0, ErrWrongPeer
	}
	return p.Write(b)
}
func (p *Packet) Close() error {
	p.once.Do(func() {
		p.closed.Store(true)
		p.closeErr = p.conn.Close()
		if p.release != nil {
			p.release()
		}
	})
	return p.closeErr
}

func (p *Packet) Read(b []byte) (int, error) { n, _, err := p.ReadFrom(b); return n, err }
func (p *Packet) ReadFrom(b []byte) (int, net.Addr, error) {
	if p.closed.Load() {
		return 0, nil, net.ErrClosed
	}
	if p.permit != nil && !p.permit() {
		return 0, nil, ErrAdmission
	}
	// Inspect the full allowed payload even when the caller requests truncation.
	// This bounds per-read storage and rejects oversized reassembled datagrams.
	var packet [MaxPayload + 1]byte
	n, addr, err := p.conn.ReadFrom(packet[:])
	if p.closed.Load() {
		return 0, nil, net.ErrClosed
	}
	if err != nil {
		return 0, addr, err
	}
	if p.permit != nil && !p.permit() {
		return 0, nil, ErrAdmission
	}
	if n > MaxPayload {
		return 0, addr, ErrPayloadTooLarge
	}
	return copy(b, packet[:n]), addr, nil
}
func (p *Packet) SetDeadline(t time.Time) error      { return p.conn.SetDeadline(t) }
func (p *Packet) SetReadDeadline(t time.Time) error  { return p.conn.SetReadDeadline(t) }
func (p *Packet) SetWriteDeadline(t time.Time) error { return p.conn.SetWriteDeadline(t) }
