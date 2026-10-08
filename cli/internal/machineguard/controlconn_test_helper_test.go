//go:build linux || darwin || windows

package machineguard

import (
	"io"
	"net"
)

type testControlConn struct{ identity string }

func (c *testControlConn) Identity() (string, error)       { return c.identity, nil }
func (*testControlConn) Receive() (request, error)         { return request{}, io.EOF }
func (*testControlConn) Send(response, net.Listener) error { return nil }
func (*testControlConn) Close() error                      { return nil }
