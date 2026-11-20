package derpquic

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/admission"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/quic-go/quic-go"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=keyed-peer-connection-registry
	"tailscale.com/types/key"
)

// MaxConnections bounds the capacity a node's registry lease may grant (one ceiling for the server's
// registry, self-host enrollment and this relay); the lease's CapacityLimit is the node's real size.
const MaxConnections = relaylimits.MaxConnectionsPerNode

// accountShareLocked is how many connections one account may hold on this node: every slot but a
// tenth, so other accounts always find room. Byte budgets per account keep forwarding fair; a fixed
// small count refused the 17th machine of an account (relay capacity exhausted on first contact).
func (s *Server) accountShareLocked() int { return max(1, s.connectionLimit-s.connectionLimit/10) }

// overFairShareLocked refuses an account's new connection once the node is half full and the account already holds
// its equal share (limit / accounts connected, itself included): the 90% cap alone let two busy accounts starve a
// third, and a second account arriving beside one holding 90% found 10%. Nothing is evicted; a node that was lopsided
// evens out as the larger account's connections turn over. An idle node never applies it, so a lone account's 17th
// machine still connects. A replacement of the account's own route (old != nil) takes no new slot and is never refused by it. Prior art: Teleport caps connections per user (role max_connections, lib/srv/authhandlers.go:1056), not per node alone.
func (s *Server) overFairShareLocked(account string, count int) bool {
	if len(s.peers) < s.connectionLimit/2 {
		return false
	}
	active := map[string]struct{}{account: {}}
	for _, q := range s.peers {
		active[q.grant.AccountID] = struct{}{}
	}
	return count >= max(1, s.connectionLimit/len(active))
}

type Stats struct {
	Accepted, Denied, Forwarded, Dropped uint64
	// BytesForwarded is the payload bytes of every forwarded packet.
	BytesForwarded uint64
	Connections    int
	Draining       bool
}
type Server struct {
	connectionLimit                                      int
	service                                              *localService
	verifier                                             Verifier // Keys unused: the trusted set is keys
	keys                                                 atomic.Pointer[map[string]ed25519.PublicKey]
	refresher                                            atomic.Pointer[func() error]
	keysRefreshedAt                                      atomic.Int64
	mu                                                   sync.Mutex
	peers                                                map[key.NodePublic]*peerConn
	fences                                               map[string]*grantFence
	fenceSweptAt                                         time.Time
	handover                                             map[string]time.Time
	handoverTimer                                        *time.Timer
	accounts                                             map[string]*budget
	accountRate                                          float64            // bytes per second an account may use here
	accountSpeeds                                        map[string]float64 // accounts held to less, in bytes per second
	connections                                          map[*quic.Conn]struct{}
	wssConnections                                       map[*peerConn]struct{}
	draining                                             bool
	drainTimer                                           *time.Timer
	deadline                                             time.Time
	listener                                             *quic.Listener
	transport                                            *quic.Transport
	workers                                              sync.WaitGroup
	accepted, denied, forwarded, dropped, bytesForwarded atomic.Uint64
	lastRefusalLog                                       atomic.Int64
	mesh                                                 *Mesh // nil: this relay is not linked to others (SetMesh)
}
type queued struct {
	packet
	source  *peerConn
	service bool
	lease   *PairLease
	// remote is the verified grant of a sender connected to another relay (the packet arrived over the mesh); source is nil then.
	remote *remoteSource
}
type peerConn struct {
	conn   *quic.Conn
	stream interface {
		io.Reader
		io.Writer
		SetReadDeadline(time.Time) error
		SetWriteDeadline(time.Time) error
	}
	ctx             context.Context
	cancel          context.CancelFunc
	closeFn         func(uint64, string)
	tlsState        tls.ConnectionState
	key             key.NodePublic
	namespace       [32]byte
	generationDisco key.DiscoPublic
	grant           Grant
	out             chan queued
	control         chan []byte
	rate            *budget
	sequence        atomic.Uint64
	// paired remembers the last peer this connection's grant was checked against, both signed grants at the
	// time and the answer, so a run of datagrams to one peer, paired or not, costs one check (guarded by Server.mu).
	paired pairMemo
	// tokens are the signed grant tokens this carrier authenticated with (of tokenGeneration), kept to answer a mesh
	// peer's NeedGrant; guarded by Server.mu.
	tokens          []string
	tokenGeneration uint64
}

type pairMemo struct {
	peer         *peerConn
	mine, theirs [sha256.Size]byte
	allowed      bool
}

func quicConfig() *quic.Config {
	return &quic.Config{EnableDatagrams: true, HandshakeIdleTimeout: 5 * time.Second, MaxIdleTimeout: 45 * time.Second, KeepAlivePeriod: 15 * time.Second, MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, InitialStreamReceiveWindow: MaxControl + 5, MaxStreamReceiveWindow: MaxControl + 5, InitialConnectionReceiveWindow: 2 * MaxControl, MaxConnectionReceiveWindow: 2 * MaxControl}
}
func NewServer(v Verifier) (*Server, error) {
	if v.Issuer == "" || v.NodeID == "" || v.NodeGeneration == 0 || v.ProcessEpoch == "" || len(v.Keys) == 0 {
		return nil, ErrAdmission
	}
	keys, err := copyKeys(v.Keys)
	if err != nil {
		return nil, err
	}
	v.Keys = nil
	s := &Server{connectionLimit: MaxConnections, verifier: v, peers: make(map[key.NodePublic]*peerConn), fences: make(map[string]*grantFence), accounts: make(map[string]*budget), accountRate: float64(accountBytesPerSecond), connections: make(map[*quic.Conn]struct{}), wssConnections: make(map[*peerConn]struct{})}
	s.keys.Store(&keys)
	return s, nil
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
	if s.mesh != nil {
		// A relay of the mesh dials with the mesh ALPN: it gets the mesh TLS (client certificate pinned to the lease's
		// directory); every other client keeps the derp-v1 configuration above, which requires a grant-bound certificate.
		tlsConfig.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			if slices.Contains(hello.SupportedProtos, nodemesh.RelayMeshALPN) {
				return s.mesh.serverTLS, nil
			}
			return nil, nil
		}
	}
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
	if s.mesh != nil {
		s.mesh.start(ctx)
	}
	for {
		c, e := ln.Accept(ctx)
		if e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e
		}
		if s.mesh != nil && c.ConnectionState().TLS.NegotiatedProtocol == nodemesh.RelayMeshALPN {
			// A mesh link is another relay, not a client: it is neither a connection of this node's capacity nor refused by it.
			s.mu.Lock()
			s.workers.Add(1)
			s.mu.Unlock()
			go func() { defer s.workers.Done(); s.mesh.accept(c) }()
			continue
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
				s.mesh.announce(meshGone, k)
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
				s.mesh.announce(meshGone, k)
			}
		}
		s.mu.Unlock()
	}()
	stream.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, b, err := readFrame(stream)
	if err != nil || kind != frameAuth && kind != frameAuthGeneration && kind != frameAuthBatch {
		s.reject(p, ErrProtocol)
		return
	}
	if err = s.authenticate(p, kind, b); err != nil {
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
		now := time.Now()
		if !p.rate.allow(now, len(b)) || accountBudget == nil {
			s.reject(p, ErrOverload)
			return
		}
		// Payload over the account's fair-use speed waits for its tokens before the next frame is read, so the sender's TCP slows to
		// the speed (nothing is dropped); control frames are bounded by the carrier's own budget only, so a saturated account can
		// still ping and re-authenticate.
		if kind == framePacket {
			if delay, _ := accountBudget.reserve(now, len(b), time.Hour); delay > 0 {
				select {
				case <-time.After(delay):
				case <-p.context().Done():
					return
				}
			}
		}
		switch kind {
		case frameAuth, frameAuthGeneration, frameAuthBatch:
			if err = s.authenticate(p, kind, b); errors.Is(err, ErrExpired) {
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
	// A refusal is never silent: one bounded line per second names its typed code.
	if now := time.Now().UnixNano(); now-s.lastRefusalLog.Load() >= int64(time.Second) && s.lastRefusalLog.CompareAndSwap(s.lastRefusalLog.Load(), now) {
		slog.Info("relay admission refused", "code", e.Code, "reason", err.Error(), "refused_total", s.denied.Load())
	}
	p.close(e.Code, e.Message)
}

// fenceSweepEvery is how often an admission sweeps the expired grant fences.
const fenceSweepEvery = time.Second

func (s *Server) admit(p *peerConn, g Grant, now time.Time) error {
	var err error
	k, _ := ParseKey(g.WireGuardPublicKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	// An endpoint with more peers than one grant names sends one grant per chunk of peers, all of the
	// same generation; on its admitted connection they add up.
	// The part is merged at the commit below, after every refusal, so a refused part changes nothing.
	merging := !p.key.IsZero() && p.grant.Generation == g.Generation && p.grant.signedHash != g.signedHash && sameGrantIdentity(p.grant, g)
	if s.draining && (p.key.IsZero() || !now.Before(s.deadline)) {
		return ErrOverload
	}
	if s.handedOver(g.AccountID, now) {
		return ErrOverload
	}
	id := g.AccountID + "\x00" + g.EndpointID
	// A fence outlives its grant by an hour, so the sweep need not run on every admission (it walked every fence each time).
	if now.Sub(s.fenceSweptAt) >= fenceSweepEvery {
		s.fenceSweptAt = now
		var live map[liveGrantKey]struct{}
		for id, old := range s.fences {
			if old.retainUntil > now.Unix() {
				continue
			}
			if live == nil {
				live = s.liveGrantsLocked()
			}
			if !liveIn(live, old.Grant) {
				delete(s.fences, id)
			}
		}
	}
	// An admitted grant's fence refuses older grants and a conflicting grant of its generation; a
	// revocation's fence (no signed grant) holds min_generation and refuses only below it. A reissue of
	// the same authority (a staged carrier generation refreshing its own copy) is not a conflict.
	if previous, ok := s.fences[id]; ok && (g.Generation < previous.Generation || !merging && previous.AccountID != "" && g.Generation == previous.Generation && g.signedHash != previous.signedHash && !sameGrantAuthority(previous.Grant, g)) {
		return fmt.Errorf("fence (generation %d, held %d): %w", g.Generation, previous.Generation, ErrAdmission)
	}
	if !p.key.IsZero() && (p.key != k || p.grant.AccountID != g.AccountID || p.grant.EndpointID != g.EndpointID) {
		return fmt.Errorf("connection identity changed: %w", ErrAdmission)
	}
	count := 0
	for _, q := range s.peers {
		if q.grant.AccountID == g.AccountID && q != p {
			count++
		}
	}
	old := s.peers[routeKey(k, p.namespace)]
	if old != nil && old != p {
		if old.grant.AccountID != g.AccountID || old.grant.EndpointID != g.EndpointID || g.Generation < old.grant.Generation {
			return fmt.Errorf("route owned by another grant: %w", ErrAdmission)
		}
		count--
	}
	if p.key.IsZero() && (count >= s.accountShareLocked() || old == nil && s.overFairShareLocked(g.AccountID, count) || len(s.fences) >= MaxConnections && s.fences[id] == nil) {
		return ErrOverload
	}
	if old != nil && old != p {
		code := uint64(1)
		if sameGrantAuthority(old.grant, g) {
			code = 0
		}
		old.close(code, "relay registration superseded")
	}
	if merging {
		if g, err = mergeGrants(p.grant, g); err != nil {
			return err
		}
	}
	if p.key.IsZero() {
		s.accepted.Add(1)
	}
	p.key = k
	p.grant = g
	s.peers[routeKey(k, p.namespace)] = p
	if old == nil {
		s.mesh.announce(meshPresent, routeKey(k, p.namespace))
	}
	// A merged part only adds peers to the admitted grant: its fence (and the peer-relay leases that hold
	// it) stays. Comparing the whole authority costs every peer the endpoint names, on every part.
	if previous := s.fences[id]; previous != nil && (merging || sameGrantAuthority(previous.Grant, g)) {
		previous.Grant = g
		previous.retainUntil = max(previous.retainUntil, now.Add(MaxGrantLifetime+MaxClockSkew).Unix())
	} else {
		next := newGrantFence(g, now)
		if previous := s.fences[id]; previous != nil {
			next.retainUntil = max(next.retainUntil, previous.retainUntil)
		}
		s.fences[id] = next
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
		s.accounts[g.AccountID] = newAccountBudget(s.rateForLocked(g.AccountID))
	}
	return nil
}
func (s *Server) authorizedLocked(p, q *peerConn, now time.Time) bool {
	if p == nil || q == nil || p.namespace != q.namespace || s.peers[routeKey(p.key, p.namespace)] != p || s.peers[routeKey(q.key, q.namespace)] != q || s.draining && !now.Before(s.deadline) {
		return false
	}
	// Any new, merged or revoked grant changes a signed hash or the connection, so the memo cannot outlive
	// the authority it was computed from.
	if p.paired.peer == q && p.paired.mine == p.grant.signedHash && p.paired.theirs == q.grant.signedHash {
		return p.paired.allowed
	}
	p.paired = pairMemo{peer: q, mine: p.grant.signedHash, theirs: q.grant.signedHash, allowed: allowedAdmitted(p.grant, q.grant)}
	return p.paired.allowed
}
func (s *Server) route(p *peerConn, msg packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := routeKey(msg.peer, p.namespace)
	q := s.peers[dst]
	if q == nil && s.forwardLocked(p, dst, msg) {
		return
	}
	authorized := s.authorizedLocked(p, q, time.Now())
	if msg.control {
		authorized = authorized || p != nil && q != nil && s.peers[routeKey(p.key, p.namespace)] == p && s.peers[routeKey(q.key, q.namespace)] == q && controlAdmitted(p.grant, q.grant)
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

// readDatagrams receives a carrier's datagrams. Over the account's fair-use speed they are shaped, not dropped: each waits in the
// carrier's queue (shapingMaxDelay at most; beyond it the buffer is full and the datagram is dropped) and is forwarded, in order, when
// its tokens are there, so the account's own transport settles at its speed. One goroutine owns the reassembler: inline while the queue
// is idle, the shaper goroutine while it is not.
func (s *Server) readDatagrams(p *peerConn) {
	defer p.close(0, "relay closed")
	var r reassembler
	forward := func(b []byte, now time.Time) bool {
		msg, ok, e := r.accept(b, now, func(k key.NodePublic) bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.authorizedLocked(p, s.peers[routeKey(k, p.namespace)], now)
		})
		if e != nil {
			s.reject(p, e)
			return false
		}
		if ok {
			s.route(p, msg)
		}
		return true
	}
	type shaped struct {
		b  []byte
		at time.Time
	}
	queue := make(chan shaped, 4096)
	var waiting atomic.Int32 // datagrams queued or being forwarded by the shaper goroutine
	// The shaper goroutine starts with the first datagram that has to wait: an idle carrier costs no goroutine.
	startShaper := sync.OnceFunc(func() {
		go func() {
			timer := time.NewTimer(time.Hour)
			defer timer.Stop()
			for {
				select {
				case <-p.context().Done():
					return
				case item := <-queue:
					if wait := time.Until(item.at); wait > 0 {
						timer.Reset(wait)
						select {
						case <-timer.C:
						case <-p.context().Done():
							return
						}
					}
					ok := forward(item.b, time.Now())
					waiting.Add(-1)
					if !ok {
						return
					}
				}
			}
		}()
	})
	for {
		b, err := p.conn.ReceiveDatagram(p.context())
		if err != nil {
			return
		}
		now := time.Now()
		s.mu.Lock()
		account := s.accounts[p.grant.AccountID]
		s.mu.Unlock()
		if !p.rate.allow(now, len(b)) || account == nil {
			s.dropped.Add(1)
			continue
		}
		delay, ok := account.reserve(now, len(b), shapingMaxDelay)
		if !ok {
			s.dropped.Add(1)
			continue
		}
		if delay == 0 && waiting.Load() == 0 {
			if !forward(b, now) {
				return
			}
			continue
		}
		startShaper()
		waiting.Add(1)
		select {
		case queue <- shaped{b, now.Add(delay)}:
		default:
			waiting.Add(-1)
			s.dropped.Add(1)
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
			now := time.Now()
			valid := msg.remote == nil && s.authorizedLocked(msg.source, p, now) || msg.remote != nil && s.authorizedRemoteLocked(msg.remote, p, msg.control, now)
			if msg.control && !msg.service {
				valid = valid || msg.source != nil && s.peers[routeKey(msg.source.key, msg.source.namespace)] == msg.source && s.peers[routeKey(p.key, p.namespace)] == p && controlAdmitted(msg.source.grant, p.grant)
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
			if !msg.control { // control frames are protocol, not payload
				s.bytesForwarded.Add(uint64(len(msg.packet.data)))
			}
		}
	}
}

// SetAccountSpeed sets the speed one account may use on this relay, shared by all its carriers (a token bucket per account:
// Tailscale's DERP applies one per client the same way, derpserver.go rateLimit). Traffic above it is delayed (a bounded queue for datagrams,
// a wait before the next frame for streams), so the account's own transport settles at its speed. 0 lifts the cap to the relay's own overload ceiling (a customer-owned relay is
// never limited by Paperboat).
func (s *Server) SetAccountSpeed(mbit float64) error {
	if mbit < 0 || mbit > MaxAccountMbit {
		return ErrProtocol
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountRate = speedBytes(mbit)
	s.applyAccountRatesLocked()
	return nil
}

// SetAccountSpeeds replaces the accounts held to less than the normal speed (each with its Mbit/s here: the plan's slower speed once
// the account carried the plan's included relay bytes this month, and its speed divided among the relays that carry it, so an account's
// speed is one bucket across relays); an empty set returns every account to the normal speed.
func (s *Server) SetAccountSpeeds(accounts map[string]float64) error {
	if len(accounts) > MaxConnections {
		return ErrProtocol
	}
	slow := make(map[string]float64, len(accounts))
	for account, mbit := range accounts {
		if account == "" || len(account) > 256 || mbit <= 0 || mbit > MaxAccountMbit {
			return ErrProtocol
		}
		slow[account] = speedBytes(mbit)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accountSpeeds = slow
	s.applyAccountRatesLocked()
	return nil
}

// TakeAccountDemand returns, for every account with traffic admitted on this relay, the bytes it offered since the last call (counted
// before the speed limit decides, so delayed and dropped bytes count: a relay held at its share still shows it wants more) and starts
// a new window. The control plane divides the account's speed among its relays by it.
func (s *Server) TakeAccountDemand() map[string]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]uint64, len(s.accounts))
	for account, b := range s.accounts {
		out[account] = b.takeOffered()
	}
	return out
}

// AccountMbit is the speed in Mbit/s the account is held to on this relay now (the ceiling when it is not limited).
func (s *Server) AccountMbit(account string) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rateForLocked(account) * 8 / 1e6
}

// speedBytes is a cap in Mbit/s as bytes per second, 0 meaning the ceiling.
func speedBytes(mbit float64) float64 {
	if mbit <= 0 {
		return float64(accountBytesPerSecond)
	}
	return min(float64(accountBytesPerSecond), mbit*1e6/8)
}

func (s *Server) rateForLocked(account string) float64 {
	if slow, ok := s.accountSpeeds[account]; ok {
		return min(s.accountRate, slow)
	}
	return s.accountRate
}

func (s *Server) applyAccountRatesLocked() {
	for account, b := range s.accounts {
		rate := s.rateForLocked(account)
		b.setRate(rate, accountBurst(rate))
	}
}

// ValidRevocation is the shape Revoke accepts: both ids present and bounded, and a first valid
// generation of at least 2 (generation 1 was never revoked).
func ValidRevocation(account, endpoint string, minGeneration uint64) bool {
	return account != "" && endpoint != "" && len(account) <= 256 && len(endpoint) <= 256 && minGeneration >= 2
}

// Revoke fences an endpoint's grants below minGeneration, the first generation still valid: it
// closes their connections now, and only a grant of minGeneration or later is admitted again.
func (s *Server) Revoke(account, endpoint string, minGeneration uint64) error {
	if !ValidRevocation(account, endpoint, minGeneration) {
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
	if minGeneration <= g.Generation {
		return nil
	}
	if g.Generation == 0 && len(s.fences) >= MaxConnections {
		return ErrOverload
	}
	g.Generation = minGeneration
	// A revoked grant stays valid on this relay until its exp, up to MaxGrantLifetime after issue on a
	// clock up to MaxClockSkew behind the issuer's, so the fence outlives every such grant.
	g.ExpiresAt = time.Now().Add(MaxGrantLifetime + MaxClockSkew).Unix()
	s.fences[id] = newGrantFence(Grant{RelayGrant: peernetwork.RelayGrant{Generation: g.Generation, ExpiresAt: g.ExpiresAt}}, time.Now())
	for k, p := range s.peers {
		if p.grant.AccountID == account && p.grant.EndpointID == endpoint {
			delete(s.peers, k)
			s.mesh.announce(meshGone, k)
			p.close(1, "relay authority revoked")
		}
	}
	return nil
}
func (s *Server) Drain(deadline time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining || deadline.Before(s.deadline) {
		s.deadline = deadline
	}
	s.draining = true
	// One timer for every connection, not a poll per connection under this lock (PLAN 3b R3). Revocation and
	// supersede close their connections where they happen (Revoke, admit).
	if s.drainTimer != nil {
		s.drainTimer.Stop()
	}
	s.drainTimer = time.AfterFunc(time.Until(s.deadline), s.closeDrained)
}

// closeDrained ends every admitted connection once the drain deadline has passed.
func (s *Server) closeDrained() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		return
	}
	if wait := time.Until(s.deadline); wait > 0 {
		s.drainTimer = time.AfterFunc(wait, s.closeDrained)
		return
	}
	for _, p := range s.peers {
		s.reject(p, ErrOverload)
	}
}
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{s.accepted.Load(), s.denied.Load(), s.forwarded.Load(), s.dropped.Load(), s.bytesForwarded.Load(), len(s.connections) + len(s.wssConnections), s.draining}
}
func (s *Server) Close() error {
	s.mu.Lock()
	s.draining = true
	s.deadline = time.Now()
	if s.drainTimer != nil {
		s.drainTimer.Stop()
	}
	if s.handoverTimer != nil {
		s.handoverTimer.Stop()
	}
	ln, tr, mesh := s.listener, s.transport, s.mesh
	for c := range s.connections {
		c.CloseWithError(0, "relay stopped")
	}
	for p := range s.wssConnections {
		p.close(0, "relay stopped")
	}
	s.mu.Unlock()
	if mesh != nil {
		mesh.close()
	}
	if ln != nil {
		ln.Close()
	}
	if tr != nil {
		tr.Close()
	}
	return nil
}
func (s *Server) Wait() {
	s.workers.Wait()
	if s.mesh != nil {
		s.mesh.wait()
	}
}

func (p *peerConn) Close() error { p.close(ErrAdmission.Code, "relay authority revoked"); return nil }
