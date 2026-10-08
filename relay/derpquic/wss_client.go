package derpquic

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"tailscale.com/derp"
	"tailscale.com/net/wsconn"
	"tailscale.com/types/key"
)

type WSSClientConfig struct {
	URL        string
	TLS        *tls.Config
	Credential func(context.Context) (string, error)
	Proxy      func(*http.Request) (*url.URL, error)
}

type WSSClient struct {
	config      WSSClientConfig
	ctx         context.Context
	cancel      context.CancelFunc
	connectGate chan struct{}
	mu          sync.Mutex
	current     *wssClientConn
	observed    atomic.Pointer[wssClientConn]
	generation  int
	fatal       error
}

type wssClientConn struct {
	ctx        context.Context
	cancel     context.CancelFunc
	ws         *websocket.Conn
	conn       net.Conn
	receive    chan packet
	generation int
	writeMu    sync.Mutex
	pingMu     sync.Mutex
	pings      map[[8]byte]chan struct{}
	workers    sync.WaitGroup
	errMu      sync.Mutex
	err        error
}

func NewWSSClient(config WSSClientConfig) *WSSClient {
	ctx, cancel := context.WithCancel(context.Background())
	return &WSSClient{config: config, ctx: ctx, cancel: cancel, connectGate: make(chan struct{}, 1)}
}

func classifyWSS(err error) error {
	if err == nil {
		return nil
	}
	switch websocket.CloseStatus(err) {
	case 4001:
		return retainDecision(ErrAdmission, err)
	case 4002:
		return retainDecision(ErrProtocol, err)
	case 4003:
		return retainDecision(ErrOverload, err)
	case 4004:
		return retainDecision(ErrExpired, err)
	}
	var cert *tls.CertificateVerificationError
	if errors.As(err, &cert) {
		return retainDecision(ErrAdmission, err)
	}
	return err
}

func (c *WSSClient) ensure(ctx context.Context) (*wssClientConn, error) {
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
		if old.ctx.Err() == nil {
			return old, nil
		}
		old.workers.Wait()
		old.errMu.Lock()
		ended := old.err
		old.errMu.Unlock()
		if fatalCarrier(ended) {
			c.fatal = ended
			return nil, ended
		}
		c.current = nil
		c.observed.Store(nil)
	}
	if c.config.TLS == nil || c.config.TLS.InsecureSkipVerify && c.config.TLS.VerifyConnection == nil && c.config.TLS.VerifyPeerCertificate == nil || len(c.config.TLS.Certificates) != 1 || c.config.Credential == nil || c.config.URL == "" {
		c.fatal = ErrAdmission
		return nil, c.fatal
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	token, err := c.config.Credential(dialCtx)
	if err != nil || token == "" || len(token) > MaxControl {
		if dialCtx.Err() != nil {
			return nil, dialCtx.Err()
		}
		if errors.Is(err, ErrExpired) {
			return nil, retainDecision(ErrExpired, err)
		}
		c.fatal = retainDecision(ErrAdmission, err)
		return nil, c.fatal
	}
	proxy := c.config.Proxy
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	transport := &http.Transport{Proxy: proxy, TLSClientConfig: c.config.TLS.Clone(), ForceAttemptHTTP2: false}
	defer transport.CloseIdleConnections()
	ws, response, err := websocket.Dial(dialCtx, c.config.URL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}, Subprotocols: []string{WSSSubprotocol}, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			c.fatal = retainDecision(ErrAdmission, err)
			return nil, c.fatal
		}
		return nil, classifyWSS(err)
	}
	if ws.Subprotocol() != WSSSubprotocol {
		_ = ws.Close(websocket.StatusPolicyViolation, "relay protocol mismatch")
		c.fatal = ErrProtocol
		return nil, c.fatal
	}
	connCtx, connCancel := context.WithCancel(c.ctx)
	ws.SetReadLimit(MaxControl + 5)
	nc := wsconn.NetConn(connCtx, ws, websocket.MessageBinary, c.config.URL)
	fail := func(err error) (*wssClientConn, error) {
		connCancel()
		_ = ws.Close(websocket.StatusInternalError, "relay setup failed")
		err = classifyWSS(err)
		var fatal interface{ Fatal() bool }
		if errors.As(err, &fatal) && fatal.Fatal() {
			c.fatal = err
		}
		return nil, err
	}
	_ = nc.SetDeadline(time.Now().Add(5 * time.Second))
	if err = writeFrame(nc, frameAuth, []byte(token)); err != nil {
		return fail(err)
	}
	kind, b, err := readFrame(nc)
	if err != nil {
		return fail(err)
	}
	if kind != frameReady || len(b) != 0 {
		return fail(ErrProtocol)
	}
	_ = nc.SetDeadline(time.Time{})
	c.generation++
	cc := &wssClientConn{ctx: connCtx, cancel: connCancel, ws: ws, conn: nc, receive: make(chan packet, queueDepth), generation: c.generation, pings: make(map[[8]byte]chan struct{})}
	c.current = cc
	c.observed.Store(cc)
	cc.workers.Add(2)
	go c.read(cc)
	go c.refresh(cc)
	return cc, nil
}

func (c *WSSClient) Connect(ctx context.Context) error { _, err := c.ensure(ctx); return err }
func (c *WSSClient) Disconnect() error {
	if err := acquireConnect(context.Background(), c.ctx, c.connectGate); err != nil {
		return err
	}
	defer releaseConnect(c.connectGate)
	return c.disconnect()
}

func (c *WSSClient) disconnect() error {
	c.observed.Store(nil)
	c.mu.Lock()
	cc := c.current
	c.current = nil
	c.mu.Unlock()
	if cc == nil {
		return nil
	}
	cc.cancel()
	err := cc.ws.Close(websocket.StatusNormalClosure, "relay disconnected")
	cc.workers.Wait()
	return err
}

func (c *WSSClient) Close() error {
	c.cancel()
	c.connectGate <- struct{}{}
	defer releaseConnect(c.connectGate)
	return c.disconnect()
}
func (cc *wssClientConn) frame(kind byte, b []byte) error {
	cc.writeMu.Lock()
	defer cc.writeMu.Unlock()
	_ = cc.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return classifyWSS(writeFrame(cc.conn, kind, b))
}
func (c *WSSClient) Send(k key.NodePublic, b []byte) error {
	if len(b) == 0 || len(b) > MaxPacket {
		return ErrProtocol
	}
	cc, err := c.ensure(c.ctx)
	if err != nil {
		return err
	}
	return cc.frame(framePacket, encodeControl(packet{peer: k, data: b}))
}
func (c *WSSClient) SendControl(k key.NodePublic, b []byte) error {
	if len(b) == 0 || len(b) > MaxPacket {
		return ErrProtocol
	}
	cc, err := c.ensure(c.ctx)
	if err != nil {
		return err
	}
	return cc.frame(frameControl, encodeControl(packet{peer: k, data: b, control: true}))
}
func (c *WSSClient) RecvDetail() (derp.ReceivedMessage, int, error) {
	cc, err := c.ensure(c.ctx)
	if err != nil {
		return nil, 0, err
	}
	select {
	case <-c.ctx.Done():
		return nil, cc.generation, ErrClosed
	case <-cc.ctx.Done():
		cc.errMu.Lock()
		ended := cc.err
		cc.errMu.Unlock()
		if ended == nil {
			ended = classifyWSS(context.Cause(cc.ctx))
		}
		if fatalCarrier(ended) {
			c.mu.Lock()
			if c.fatal == nil {
				c.fatal = ended
			}
			c.mu.Unlock()
		}
		return nil, cc.generation, ended
	case p := <-cc.receive:
		return derp.ReceivedPacket{Source: p.peer, Data: p.data}, cc.generation, nil
	}
}
func (*WSSClient) NotePreferred(bool) {}
func (c *WSSClient) SendPong(data [8]byte) error {
	cc, e := c.ensure(c.ctx)
	if e != nil {
		return e
	}
	return cc.frame(framePong, data[:])
}
func (c *WSSClient) LocalAddr() (netip.AddrPort, error) {
	cc := c.observed.Load()
	if cc == nil || cc.ctx.Err() != nil {
		return netip.AddrPort{}, ErrClosed
	}
	return netip.AddrPort{}, nil
}
func (c *WSSClient) Ping(ctx context.Context) error {
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
	case <-cc.ctx.Done():
		return classifyWSS(context.Cause(cc.ctx))
	case <-done:
		return nil
	}
}
func (c *WSSClient) read(cc *wssClientConn) {
	defer cc.workers.Done()
	var final error
	defer func() { cc.errMu.Lock(); cc.err = classifyWSS(final); cc.errMu.Unlock(); cc.cancel() }()
	for {
		kind, b, e := readFrame(cc.conn)
		if e != nil {
			final = e
			return
		}
		switch kind {
		case frameControl, framePacket:
			p, e := decodeControl(b)
			if e != nil {
				final = ErrProtocol
				return
			}
			p.control = kind == frameControl
			select {
			case cc.receive <- p:
			case <-cc.ctx.Done():
				return
			}
		case framePong:
			if len(b) != 8 {
				final = ErrProtocol
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
			final = ErrProtocol
			return
		}
	}
}
func (c *WSSClient) refresh(cc *wssClientConn) {
	defer cc.workers.Done()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-cc.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(cc.ctx, 5*time.Second)
			token, e := c.config.Credential(ctx)
			cancel()
			if e != nil || token == "" || cc.frame(frameAuth, []byte(token)) != nil {
				cc.cancel()
				return
			}
		}
	}
}
