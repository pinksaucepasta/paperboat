//go:build linux || darwin

package machineguard

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// Passing a listener on must leave it non-blocking: os.File.Fd on its dup made the guard's and daemon's
// Accept a blocking syscall that Close and deadlines could not end (machine guard requests then timed out).
func TestSocketDescriptorKeepsListenerNonBlocking(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fd, err := socketDescriptor(file)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK == 0 {
		t.Fatal("the shared listener socket was switched to blocking mode")
	}
	file.Fd() // the old path: proves the check sees the regression
	if flags, _ = unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0); flags&unix.O_NONBLOCK != 0 {
		t.Fatal("os.File.Fd no longer switches to blocking; the check cannot see the regression")
	}
}
