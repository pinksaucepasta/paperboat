// Copyright (c) Tailscale Inc & contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"net"
	"sync"
	"time"
)

// ConnPacketConn is a connected datagram socket. Read and Write preserve UDP
// datagram boundaries, while the net.PacketConn methods are available to code
// that prefers packet-oriented APIs. LocalAddr and RemoteAddr identify the
// destination and source endpoints of an incoming server flow.
type ConnPacketConn interface {
	net.Conn
	net.PacketConn
}

// MaxUDPPayload is the largest UDP payload that fits the tunnel's 1280-byte
// IPv6 MTU without IP fragmentation (1280 minus 40 bytes of IPv6 header and 8
// bytes of UDP header). Applications should keep datagrams at or below this
// size; larger writes are not guaranteed to reach the peer.
const MaxUDPPayload = 1232

// DefaultUDPIdleTimeout is the amount of inactivity after which an incoming
// UDP flow is closed.
const DefaultUDPIdleTimeout = 2 * time.Minute

// idlePacketConn closes c after timeout without a successful read or write.
// It is used for server-side UDP flows, which do not otherwise have a natural
// close signal from the peer.
type idlePacketConn struct {
	ConnPacketConn
	timeout  time.Duration
	timer    *time.Timer
	deadline time.Time

	mu     sync.Mutex
	closed bool
}

func newIdlePacketConn(c ConnPacketConn, timeout time.Duration) *idlePacketConn {
	ic := &idlePacketConn{ConnPacketConn: c, timeout: timeout, deadline: time.Now().Add(timeout)}
	ic.timer = time.AfterFunc(timeout, ic.checkIdle)
	return ic
}

// checkIdle closes the flow if the deadline has passed. If activity extended
// the deadline while the timer was firing, it reschedules instead of closing,
// so a concurrent touch cannot be followed by a spurious close.
func (c *idlePacketConn) checkIdle() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	if remaining := time.Until(c.deadline); remaining > 0 {
		c.timer.Reset(remaining)
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.mu.Unlock()
	c.ConnPacketConn.Close()
}

func (c *idlePacketConn) touch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.deadline = time.Now().Add(c.timeout)
	c.timer.Reset(c.timeout)
}

func (c *idlePacketConn) Read(b []byte) (int, error) {
	n, err := c.ConnPacketConn.Read(b)
	if err == nil {
		c.touch()
	}
	return n, err
}

func (c *idlePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.ConnPacketConn.ReadFrom(b)
	if err == nil {
		c.touch()
	}
	return n, addr, err
}

func (c *idlePacketConn) Write(b []byte) (int, error) {
	n, err := c.ConnPacketConn.Write(b)
	if err == nil {
		c.touch()
	}
	return n, err
}

func (c *idlePacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.ConnPacketConn.WriteTo(b, addr)
	if err == nil {
		c.touch()
	}
	return n, err
}

func (c *idlePacketConn) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return net.ErrClosed
	}
	c.closed = true
	c.timer.Stop()
	c.mu.Unlock()
	return c.ConnPacketConn.Close()
}
