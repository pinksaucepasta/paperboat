// Package bandwidth measures successful application I/O once, at its host.
package bandwidth

import (
	"net"
	"time"
)

type Path struct{ Mode, NodeID string }
type Binding struct {
	AccessSessionID, StreamID, Consumer string
	Reverse                             bool
}
type Observer func(string) Path

type meteredConn struct {
	net.Conn
	recorder *Recorder
	binding  Binding
	observe  Observer
}

func (r *Recorder) Wrap(conn net.Conn, binding Binding, observe Observer) net.Conn {
	if r == nil || conn == nil || observe == nil {
		return conn
	}
	return &meteredConn{conn, r, binding, observe}
}

func samePath(before, after Path) Path {
	if before != after {
		return Path{Mode: "unknown"}
	}
	if before.Mode != "direct" && before.Mode != "relay" && before.Mode != "edge" {
		return Path{Mode: "unknown"}
	}
	if before.Mode == "direct" {
		before.NodeID = ""
	}
	return before
}

func (c *meteredConn) Read(p []byte) (int, error) {
	direction := "upload"
	if c.binding.Reverse {
		direction = "download"
	}
	before := c.observe(direction)
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.recorder.add(c.binding, samePath(before, c.observe(direction)), int64(n), !c.binding.Reverse, time.Now().UTC())
	}
	return n, err
}
func (c *meteredConn) Write(p []byte) (int, error) {
	direction := "download"
	if c.binding.Reverse {
		direction = "upload"
	}
	before := c.observe(direction)
	n, err := c.Conn.Write(p)
	if n > 0 {
		c.recorder.add(c.binding, samePath(before, c.observe(direction)), int64(n), c.binding.Reverse, time.Now().UTC())
	}
	return n, err
}

// Preserve half-close semantics used by SSH and TCP forwarding.
func (c *meteredConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return nil
}
func (c *meteredConn) CloseRead() error {
	if half, ok := c.Conn.(interface{ CloseRead() error }); ok {
		return half.CloseRead()
	}
	return nil
}
