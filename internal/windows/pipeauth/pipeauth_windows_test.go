//go:build windows

package pipeauth

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
)

type trackedPipe struct{ closed bool }

func (p *trackedPipe) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (p *trackedPipe) Write(b []byte) (int, error)      { return len(b), nil }
func (p *trackedPipe) Close() error                     { p.closed = true; return nil }
func (p *trackedPipe) LocalAddr() net.Addr              { return nil }
func (p *trackedPipe) RemoteAddr() net.Addr             { return nil }
func (p *trackedPipe) SetDeadline(time.Time) error      { return nil }
func (p *trackedPipe) SetReadDeadline(time.Time) error  { return nil }
func (p *trackedPipe) SetWriteDeadline(time.Time) error { return nil }

func TestDialCurrentUserRejectsForeignOwner(t *testing.T) {
	originalDial, originalOwner, originalExpected := dialPipe, connectedOwner, currentOwner
	t.Cleanup(func() { dialPipe, connectedOwner, currentOwner = originalDial, originalOwner, originalExpected })
	pipe := &trackedPipe{}
	dialPipe = func(context.Context, string) (net.Conn, error) { return pipe, nil }
	connectedOwner = func(net.Conn) (string, error) { return "S-1-5-18", nil }
	currentOwner = func() (string, error) { return "S-1-5-21-1-2-3-1001", nil }
	if _, err := DialCurrentUser(context.Background(), `\\.\pipe\impostor`); err == nil || !pipe.closed {
		t.Fatalf("foreign pipe result err=%v closed=%v", err, pipe.closed)
	}
}

func TestDialCurrentUserAcceptsOwnerAndFailsClosed(t *testing.T) {
	originalDial, originalOwner, originalExpected := dialPipe, connectedOwner, currentOwner
	t.Cleanup(func() { dialPipe, connectedOwner, currentOwner = originalDial, originalOwner, originalExpected })
	expected := "S-1-5-21-1-2-3-1001"
	currentOwner = func() (string, error) { return expected, nil }
	for _, test := range []struct {
		name    string
		owner   func(net.Conn) (string, error)
		wantErr bool
	}{
		{name: "current owner", owner: func(net.Conn) (string, error) { return expected, nil }},
		{name: "unreadable owner", owner: func(net.Conn) (string, error) { return "", errors.New("denied") }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pipe := &trackedPipe{}
			dialPipe = func(context.Context, string) (net.Conn, error) { return pipe, nil }
			connectedOwner = test.owner
			conn, err := DialCurrentUser(context.Background(), `\\.\pipe\test`)
			if (err != nil) != test.wantErr || pipe.closed != test.wantErr {
				t.Fatalf("conn=%v err=%v closed=%v", conn, err, pipe.closed)
			}
		})
	}
}

const (
	nativeForeignPipe = `\\.\pipe\paperboat-daemonrpc-owner-review`
	nativeReadyFile   = `C:\ProgramData\paperboat-daemonrpc-owner-review.ready`
)

func TestSystemOwnedPipeFixture(t *testing.T) {
	owner, err := currentOwner()
	if err != nil {
		t.Fatal(err)
	}
	if owner != "S-1-5-18" {
		t.Skip("fixture requires LocalSystem")
	}
	listener, err := winio.ListenPipe(nativeForeignPipe, &winio.PipeConfig{SecurityDescriptor: "O:SYD:P(A;;GRGW;;;AU)(A;;GA;;;SY)"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = os.WriteFile(nativeReadyFile, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(nativeReadyFile)
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestNativeDialRejectsSystemOwnedImpostor(t *testing.T) {
	if os.Getenv("PAPERBOAT_DAEMONRPC_PRIVILEGED_TEST") != "1" {
		t.Skip("privileged native fixture is not enabled")
	}
	conn, err := DialCurrentUser(context.Background(), nativeForeignPipe)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil {
		t.Fatal("accepted LocalSystem-owned impostor pipe for current user")
	}
}
