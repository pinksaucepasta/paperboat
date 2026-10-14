//go:build darwin

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDarwinSocketDescriptorValidation(t *testing.T) {
	for _, kind := range []int{unix.SOCK_DGRAM, unix.SOCK_STREAM} {
		fd, err := unix.Socket(unix.AF_INET, kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		if kind == unix.SOCK_STREAM {
			if err = unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
				unix.Close(fd)
				t.Fatal(err)
			}
		}
		listener, err := bindDarwinSocket(t.Context(), fd, "127.100.0.4:54389", "")
		if listener != nil {
			listener.Close()
			t.Fatal("invalid socket accepted")
		}
		if err == nil || errors.Is(err, ErrPortInUse) {
			t.Fatalf("wrong failure: %v", err)
		}
		if _, err = unix.Getsockname(fd); !errors.Is(err, unix.EBADF) {
			t.Fatal("rejected descriptor leaked", err)
		}
	}
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = bindDarwinSocket(ctx, fd, "127.100.0.4:54389", ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = unix.Getsockname(fd); !errors.Is(err, unix.EBADF) {
		t.Fatal("canceled descriptor leaked", err)
	}
}

func TestDarwinSocketHandoffValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		marker []byte
		count  int
		valid  bool
	}{
		{"single socket", []byte{1}, 1, true},
		{"missing descriptor", []byte{1}, 0, false},
		{"multiple descriptors", []byte{1}, 2, false},
		{"invalid marker", []byte{9}, 1, false},
		{"extra marker byte", []byte{1, 1}, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			conns := make([]*net.UnixConn, 2)
			for i, fd := range pair {
				f := os.NewFile(uintptr(fd), "channel")
				c, e := net.FileConn(f)
				f.Close()
				if e != nil {
					t.Fatal(e)
				}
				conns[i] = c.(*net.UnixConn)
				defer c.Close()
			}
			var fds []int
			for i := 0; i < tt.count; i++ {
				fd, e := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP)
				if e != nil {
					t.Fatal(e)
				}
				fds = append(fds, fd)
				defer unix.Close(fd)
			}
			var oob []byte
			if len(fds) > 0 {
				oob = unix.UnixRights(fds...)
			}
			if _, _, err = conns[0].WriteMsgUnix(tt.marker, oob, nil); err != nil {
				t.Fatal(err)
			}
			fd, err := receiveDarwinSocket(conns[1])
			if fd >= 0 {
				defer unix.Close(fd)
			}
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%t error=%v", tt.valid, err)
			}
		})
	}
}

// This exercises the complete native bind path, not just rejection predicates.
// The fixture owns only a temporary loopback alias and does not alter PF policy.
func TestDarwinSocketBindAndConflict(t *testing.T) {
	if os.Getenv("PAPERBOAT_GUARD_DARWIN_TEST") != "1" || os.Geteuid() != 0 {
		t.Skip("requires authorized Darwin loopback qualification")
	}
	const host = "127.123.253.72"
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	existed := false
	for _, address := range addresses {
		ip, _, e := net.ParseCIDR(address.String())
		if e == nil && ip.String() == host {
			existed = true
		}
	}
	if !existed {
		// This standalone socket test has no guard alias journal. Create only
		// its own alias directly, without touching global guard/PF state.
		if out, aliasErr := exec.CommandContext(t.Context(), "/sbin/ifconfig", "lo0", "alias", host, "255.255.255.255").CombinedOutput(); aliasErr != nil {
			t.Fatalf("create owned loopback alias: %v %s", aliasErr, out)
		}
		defer func() {
			if out, e := exec.Command("/sbin/ifconfig", "lo0", "-alias", host).CombinedOutput(); e != nil {
				t.Errorf("remove owned alias: %v %s", e, out)
			}
		}()
	}
	probe, err := net.Listen("tcp4", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	address := fmt.Sprintf("%s:%d", host, port)
	makeSocket := func() int {
		fd, e := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP)
		if e != nil {
			t.Fatal(e)
		}
		return fd
	}
	fd := makeSocket()
	listener, err := bindDarwinSocket(t.Context(), fd, address, "127.123.0.0/16")
	if err != nil {
		t.Fatal("valid unbound TCP socket rejected", err)
	}
	defer listener.Close()
	if listener.Addr().String() != address {
		t.Fatalf("bound unexpected address %s", listener.Addr())
	}
	if _, err = unix.Getsockname(fd); !errors.Is(err, unix.EBADF) {
		t.Fatal("original descriptor leaked", err)
	}
	conflicting := makeSocket()
	other, err := bindDarwinSocket(t.Context(), conflicting, address, "127.123.0.0/16")
	if other != nil {
		other.Close()
		t.Fatal("occupied listener accepted")
	}
	if !errors.Is(err, ErrPortInUse) {
		t.Fatalf("occupied bind classification: %v", err)
	}
	if _, err = unix.Getsockname(conflicting); !errors.Is(err, unix.EBADF) {
		t.Fatal("failed bind descriptor leaked", err)
	}
}
