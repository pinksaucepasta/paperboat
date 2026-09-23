//go:build windows

package deviceguard

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// go-winio creates pipes with FILE_PIPE_REJECT_REMOTE_CLIENTS. Do not deny the
// NETWORK SID here: a local process launched through OpenSSH retains that SID.
const pipeSDDL = "O:SYD:P(D;;GA;;;AN)(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x0012019b;;;AU)"
const pipeClientAccess = 0x0012019b
const pipeHandshakeTimeout = 5 * time.Second

var pipeClientPreface = []byte("paperboat-deviceguard/v1\n")

type authenticatedPipe struct {
	conn     net.Conn
	identity string
	pid      uint32
	err      error
}

type windowsPipeAuthenticator struct {
	listener net.Listener
	ready    chan authenticatedPipe
	done     chan struct{}
	once     sync.Once
	slots    chan struct{}
	wg       sync.WaitGroup
}

func newWindowsPipeAuthenticator(listener net.Listener) *windowsPipeAuthenticator {
	a := &windowsPipeAuthenticator{listener: listener, ready: make(chan authenticatedPipe), done: make(chan struct{}), slots: make(chan struct{}, 128)}
	go a.serve()
	return a
}

func (a *windowsPipeAuthenticator) serve() {
	defer func() {
		a.wg.Wait()
		close(a.ready)
	}()
	for {
		conn, err := a.listener.Accept()
		if err != nil {
			select {
			case a.ready <- authenticatedPipe{err: err}:
			case <-a.done:
			}
			return
		}
		select {
		case a.slots <- struct{}{}:
			a.wg.Add(1)
			go a.authenticate(conn)
		default:
			conn.Close()
		}
	}
}

func (a *windowsPipeAuthenticator) authenticate(conn net.Conn) {
	defer a.wg.Done()
	defer func() { <-a.slots }()
	_ = conn.SetReadDeadline(time.Now().Add(pipeHandshakeTimeout))
	preface := make([]byte, len(pipeClientPreface))
	_, err := io.ReadFull(conn, preface)
	if err == nil && !bytes.Equal(preface, pipeClientPreface) {
		err = errors.New("invalid device guard pipe preface")
	}
	var sid string
	var pid uint32
	if err == nil {
		sid, pid, err = pipePeer(conn)
	}
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		conn.Close()
		return
	}
	select {
	case a.ready <- authenticatedPipe{conn: conn, identity: sid, pid: pid}:
	case <-a.done:
		conn.Close()
	}
}

func (a *windowsPipeAuthenticator) AcceptPeer() (authenticatedPipe, error) {
	peer, ok := <-a.ready
	if !ok {
		return authenticatedPipe{}, net.ErrClosed
	}
	return peer, peer.err
}

func (a *windowsPipeAuthenticator) Close() error {
	var err error
	a.once.Do(func() {
		close(a.done)
		err = a.listener.Close()
	})
	return err
}

type windowsControlListener struct{ auth *windowsPipeAuthenticator }

func (l windowsControlListener) Accept() (controlConn, error) {
	peer, err := l.auth.AcceptPeer()
	if err != nil {
		return nil, err
	}
	return &windowsControlConn{Conn: peer.conn, identity: peer.identity, pid: peer.pid}, nil
}

func (l windowsControlListener) Close() error   { return l.auth.Close() }
func (l windowsControlListener) Addr() net.Addr { return l.auth.listener.Addr() }

type windowsControlConn struct {
	net.Conn
	identity string
	pid      uint32
	mu       sync.Mutex
}

func (c *windowsControlConn) Identity() (string, error) { return c.identity, nil }
func (c *windowsControlConn) Receive() (request, error) {
	var v request
	err := readFrame(c.Conn, &v)
	return v, err
}
func (c *windowsControlConn) Send(out response, listener net.Listener) error {
	if listener != nil {
		broker, err := newAcceptBroker(listener, c.identity, c.pid)
		if err != nil {
			return err
		}
		registerBroker(listener, broker)
		out.Socket = []byte(broker.path)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return writeFrame(c.Conn, out)
}
func listenControl(path string) (controlListener, error) {
	l, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSDDL, MessageMode: true, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return nil, err
	}
	return windowsControlListener{auth: newWindowsPipeAuthenticator(l)}, nil
}
func pipePeer(c net.Conn) (string, uint32, error) {
	h, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return "", 0, errors.New("device guard pipe has no Windows handle")
	}
	handle := windows.Handle(h.Fd())
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(handle, &pid); err != nil || pid == 0 {
		return "", 0, errors.New("device guard cannot identify pipe client")
	}
	sid, err := namedPipeClientSID(handle)
	return sid, pid, err
}

func verifySystemPipe(c net.Conn) error {
	h, ok := c.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("device guard pipe has no Windows handle")
	}
	descriptor, err := windows.GetSecurityInfo(windows.Handle(h.Fd()), windows.SE_KERNEL_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil || owner == nil || !owner.Equals(system) {
		return errors.New("device guard pipe is not owned by LocalSystem")
	}
	return nil
}

var impersonateNamedPipeClient = windows.NewLazySystemDLL("advapi32.dll").NewProc("ImpersonateNamedPipeClient")

func namedPipeClientSID(pipe windows.Handle) (string, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	ok, _, callErr := impersonateNamedPipeClient.Call(uintptr(pipe))
	if ok == 0 {
		return "", callErr
	}
	defer func() {
		if err := windows.RevertToSelf(); err != nil {
			panic(fmt.Errorf("revert device guard pipe impersonation: %w", err))
		}
	}()
	var token windows.Token
	if err := windows.OpenThreadToken(windows.CurrentThread(), windows.TOKEN_QUERY, true, &token); err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", errors.New("device guard cannot identify pipe client SID")
	}
	return user.User.Sid.String(), nil
}
func readFrame(r io.Reader, v any) error {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(h[:])
	if n == 0 || n > 64<<10 {
		return errors.New("invalid device guard frame")
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("invalid device guard frame")
	}
	return nil
}
func writeFrame(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 64<<10 {
		return errors.New("invalid device guard frame size")
	}
	var h [4]byte
	binary.LittleEndian.PutUint32(h[:], uint32(len(data)))
	if _, err = w.Write(h[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

type acceptBroker struct {
	path, owner string
	pid         uint32
	tcp         net.Listener
	pipe        *windowsPipeAuthenticator
	done        chan struct{}
	once        sync.Once
	mu          sync.Mutex
	active      map[net.Conn]net.Conn
	pending     net.Conn
	closing     bool
	wg          sync.WaitGroup
}

var proxySlots = make(chan struct{}, 256)

var brokerRegistry = struct {
	sync.Mutex
	byListener map[net.Listener]*acceptBroker
}{byListener: map[net.Listener]*acceptBroker{}}

func newAcceptBroker(listener net.Listener, owner string, pid uint32) (*acceptBroker, error) {
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, err
	}
	path := `\\.\pipe\PaperboatDeviceGuardAccept-` + hex.EncodeToString(token[:])
	pipe, err := winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: pipeSDDL, MessageMode: true, InputBufferSize: 4096, OutputBufferSize: 64 << 10})
	if err != nil {
		return nil, err
	}
	b := &acceptBroker{path: path, owner: owner, pid: pid, tcp: listener, pipe: newWindowsPipeAuthenticator(pipe), done: make(chan struct{}), active: make(map[net.Conn]net.Conn)}
	go b.serve()
	return b, nil
}
func registerBroker(l net.Listener, b *acceptBroker) {
	brokerRegistry.Lock()
	brokerRegistry.byListener[l] = b
	brokerRegistry.Unlock()
}
func (b *acceptBroker) serve() {
	defer close(b.done)
	for {
		peer, err := b.pipe.AcceptPeer()
		if err != nil {
			return
		}
		p := peer.conn
		if peer.identity != b.owner || peer.pid != b.pid {
			p.Close()
			continue
		}
		b.mu.Lock()
		if b.closing {
			b.mu.Unlock()
			p.Close()
			return
		}
		b.pending = p
		b.mu.Unlock()
	acceptTCP:
		c, err := b.tcp.Accept()
		if err != nil {
			p.Close()
			return
		}
		select {
		case proxySlots <- struct{}{}:
		default:
			c.Close()
			goto acceptTCP
		}
		_ = p.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err = writeFrame(p, response{Socket: []byte{1}}); err != nil {
			<-proxySlots
			c.Close()
			p.Close()
			b.mu.Lock()
			b.pending = nil
			b.mu.Unlock()
			continue
		}
		_ = p.SetWriteDeadline(time.Time{})
		b.mu.Lock()
		b.pending = nil
		b.active[c] = p
		b.wg.Add(1)
		b.mu.Unlock()
		go b.proxyAcceptedConnection(c, p)
	}
}

func (b *acceptBroker) proxyAcceptedConnection(tcp net.Conn, pipe net.Conn) {
	defer func() {
		b.mu.Lock()
		delete(b.active, tcp)
		b.mu.Unlock()
		b.wg.Done()
		<-proxySlots
	}()
	defer tcp.Close()
	defer pipe.Close()
	type closeWriter interface{ CloseWrite() error }
	done := make(chan error, 2)
	copyOne := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		if err == nil {
			if writer, ok := dst.(closeWriter); ok {
				err = writer.CloseWrite()
			}
		}
		done <- err
	}
	go copyOne(tcp, pipe)
	go copyOne(pipe, tcp)
	if err := <-done; err != nil {
		tcp.Close()
		pipe.Close()
	}
	<-done
}
func (b *acceptBroker) close() error {
	var err error
	b.once.Do(func() {
		b.mu.Lock()
		b.closing = true
		if b.pending != nil {
			b.pending.Close()
		}
		b.mu.Unlock()
		err = errors.Join(b.pipe.Close(), b.tcp.Close())
		<-b.done
		b.mu.Lock()
		for tcp, pipe := range b.active {
			tcp.Close()
			pipe.Close()
		}
		b.mu.Unlock()
		b.wg.Wait()
	})
	return err
}
func shutdownListener(l net.Listener) error {
	brokerRegistry.Lock()
	b := brokerRegistry.byListener[l]
	delete(brokerRegistry.byListener, l)
	brokerRegistry.Unlock()
	if b != nil {
		return b.close()
	}
	return l.Close()
}

type windowsProxyListener struct {
	path     string
	once     sync.Once
	done     chan struct{}
	mu       sync.Mutex
	acceptMu sync.Mutex
	cancel   context.CancelFunc
	pending  net.Conn
	closed   bool
}

func (l *windowsProxyListener) Accept() (net.Conn, error) {
	l.acceptMu.Lock()
	defer l.acceptMu.Unlock()
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil, net.ErrClosed
	}
	l.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		cancel()
		return nil, net.ErrClosed
	}
	l.cancel = cancel
	l.mu.Unlock()
	defer cancel()
	p, err := winio.DialPipeAccessImpLevel(ctx, l.path, pipeClientAccess, winio.PipeImpLevelImpersonation)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		p.Close()
		return nil, net.ErrClosed
	}
	l.pending = p
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		if l.pending == p {
			l.pending = nil
		}
		l.cancel = nil
		l.mu.Unlock()
	}()
	if err = verifySystemPipe(p); err != nil {
		p.Close()
		return nil, err
	}
	if err = writePipePreface(ctx, p); err != nil {
		p.Close()
		return nil, err
	}
	var out response
	if err = readFrame(p, &out); err != nil {
		p.Close()
		return nil, err
	}
	if out.Error != "" {
		p.Close()
		return nil, errors.New(out.Error)
	}
	if len(out.Socket) == 1 && out.Socket[0] == 1 {
		return p, nil
	}
	p.Close()
	return nil, errors.New("invalid device guard accept response")
}
func (l *windowsProxyListener) Close() error {
	l.once.Do(func() {
		close(l.done)
		l.mu.Lock()
		l.closed = true
		if l.cancel != nil {
			l.cancel()
		}
		if l.pending != nil {
			l.pending.Close()
		}
		l.mu.Unlock()
	})
	return nil
}
func (l *windowsProxyListener) Addr() net.Addr { return guardAddr(l.path) }

type guardAddr string

func (a guardAddr) Network() string { return "paperboat-deviceguard" }
func (a guardAddr) String() string  { return string(a) }
