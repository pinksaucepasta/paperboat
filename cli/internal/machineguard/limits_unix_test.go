//go:build linux || darwin

package machineguard

import (
	"io"
	"net"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

func receiveFrame(r io.Reader) error { _, err := readFrame(r); return err }

func oversizeClient(t *testing.T) (*Client, func()) {
	t.Helper()
	path := filepath.Join(shorttemp.Dir(t), "s")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{conn: conn.(*net.UnixConn)}, func() { conn.Close(); listener.Close() }
}
