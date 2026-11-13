// Package handover passes an edge process's listening sockets and process epoch to its
// replacement, so an install hands over instead of stopping. The new process binds nothing
// itself: it starts on the inherited sockets, warms up while the old one still serves, then
// takes over accepting while the old one drains and exits. One shared socket means one kernel
// accept queue, so no connection is refused or reset at the switch.
//
// Prior art: nginx binary upgrade (~/ref/nginx/src/core/nginx.c:720, the new binary inherits the
// listening fds) and Envoy hot restart (listen sockets passed over a Unix socket with SCM_RIGHTS;
// the parent drains once the child is ready). SO_REUSEPORT was rejected: adding a second UDP
// socket re-hashes live QUIC flows to the new process before it is ready, and a closing TCP
// socket's accept queue is reset.
package handover

import (
	"errors"
	"net"
	"os"
	"sync"
)

// sockets is this process's set of listening sockets: the ones it inherited and has not used
// yet, and every one it listens on (kept as dup'd files so they can be passed on).
type sockets struct {
	mu        sync.Mutex
	inherited map[string]*os.File
	owned     map[string]*os.File
	tcp       map[*listener]struct{}
	// accept is closed once this process may accept TCP connections; udp once it may read UDP.
	accept, udp chan struct{}
	// released is closed once this process handed accepting over to its replacement.
	released chan struct{}
}

var current = fresh()

func fresh() *sockets {
	open := make(chan struct{})
	close(open)
	return &sockets{inherited: map[string]*os.File{}, owned: map[string]*os.File{}, tcp: map[*listener]struct{}{}, accept: open, udp: open, released: make(chan struct{})}
}

func key(network, address string) string { return network + " " + address }

// Listen is net.Listen for the edge: it returns the inherited socket for this address when there
// is one. Its Accept waits until this process owns accepting.
func Listen(network, address string) (net.Listener, error) { return current.listen(network, address) }

// ListenPacket is net.ListenPacket for the edge. The caller must not read before UDPReady.
func ListenPacket(network, address string) (net.PacketConn, error) {
	return current.listenPacket(network, address)
}

// UDPReady is closed once this process may read its UDP sockets: at once for a fresh process,
// after the previous process exited for an inheriting one.
func UDPReady() <-chan struct{} { return current.udpReady() }

func (s *sockets) udpReady() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.udp
}

func (s *sockets) take(k string) *os.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	f := s.inherited[k]
	delete(s.inherited, k)
	return f
}

func (s *sockets) listen(network, address string) (net.Listener, error) {
	k := key(network, address)
	var inner net.Listener
	var file *os.File
	var err error
	if file = s.take(k); file != nil {
		inner, err = net.FileListener(file)
	} else if inner, err = net.Listen(network, address); err == nil {
		if tcp, ok := inner.(*net.TCPListener); ok {
			file, err = tcp.File()
		}
	}
	if err != nil {
		if inner != nil {
			_ = inner.Close()
		}
		if file != nil {
			_ = file.Close()
		}
		return nil, err
	}
	l := &listener{Listener: inner, set: s, key: k, file: file, closed: make(chan struct{})}
	s.mu.Lock()
	if file != nil {
		s.owned[k] = file
	}
	s.tcp[l] = struct{}{}
	accept, released := s.accept, s.released
	s.mu.Unlock()
	l.accept, l.released = accept, released
	return l, nil
}

func (s *sockets) listenPacket(network, address string) (net.PacketConn, error) {
	k := key(network, address)
	var conn net.PacketConn
	var file *os.File
	var err error
	if file = s.take(k); file != nil {
		conn, err = net.FilePacketConn(file)
	} else if conn, err = net.ListenPacket(network, address); err == nil {
		if udp, ok := conn.(*net.UDPConn); ok {
			file, err = udp.File()
		}
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		if file != nil {
			_ = file.Close()
		}
		return nil, err
	}
	udp, ok := conn.(*net.UDPConn)
	if !ok {
		return conn, nil
	}
	s.mu.Lock()
	if file != nil {
		s.owned[k] = file
	}
	s.mu.Unlock()
	// Embedding keeps every *net.UDPConn method, so quic-go still uses its batched, ECN and GSO path.
	return &packetConn{UDPConn: udp, set: s, key: k, file: file}, nil
}

func (s *sockets) forget(k string, file *os.File) {
	s.mu.Lock()
	if s.owned[k] == file {
		delete(s.owned, k)
	}
	s.mu.Unlock()
	if file != nil {
		_ = file.Close()
	}
}

// snapshot is every owned socket, for passing to a replacement.
func (s *sockets) snapshot() (keys []string, files []*os.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, f := range s.owned {
		keys, files = append(keys, k), append(files, f)
	}
	return keys, files
}

// release stops this process accepting TCP: every listener's own descriptor is closed, so the
// replacement's descriptor of the same socket is the only one left accepting. Accept then waits
// until the listener is closed, so servers see an idle listener, not a failure.
func (s *sockets) release() {
	s.mu.Lock()
	select {
	case <-s.released:
		s.mu.Unlock()
		return
	default:
	}
	close(s.released)
	listeners := make([]*listener, 0, len(s.tcp))
	for l := range s.tcp {
		listeners = append(listeners, l)
	}
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Listener.Close()
	}
}

type listener struct {
	net.Listener
	set              *sockets
	key              string
	file             *os.File
	accept, released <-chan struct{}
	closed           chan struct{}
	once             sync.Once
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case <-l.accept:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		select {
		case <-l.released:
			<-l.closed
			return nil, net.ErrClosed
		default:
		}
	}
	return c, err
}

func (l *listener) Close() error {
	err := net.ErrClosed
	l.once.Do(func() {
		close(l.closed)
		l.set.mu.Lock()
		delete(l.set.tcp, l)
		l.set.mu.Unlock()
		l.set.forget(l.key, l.file)
		err = l.Listener.Close()
		select {
		case <-l.released:
			err = nil // its descriptor was closed at release
		default:
		}
	})
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

type packetConn struct {
	*net.UDPConn
	set  *sockets
	key  string
	file *os.File
	once sync.Once
}

func (c *packetConn) Close() error {
	var err error
	c.once.Do(func() {
		c.set.forget(c.key, c.file)
		err = c.UDPConn.Close()
	})
	return err
}
