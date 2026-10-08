package derpquic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"tailscale.com/derp"
	"tailscale.com/types/key"
)

type ClientConfig struct {
	Address    string
	TLS        *tls.Config
	Credential func(context.Context) (string, error)
}
type Client struct {
	config      ClientConfig
	ctx         context.Context
	cancel      context.CancelFunc
	connectGate chan struct{}
	mu          sync.Mutex
	current     *clientConn
	observed    atomic.Pointer[clientConn]
	generation  int
	fatal       error
}
type clientConn struct {
	conn       *quic.Conn
	stream     *quic.Stream
	writeMu    sync.Mutex
	receive    chan packet
	sequence   atomic.Uint64
	generation int
	pingMu     sync.Mutex
	pings      map[[8]byte]chan struct{}
	workers    sync.WaitGroup
}

func NewClient(config ClientConfig) *Client {
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{config: config, ctx: ctx, cancel: cancel, connectGate: make(chan struct{}, 1)}
}
func classify(err error) error {
	var app *quic.ApplicationError
	if errors.As(err, &app) {
		switch app.ErrorCode {
		case 1:
			return ErrAdmission
		case 2:
			return ErrProtocol
		case 3:
			return ErrOverload
		case 4:
			return ErrExpired
		}
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return ErrAdmission
	}
	return err
}
func (c *Client) ensure(ctx context.Context) (*clientConn, error) {
	if err := acquireConnect(ctx, c.ctx, c.connectGate); err != nil {
		return nil, err
	}
	defer releaseConnect(c.connectGate)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return nil, ErrClosed
	}
	if c.fatal != nil {
		return nil, c.fatal
	}
	if old := c.current; old != nil {
		if old.conn.Context().Err() == nil {
			return old, nil
		}
		err := classify(context.Cause(old.conn.Context()))
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			c.fatal = err
			return nil, err
		}
		old.workers.Wait()
		c.current = nil
		c.observed.Store(nil)
	}
	if c.config.TLS == nil || c.config.TLS.InsecureSkipVerify && c.config.TLS.VerifyConnection == nil && c.config.TLS.VerifyPeerCertificate == nil || len(c.config.TLS.Certificates) != 1 || c.config.Credential == nil || c.config.Address == "" {
		c.fatal = ErrAdmission
		return nil, c.fatal
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	token, err := c.config.Credential(dialCtx)
	if err != nil {
		if dialCtx.Err() != nil {
			return nil, dialCtx.Err()
		}
		if errors.Is(err, ErrExpired) {
			return nil, ErrExpired
		}
		c.fatal = ErrAdmission
		return nil, c.fatal
	}
	if token == "" || len(token) > MaxControl {
		return nil, ErrAdmission
	}
	config := c.config.TLS.Clone()
	config.NextProtos = []string{ALPN}
	config.MinVersion = tls.VersionTLS13
	conn, err := quic.DialAddr(dialCtx, c.config.Address, config, quicConfig())
	if err != nil {
		err = classify(err)
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			c.fatal = err
		}
		return nil, err
	}
	fail := func(err error) (*clientConn, error) {
		conn.CloseWithError(0, "relay setup failed")
		err = classify(err)
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			c.fatal = err
		}
		return nil, err
	}
	stream, err := conn.OpenStreamSync(dialCtx)
	if err != nil {
		return fail(err)
	}
	stream.SetDeadline(time.Now().Add(5 * time.Second))
	if err = writeFrame(stream, frameAuth, []byte(token)); err != nil {
		return fail(err)
	}
	kind, b, err := readFrame(stream)
	if err != nil {
		return fail(err)
	}
	if kind != frameReady || len(b) != 0 {
		return fail(ErrProtocol)
	}
	stream.SetDeadline(time.Time{})
	c.generation++
	cc := &clientConn{conn: conn, stream: stream, receive: make(chan packet, queueDepth), generation: c.generation, pings: make(map[[8]byte]chan struct{})}
	c.current = cc
	c.observed.Store(cc)
	cc.workers.Add(3)
	go c.readControl(cc)
	go c.readData(cc)
	go c.refresh(cc)
	return cc, nil
}
func (c *Client) Connect(ctx context.Context) error { _, err := c.ensure(ctx); return err }
func (c *Client) Disconnect() error {
	if err := acquireConnect(context.Background(), c.ctx, c.connectGate); err != nil {
		return err
	}
	defer releaseConnect(c.connectGate)
	return c.disconnect()
}

func (c *Client) disconnect() error {
	c.observed.Store(nil)
	c.mu.Lock()
	cc := c.current
	c.current = nil
	c.mu.Unlock()
	if cc == nil {
		return nil
	}
	err := cc.conn.CloseWithError(0, "relay disconnected")
	cc.workers.Wait()
	return err
}

func (c *Client) Close() error {
	c.cancel()
	c.connectGate <- struct{}{}
	defer releaseConnect(c.connectGate)
	return c.disconnect()
}
func (cc *clientConn) frame(kind byte, b []byte) error {
	cc.writeMu.Lock()
	defer cc.writeMu.Unlock()
	cc.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return cc.streamWrite(kind, b)
}
func (cc *clientConn) streamWrite(kind byte, b []byte) error {
	err := writeFrame(cc.stream, kind, b)
	if err != nil {
		cc.conn.CloseWithError(0, "relay control failed")
	}
	return classify(err)
}
func (c *Client) Send(k key.NodePublic, b []byte) error {
	if len(b) == 0 || len(b) > MaxPacket {
		return ErrProtocol
	}
	cc, e := c.ensure(c.ctx)
	if e != nil {
		return e
	}
	return classify(sendPacket(cc.conn, &cc.sequence, packet{k, b, false}))
}
func (c *Client) SendControl(k key.NodePublic, b []byte) error {
	if len(b) == 0 || len(b) > MaxPacket {
		return ErrProtocol
	}
	cc, e := c.ensure(c.ctx)
	if e != nil {
		return e
	}
	return cc.frame(frameControl, encodeControl(packet{k, b, true}))
}
func (c *Client) RecvDetail() (derp.ReceivedMessage, int, error) {
	cc, e := c.ensure(c.ctx)
	if e != nil {
		return nil, 0, e
	}
	select {
	case <-c.ctx.Done():
		return nil, cc.generation, ErrClosed
	case <-cc.conn.Context().Done():
		if c.ctx.Err() != nil {
			return nil, cc.generation, ErrClosed
		}
		return nil, cc.generation, classify(context.Cause(cc.conn.Context()))
	case p := <-cc.receive:
		if cc.conn.Context().Err() != nil {
			return nil, cc.generation, classify(context.Cause(cc.conn.Context()))
		}
		return derp.ReceivedPacket{Source: p.peer, Data: p.data}, cc.generation, nil
	}
}
func (c *Client) NotePreferred(bool) {} // Each selected regional carrier has one authenticated connection.
func (c *Client) SendPong(data [8]byte) error {
	cc, e := c.ensure(c.ctx)
	if e != nil {
		return e
	}
	return cc.frame(framePong, data[:])
}
func (c *Client) LocalAddr() (netip.AddrPort, error) {
	cc := c.observed.Load()
	if cc == nil || cc.conn.Context().Err() != nil {
		return netip.AddrPort{}, ErrClosed
	}
	return netip.ParseAddrPort(cc.conn.LocalAddr().String())
}
func (c *Client) Ping(ctx context.Context) error {
	cc, e := c.ensure(ctx)
	if e != nil {
		return e
	}
	var id [8]byte
	if _, e = rand.Read(id[:]); e != nil {
		return e
	}
	done := make(chan struct{})
	cc.pingMu.Lock()
	if len(cc.pings) >= 8 {
		cc.pingMu.Unlock()
		return ErrOverload
	}
	cc.pings[id] = done
	cc.pingMu.Unlock()
	defer func() { cc.pingMu.Lock(); delete(cc.pings, id); cc.pingMu.Unlock() }()
	if e = cc.frame(framePing, id[:]); e != nil {
		return e
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-cc.conn.Context().Done():
		return classify(context.Cause(cc.conn.Context()))
	case <-done:
		return nil
	}
}
func (c *Client) readControl(cc *clientConn) {
	defer cc.workers.Done()
	defer cc.conn.CloseWithError(0, "relay control ended")
	for {
		kind, b, e := readFrame(cc.stream)
		if e != nil {
			return
		}
		switch kind {
		case frameControl:
			p, e := decodeControl(b)
			if e != nil {
				cc.conn.CloseWithError(2, "invalid relay control")
				return
			}
			select {
			case cc.receive <- p:
			case <-cc.conn.Context().Done():
				return
			}
		case framePong:
			if len(b) != 8 {
				cc.conn.CloseWithError(2, "invalid relay pong")
				return
			}
			var id [8]byte
			copy(id[:], b)
			cc.pingMu.Lock()
			if done := cc.pings[id]; done != nil {
				close(done)
				delete(cc.pings, id)
			}
			cc.pingMu.Unlock()
		default:
			cc.conn.CloseWithError(2, "invalid relay frame")
			return
		}
	}
}
func (c *Client) readData(cc *clientConn) {
	defer cc.workers.Done()
	defer cc.conn.CloseWithError(0, "relay data ended")
	var r reassembler
	for {
		b, e := cc.conn.ReceiveDatagram(cc.conn.Context())
		if e != nil {
			return
		}
		p, ok, e := r.accept(b, time.Now(), nil)
		if e != nil {
			cc.conn.CloseWithError(2, "invalid relay datagram")
			return
		}
		if ok {
			select {
			case cc.receive <- p:
			default:
			}
		}
	}
}
func (c *Client) refresh(cc *clientConn) {
	defer cc.workers.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-cc.conn.Context().Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(cc.conn.Context(), 5*time.Second)
			token, err := c.config.Credential(ctx)
			cancel()
			if err != nil || token == "" {
				code := ErrAdmission.Code
				if errors.Is(err, ErrExpired) {
					code = ErrExpired.Code
				}
				cc.conn.CloseWithError(quic.ApplicationErrorCode(code), "relay authority unavailable")
				return
			}
			if cc.frame(frameAuth, []byte(token)) != nil {
				return
			}
		}
	}
}
