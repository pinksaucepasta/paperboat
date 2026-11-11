//go:build linux

package machineservices

import (
	"context"
	"encoding/binary"
	"net"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func diagMessage(family, state uint8, port uint16, address []byte, uid uint32) []byte {
	message := make([]byte, diagMessageBytes)
	message[0], message[1] = family, state
	binary.BigEndian.PutUint16(message[4:], port)
	copy(message[8:], address)
	binary.NativeEndian.PutUint32(message[64:], uid)
	return message
}

// diagService applies the rule parseProcTCP applies to a /proc line: a listener of the owner on the wildcard or the
// loopback address, with a port.
func TestDiagServiceKeepsTheOwnersWildcardAndLoopbackListeners(t *testing.T) {
	loopback6 := make([]byte, 16)
	loopback6[15] = 1
	routable := []byte{192, 0, 2, 7}
	for name, test := range map[string]struct {
		message []byte
		family  uint8
		want    Service
		ok      bool
	}{
		"ipv4 loopback":      {diagMessage(unix.AF_INET, diagListenState, 8080, []byte{127, 0, 0, 1}, 1000), unix.AF_INET, Service{Port: 8080, Loopback: "127.0.0.1"}, true},
		"ipv4 wildcard":      {diagMessage(unix.AF_INET, diagListenState, 3000, nil, 1000), unix.AF_INET, Service{Port: 3000, Loopback: "127.0.0.1"}, true},
		"ipv6 loopback":      {diagMessage(unix.AF_INET6, diagListenState, 8081, loopback6, 1000), unix.AF_INET6, Service{Port: 8081, Loopback: "::1"}, true},
		"ipv6 wildcard":      {diagMessage(unix.AF_INET6, diagListenState, 3001, nil, 1000), unix.AF_INET6, Service{Port: 3001, Loopback: "::1"}, true},
		"another user":       {diagMessage(unix.AF_INET, diagListenState, 8080, []byte{127, 0, 0, 1}, 1001), unix.AF_INET, Service{}, false},
		"a routable address": {diagMessage(unix.AF_INET, diagListenState, 8080, routable, 1000), unix.AF_INET, Service{}, false},
		"not listening":      {diagMessage(unix.AF_INET, 1, 8080, []byte{127, 0, 0, 1}, 1000), unix.AF_INET, Service{}, false},
		"port zero":          {diagMessage(unix.AF_INET, diagListenState, 0, []byte{127, 0, 0, 1}, 1000), unix.AF_INET, Service{}, false},
		"wrong family":       {diagMessage(unix.AF_INET6, diagListenState, 8080, nil, 1000), unix.AF_INET, Service{}, false},
		"truncated":          {diagMessage(unix.AF_INET, diagListenState, 8080, nil, 1000)[:40], unix.AF_INET, Service{}, false},
	} {
		got, ok := diagService(test.message, test.family, 1000)
		if ok != test.ok || got != test.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", name, got, ok, test.want, test.ok)
		}
	}
}

// The kernel's listener dump and /proc/net/tcp must agree on a live machine: both find the listeners this test
// opens (IPv4 loopback, IPv4 wildcard, IPv6 loopback) and neither reports a connected socket.
func TestListenersByDiagAgreeWithProcOnALiveMachine(t *testing.T) {
	var opened []uint16
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:0", "[::1]:0"} {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			t.Logf("skipping %s: %v", address, err)
			continue
		}
		defer listener.Close()
		port := uint16(listener.Addr().(*net.TCPAddr).Port)
		opened = append(opened, port)
		// A connected pair on the listener: its sockets are established, not listening.
		if client, err := net.Dial("tcp", listener.Addr().String()); err == nil {
			defer client.Close()
		}
	}
	if len(opened) == 0 {
		t.Skip("no listener could be opened")
	}
	ports := func(services []Service) []uint16 {
		var out []uint16
		for _, service := range services {
			out = append(out, service.Port)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	diag, err := listenersByDiag(context.Background(), uint64(unixGeteuid()))
	if err != nil {
		t.Skipf("sock_diag is not available here: %v", err)
	}
	proc, err := snapshotProc(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, port := range opened {
		if !slices.Contains(ports(diag), port) || !slices.Contains(ports(proc), port) {
			t.Errorf("port %d: diag %v, proc %v", port, ports(diag), ports(proc))
		}
	}
	if !slices.Equal(ports(diag), ports(proc)) {
		t.Errorf("the two sources disagree: diag %v, proc %v", ports(diag), ports(proc))
	}
}

func unixGeteuid() int { return unix.Geteuid() }

// BenchmarkPortWatchWithManyConnections is the worker's two-second poll on a machine holding n established TCP
// connections: /proc renders every socket, the listener dump does not.
func BenchmarkPortWatchWithManyConnections(b *testing.B) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Skip(err)
	}
	defer listener.Close()
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			defer connection.Close()
		}
	}()
	for range 2000 {
		client, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			break
		}
		defer client.Close()
	}
	owner := uint64(unix.Geteuid())
	b.Run("proc", func(b *testing.B) {
		for range b.N {
			if _, err := snapshotProc(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("diag", func(b *testing.B) {
		for range b.N {
			if _, err := listenersByDiag(context.Background(), owner); err != nil {
				b.Skip(err)
			}
		}
	})
}
