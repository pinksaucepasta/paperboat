//go:build windows

package machineguard

import (
	"io"
	"net"
	"testing"
)

func receiveFrame(r io.Reader) error { var in request; return readFrame(r, &in) }

func oversizeClient(t *testing.T) (*Client, func()) {
	t.Helper()
	conn, peer := net.Pipe()
	return &Client{conn: conn}, func() { conn.Close(); peer.Close() }
}
