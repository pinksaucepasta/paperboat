package derpquic

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"github.com/pinksaucepasta/paperboat-relay/admission"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"tailscale.com/types/key"
)

const MaxConnections = 256
const MaxAccountConnections = 16

type Stats struct {
	Accepted, Denied, Forwarded, Dropped uint64
	Connections                          int
	Draining                             bool
}
type Server struct {
	connectionLimit                      int
	service                              *localService
	verifier                             Verifier
	mu                                   sync.Mutex
	peers                                map[key.NodePublic]*peerConn
	fences                               map[string]*grantFence
	quotaDenied                          map[string]bool
	accounts                             map[string]*budget
	connections                          map[*quic.Conn]struct{}
	wssConnections                       map[*peerConn]struct{}
	draining                             bool
	deadline                             time.Time
	listener                             *quic.Listener
	transport                            *quic.Transport
	workers                              sync.WaitGroup
	accepted, denied, forwarded, dropped atomic.Uint64
}
type queued struct {
	packet
	source  *peerConn
	service bool
	lease   *PairLease
}
type peerConn struct {
	conn   *quic.Conn
	stream interface {
		io.Reader
		io.Writer
		SetReadDeadline(time.Time) error
		SetWriteDeadline(time.Time) error
	}
	ctx      context.Context
	cancel   context.CancelFunc
	closeFn  func(uint64, string)
	tlsState tls.ConnectionState
	key      key.NodePublic
	grant    Grant
	out      chan queued
	control  chan []byte
	rate     *budget
	sequence atomic.Uint64
}

func quicConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 45 * time.Second, KeepAlivePeriod: 15 * time.Second, MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: MaxControl + 5, MaxStreamReceiveWindow: MaxControl + 5, InitialConnectionReceiveWindow: 2 * MaxControl, MaxConnectionReceiveWindow: 2 * MaxControl}
}
func NewServer(v Verifier) (*Server, error) {
	if v.Issuer == "" || v.NodeID == "" || v.NodeGeneration == 0 || v.ProcessEpoch == "" || len(v.Keys) == 0 {
		return nil, ErrAdmission
	}
	keys := make(map[string]ed25519.PublicKey, len(v.Keys))
	for id, k := range v.Keys {
		if id == "" || len(k) != 32 {
			return nil, ErrAdmission
		}
		keys[id] = append(ed25519.PublicKey(nil), k...)
	}
	v.Keys = keys
	return &Server{connectionLimit: MaxConnections, verifier: v, peers: make(map[key.NodePublic]*peerConn), fences: make(map[string]*grantFence), accounts: make(map[string]*budget), connections: make(map[*quic.Conn]struct{}), wssConnections: make(map[*peerConn]struct{})}, nil
}

func (p *peerConn) context() context.Context {
	if p.conn != nil {
		return p.conn.Context()
	}
	return p.ctx
}
func (p *peerConn) close(code uint64, reason string) {
	if p.closeFn != nil {
		p.closeFn(code, reason)
		return
	}
	if p.conn != nil {
		_ = p.conn.CloseWithError(quic.ApplicationErrorCode(code), reason)
		return
	}
	if p.cancel != nil {
		p.cancel()
	}
	if c, ok := p.stream.(io.Closer); ok {
		_ = c.Close()
	}
}
func (p *peerConn) state() tls.ConnectionState {
	if p.conn != nil {
		return p.conn.ConnectionState().TLS
	}
	return p.tlsState
}

// Serve owns the QUIC transport, but the caller owns the supplied UDP socket.
func (s *Server) Serve(ctx context.Context, socket net.PacketConn, config *tls.Config) error {
	if ctx == nil || socket == nil || config == nil || len(config.Certificates) == 0 {
		return ErrProtocol
	}
	tlsConfig := config.Clone()
	tlsConfig.NextProtos = []string{ALPN}
	tlsConfig.ClientAuth = tls.RequireAnyClientCert
	tlsConfig.MinVersion = tls.VersionTLS13
	tr := &quic.Transport{Conn: socket, VerifySourceAddress: func(net.Addr) bool { return true }}
	ln, err := tr.Listen(tlsConfig, quicConfig())
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.listener != nil || s.draining {
		s.mu.Unlock()
		ln.Close()
		tr.Close()
		return ErrClosed
	}
	s.listener = ln
	s.transport = tr
	s.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	defer s.Close()
	for {
		c, e := ln.Accept(ctx)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e
		}
		s.mu.Lock()
		if s.draining || len(s.connections)+len(s.wssConnections) >= s.connectionLimit {
			s.mu.Unlock()
			s.denied.Add(1)
			c.CloseWithError(3, "relay unavailable")
			continue
		}
		s.connections[c] = struct{}{}
		s.workers.Add(1)
		s.mu.Unlock()
		go func() { defer s.workers.Done(); s.serveConn(c) }()
	}
}
func (s *Server) serveConn(c *quic.Conn) {
	defer func() {
		c.CloseWithError(0, "relay closed")
		s.mu.Lock()
		delete(s.connections, c)
		for k, p := range s.peers {
			if p.conn == c {
				delete(s.peers, k)
			}
		}
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
	stream, err := c.AcceptStream(ctx)
	cancel()
	if err != nil {
		return
	}
	p := &peerConn{conn: c, stream: stream, rate: newBudget(carrierBytesPerSecond), out: make(chan queued, queueDepth), control: make(chan []byte, 8)}
	s.servePeer(p)
}

func (s *Server) servePeer(p *peerConn) {
	c := p.conn
	stream := p.stream
	defer func() {
		p.close(0, "relay closed")
		s.mu.Lock()
		delete(s.wssConnections, p)
		for k, q := range s.peers {
			if q == p {
				delete(s.peers, k)
			}
		}
		s.mu.Unlock()
	}()
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, b, err := readFrame(stream)
	if err != nil || kind != frameAuth {
		s.reject(p, ErrProtocol)
		return
	}
	if err = s.admit(p, string(b)); err != nil {
		s.reject(p, err)
		return
	}
	stream.SetReadDeadline(time.Time{})
	stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if writeFrame(stream, frameReady, nil) != nil {
		return
	}
	stream.SetWriteDeadline(time.Time{})
	var wg sync.WaitGroup
	wg.Add(1)
	if c != nil {
		wg.Add(1)
		go func() { defer wg.Done(); s.readDatagrams(p) }()
	}
	go func() { defer wg.Done(); s.writePackets(p) }()
	lease, err := admission.Admit(p.context(), time.Unix(p.grant.ExpiresAt, 0), nil)
	if err != nil {
		s.reject(p, ErrExpired)
		return
	}
	defer lease.Close()
	stop := lease.Bind(p)
	defer stop()
	stopWatch := lease.Poll(func(context.Context) error {
		s.mu.Lock()
		current := s.peers[p.key] == p
		drained := s.draining && !time.Now().Before(s.deadline)
		s.mu.Unlock()
		if drained {
			s.reject(p, ErrOverload)
			return admission.ErrRevoked
		}
		if !current {
			return admission.ErrRevoked
		}
		return nil
	})
	defer stopWatch()
	defer func() { p.close(0, "relay closed"); wg.Wait() }()
	for {
		kind, b, err = readFrame(stream)
		if err != nil {
			if errors.Is(err, ErrProtocol) {
				s.reject(p, ErrProtocol)
			}
			return
		}
		s.mu.Lock()
		accountBudget := s.accounts[p.grant.AccountID]
		s.mu.Unlock()
		if now := time.Now(); !p.rate.allow(now, len(b)) || accountBudget == nil || !accountBudget.allow(now, len(b)) {
			s.reject(p, ErrOverload)
			return
		}
		switch kind {
		case frameAuth:
			if err = s.admit(p, string(b)); errors.Is(err, ErrExpired) {
				continue
			} else if err != nil {
				s.reject(p, err)
				return
			}
		case frameControl:
			msg, e := decodeControl(b)
			if e != nil {
				s.reject(p, e)
				return
			}
			if !s.tryControlService(p, msg) {
				s.route(p, msg)
			}
		case framePing:
			if len(b) != 8 {
				s.reject(p, ErrProtocol)
				return
			}
			select {
			case p.control <- append([]byte(nil), b...):
			default:
				s.reject(p, ErrOverload)
				return
			}
		case framePong:
			if len(b) != 8 {
				s.reject(p, ErrProtocol)
				return
			}
		case framePacket:
			if c != nil {
				s.reject(p, ErrProtocol)
				return
			}
			msg, e := decodeControl(b)
			if e != nil {
				s.reject(p, e)
				return
			}
			msg.control = false
			s.route(p, msg)
		default:
			s.reject(p, ErrProtocol)
			return
		}
	}
}
func (s *Server) reject(p *peerConn, err error) {
	s.denied.Add(1)
	var e *Error
	if !errors.As(err, &e) {
		e = ErrAdmission
	}
	p.close(e.Code, e.Message)
}
func (s *Server) admit(p *peerConn, token string) error {
	now := time.Now()
	g, err := s.verifier.Verify(token, p.state(), now)
	if err != nil {
		return err
	}
	k, _ := ParseKey(g.WireGuardPublicKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining && (p.key.IsZero() || !now.Before(s.deadline)) {
		return ErrOverload
	}
	if s.quotaDenied[g.AccountID] {
		return ErrQuota
	}
	id := g.AccountID + "\x00" + g.EndpointID
	for id, old := range s.fences {
		k, _ := ParseKey(old.WireGuardPublicKey)
		live := s.peers[k]
		if old.ExpiresAt <= now.Unix() && (live == nil || grantID(live.grant) != id) {
			delete(s.fences, id)
		}
	}
	if previous, ok := s.fences[id]; ok && (g.Generation < previous.Generation || g.Generation == previous.Generation && g.signedHash != previous.signedHash) {
		return ErrAdmission
	}
	if !p.key.IsZero() && (p.key != k || p.grant.AccountID != g.AccountID || p.grant.EndpointID != g.EndpointID) {
		return ErrAdmission
	}
	count := 0
	for _, q := range s.peers {
		if q.grant.AccountID == g.AccountID && q != p {
			count++
		}
	}
	old := s.peers[k]
	if old != nil && old != p {
		if old.grant.AccountID != g.AccountID || old.grant.EndpointID != g.EndpointID || g.Generation < old.grant.Generation {
			return ErrAdmission
		}
		count--
	}
	if p.key.IsZero() && (count >= MaxAccountConnections || len(s.fences) >= MaxConnections && s.fences[id] == nil) {
		return ErrOverload
	}
	if old != nil && old != p {
		code := uint64(1)
		if sameGrantAuthority(old.grant, g) {
			code = 0
		}
		old.close(code, "relay registration superseded")
	}
	if p.key.IsZero() {
		s.accepted.Add(1)
	}
	p.key = k
	p.grant = g
	s.peers[k] = p
	if previous := s.fences[id]; previous != nil && sameGrantAuthority(previous.Grant, g) {
		previous.Grant = g
	} else {
		s.fences[id] = &grantFence{g}
	}
	if s.accounts[g.AccountID] == nil {
		if len(s.accounts) >= MaxConnections {
			for account := range s.accounts {
				live := false
				for _, q := range s.peers {
					if q.grant.AccountID == account {
						live = true
						break
					}
				}
				if !live {
					delete(s.accounts, account)
				}
			}
		}
		s.accounts[g.AccountID] = newBudget(accountBytesPerSecond)
	}
	return nil
}
func (s *Server) authorizedLocked(p, q *peerConn, now time.Time) bool {
	return p != nil && q != nil && s.peers[p.key] == p && s.peers[q.key] == q && (!s.draining || now.Before(s.deadline)) && allowedAdmitted(p.grant, q.grant)
}
func (s *Server) route(p *peerConn, msg packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.peers[msg.peer]
	authorized := s.authorizedLocked(p, q, time.Now())
	if msg.control {
		authorized = authorized || p != nil && q != nil && s.peers[p.key] == p && s.peers[q.key] == q && controlAdmitted(p.grant, q.grant)
	}
	if !authorized {
		s.dropped.Add(1)
		return
	}
	msg.peer = p.key
	select {
	case q.out <- queued{packet: msg, source: p}:
	default:
		s.dropped.Add(1)
	}
}
func (s *Server) readDatagrams(p *peerConn) {
	defer p.close(0, "relay closed")
	var r reassembler
	for {
		b, err := p.conn.ReceiveDatagram(p.context())
		if err != nil {
			return
		}
		now := time.Now()
		s.mu.Lock()
		account := s.accounts[p.grant.AccountID]
		s.mu.Unlock()
		if !p.rate.allow(now, len(b)) || account == nil || !account.allow(now, len(b)) {
			s.dropped.Add(1)
			continue
		}
		msg, ok, e := r.accept(b, now, func(k key.NodePublic) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.authorizedLocked(p, s.peers[k], now)
		})
		if e != nil {
			s.reject(p, e)
			return
		}
		if ok {
			s.route(p, msg)
		}
	}
}
func (s *Server) writePackets(p *peerConn) {
	defer p.close(0, "relay closed")
	for {
		select {
		case <-p.context().Done():
			return
		case pong := <-p.control:
			p.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if writeFrame(p.stream, framePong, pong) != nil {
				return
			}
		case msg := <-p.out:
			s.mu.Lock()
			valid := s.authorizedLocked(msg.source, p, time.Now())
			if msg.control && !msg.service {
				valid = valid || msg.source != nil && s.peers[msg.source.key] == msg.source && s.peers[p.key] == p && controlAdmitted(msg.source.grant, p.grant)
			}
			if msg.service {
				valid = msg.source == p && s.serviceAllowedLocked(p) && s.service != nil && msg.peer == s.service.key && msg.lease != nil
				if valid {
					_, valid = msg.lease.validLocked()
				}
			}
			s.mu.Unlock()
			if !valid {
				s.dropped.Add(1)
				continue
			}
			var err error
			if msg.control {
				p.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
				err = writeFrame(p.stream, frameControl, encodeControl(msg.packet))
			} else if p.conn != nil {
				err = sendPacket(p.conn, &p.sequence, msg.packet)
			} else {
				p.stream.SetWriteDeadline(time.Now().Add(5 * time.Second))
				err = writeFrame(p.stream, framePacket, encodeControl(msg.packet))
			}
			if err != nil {
				return
			}
			s.forwarded.Add(1)
		}
	}
}

// SetQuotaDenied replaces the set of accounts this relay refuses because the control plane
// reported them out of bandwidth allowance. New admissions and re-authentications of listed
// accounts get ErrQuota, and their live carriers are closed with it now; unlisted accounts
// are unaffected, and an empty set clears every refusal. It fences nothing: a grant stays
// valid and works again as soon as the account is no longer listed.
func (s *Server) SetQuotaDenied(accounts []string) error {
	if len(accounts) > MaxConnections {
		return ErrProtocol
	}
	denied := make(map[string]bool, len(accounts))
	for _, account := range accounts {
		if account == "" || len(account) > 256 {
			return ErrProtocol
		}
		denied[account] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaDenied = denied
	for k, p := range s.peers {
		if denied[p.grant.AccountID] {
			delete(s.peers, k)
			p.close(ErrQuota.Code, ErrQuota.Message)
		}
	}
	return nil
}

// QuotaDenied lists the accounts currently refused for bandwidth, sorted.
func (s *Server) QuotaDenied() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	accounts := make([]string, 0, len(s.quotaDenied))
	for account := range s.quotaDenied {
		accounts = append(accounts, account)
	}
	sort.Strings(accounts)
	return accounts
}

// Revoke fences current routing immediately; signed newer grants are required to reconnect.
func (s *Server) Revoke(account, endpoint string, generation uint64) error {
	if account == "" || endpoint == "" || len(account) > 256 || len(endpoint) > 256 || generation == 0 {
		return ErrAdmission
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := account + "\x00" + endpoint
	previous := s.fences[id]
	var g Grant
	if previous != nil {
		g = previous.Grant
	}
	if generation <= g.Generation {
		return nil
	}
	if g.Generation == 0 && len(s.fences) >= MaxConnections {
		return ErrOverload
	}
	g.Generation = generation
	g.ExpiresAt = time.Now().Add(time.Minute).Unix()
	s.fences[id] = &grantFence{Grant{Generation: g.Generation, ExpiresAt: g.ExpiresAt}}
	for k, p := range s.peers {
		if p.grant.AccountID == account && p.grant.EndpointID == endpoint {
			delete(s.peers, k)
			p.close(1, "relay authority revoked")
		}
	}
	return nil
}
func (s *Server) Drain(deadline time.Time) {
	s.mu.Lock()
	if !s.draining || deadline.Before(s.deadline) {
		s.deadline = deadline
	}
	s.draining = true
	s.mu.Unlock()
}
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{s.accepted.Load(), s.denied.Load(), s.forwarded.Load(), s.dropped.Load(), len(s.connections) + len(s.wssConnections), s.draining}
}
func (s *Server) Close() error {
	s.mu.Lock()
	s.draining = true
	s.deadline = time.Now()
	ln, tr := s.listener, s.transport
	for c := range s.connections {
		c.CloseWithError(0, "relay stopped")
	}
	for p := range s.wssConnections {
		p.close(0, "relay stopped")
	}
	s.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if tr != nil {
		tr.Close()
	}
	return nil
}
func (s *Server) Wait() { s.workers.Wait() }

func (p *peerConn) Close() error { p.close(ErrAdmission.Code, "relay authority revoked"); return nil }
