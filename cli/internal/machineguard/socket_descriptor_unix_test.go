//go:build linux || darwin

package machineguard

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// unixPair returns both ends of a connected Unix stream pair.
func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	ends := make([]*net.UnixConn, 2)
	for i, fd := range fds {
		file := os.NewFile(uintptr(fd), "pair")
		conn, err := net.FileConn(file)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
		ends[i] = conn.(*net.UnixConn)
		t.Cleanup(func() { ends[i].Close() })
	}
	return ends[0], ends[1]
}

// drain reads one message with its descriptors off the pair and closes them.
func drain(t *testing.T, conn *net.UnixConn) {
	t.Helper()
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := conn.ReadMsgUnix(make([]byte, 64<<10), oob)
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := unix.ParseSocketControlMessage(oob[:oobn])
	for _, message := range messages {
		fds, _ := unix.ParseUnixRights(&message)
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
}

func nonBlocking(t *testing.T, conn syscall.Conn) bool {
	t.Helper()
	raw, err := conn.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags int
	if err := raw.Control(func(fd uintptr) { flags, err = unix.FcntlInt(fd, unix.F_GETFL, 0) }); err != nil {
		t.Fatal(err)
	}
	return flags&unix.O_NONBLOCK != 0
}

// After the guard hands a listener to the daemon, its own listener must stay non-blocking so Close still ends a
// pending Accept. os.File.Fd on the dup switched it to blocking: Accept pinned a thread forever and guard
// requests timed out (dadape: 31 listeners stuck in accept4).
func TestHandedOffListenerStaysInterruptible(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	guardEnd, daemonEnd := unixPair(t)
	if err := (&unixControlConn{guardEnd}).Send(response{}, listener); err != nil {
		t.Fatal(err)
	}
	drain(t, daemonEnd)
	if !nonBlocking(t, listener.(*net.TCPListener)) {
		t.Fatal("handing off the listener switched it to blocking mode")
	}
	accepted := make(chan error, 1)
	go func() { _, err := listener.Accept(); accepted <- err }()
	time.Sleep(50 * time.Millisecond)
	listener.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not end a pending Accept on the handed-off listener")
	}
}

// The same for a gateway connection passed to its daemon: a read deadline must still end a pending Read.
func TestHandedOffGatewayConnStaysInterruptible(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	client, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer accepted.Close()
	file, err := accepted.(*net.TCPConn).File()
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	guardEnd, daemonEnd := unixPair(t)
	if err := (&gatewayRoute{conn: guardEnd}).pass(file); err != nil {
		t.Fatal(err)
	}
	drain(t, daemonEnd)
	if !nonBlocking(t, accepted.(*net.TCPConn)) {
		t.Fatal("passing the connection switched it to blocking mode")
	}
	_ = accepted.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	read := make(chan error, 1)
	go func() { _, err := accepted.Read(make([]byte, 1)); read <- err }()
	select {
	case <-read:
	case <-time.After(2 * time.Second):
		t.Fatal("a read deadline did not end a pending Read on the passed connection")
	}
}
