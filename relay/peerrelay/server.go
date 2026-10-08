// Package peerrelay applies Paperboat pair authority to upstream UDP/Geneve.
package peerrelay

import (
	"context"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"tailscale.com/disco"
	"tailscale.com/net/udprelay"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
	"tailscale.com/util/usermetric"
)

const MaxAllocations = 128
const MaxAccountAllocations = 16

type Config struct {
	Service      derpquic.ServiceDescriptor
	DiscoPrivate key.DiscoPrivate
	Port         uint16
	Addresses    []netip.AddrPort
}
type PairLease interface {
	AccountID() string
	Valid() (time.Time, bool)
}
type ControlRequest interface {
	SourceDisco() (key.DiscoPublic, error)
	AuthorizePair(key.DiscoPublic, key.DiscoPublic) (PairLease, error)
	ControlPayload() []byte
}
type derpControlRequest struct{ request derpquic.ControlRequest }

func (r derpControlRequest) SourceDisco() (key.DiscoPublic, error) { return r.request.SourceDisco() }
func (r derpControlRequest) AuthorizePair(a, b key.DiscoPublic) (PairLease, error) {
	return r.request.AuthorizePair(a, b)
}
func (r derpControlRequest) ControlPayload() []byte { return r.request.Payload }

type allocation struct {
	lease   PairLease
	expires time.Time
}
type Server struct {
	dataRates                 map[string]*rate
	mu                        sync.Mutex
	udp                       *udprelay.Server
	config                    Config
	allocations               map[key.SortedPairOfDiscoPublic]*allocation
	accounts                  map[string]*rate
	ctx                       context.Context
	cancel                    context.CancelFunc
	done                      chan struct{}
	closed                    bool
	denied, admitted, packets atomic.Uint64
}
type rate struct {
	last    time.Time
	packets float64
}

func (r *rate) allow(now time.Time, perSecond, burst float64) bool {
	if r.last.IsZero() {
		r.packets = burst
	} else {
		r.packets = min(burst, r.packets+now.Sub(r.last).Seconds()*perSecond)
	}
	r.last = now
	if r.packets < 1 {
		return false
	}
	r.packets--
	return true
}

type Stats struct {
	Allocations                         int
	Admitted, Denied, AuthorizedPackets uint64
}

func New(config Config) (*Server, error) {
	if !config.Service.Valid() || config.DiscoPrivate.IsZero() || derpquic.DiscoKeyString(config.DiscoPrivate.Public()) != config.Service.DiscoPublicKey || config.Port == 0 || len(config.Addresses) == 0 || len(config.Addresses) > 4 {
		return nil, derpquic.ErrAdmission
	}
	for _, a := range config.Addresses {
		if !a.IsValid() || a.Port() != config.Port || a.Addr().IsUnspecified() || a.Addr().IsMulticast() {
			return nil, derpquic.ErrAdmission
		}
	}
	udp, err := udprelay.NewServer(func(string, ...any) {}, config.Port, true, new(usermetric.Registry), nil)
	if err != nil {
		return nil, err
	}
	udp.SetStaticAddrPorts(views.SliceOf(append([]netip.AddrPort(nil), config.Addresses...)))
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{udp: udp, config: config, allocations: make(map[key.SortedPairOfDiscoPublic]*allocation), accounts: make(map[string]*rate), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	s.dataRates = make(map[string]*rate)
	udp.SetEndpointAuthorizer(s.authorizePacket)
	udp.SetForwardingLimiter(s.allowForward)
	go s.maintain()
	return s, nil
}
func (s *Server) Handle(ctx context.Context, request derpquic.ControlRequest) ([]byte, error) {
	return s.HandleControl(ctx, derpControlRequest{request: request})
}
func (s *Server) HandleControl(ctx context.Context, request ControlRequest) ([]byte, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sender, err := request.SourceDisco()
	if err != nil {
		return nil, err
	}
	header := len(disco.Magic) + 32
	b := request.ControlPayload()
	if len(b) < header+24+16 || len(b) > derpquic.MaxPacket || string(b[:len(disco.Magic)]) != disco.Magic || string(b[len(disco.Magic):header]) != string(sender.AppendTo(nil)) {
		return nil, derpquic.ErrProtocol
	}
	clear, ok := s.config.DiscoPrivate.Shared(sender).Open(b[header:])
	if !ok {
		return nil, derpquic.ErrAdmission
	}
	message, err := disco.Parse(clear)
	if err != nil {
		return nil, derpquic.ErrProtocol
	}
	req, ok := message.(*disco.AllocateUDPRelayEndpointRequest)
	if !ok {
		return nil, derpquic.ErrProtocol
	}
	lease, err := request.AuthorizePair(req.ClientDisco[0], req.ClientDisco[1])
	if err != nil {
		s.denied.Add(1)
		return nil, err
	}
	expires, valid := lease.Valid()
	if !valid {
		return nil, derpquic.ErrAdmission
	}
	pair := key.NewSortedPairOfDiscoPublic(req.ClientDisco[0], req.ClientDisco[1])
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, derpquic.ErrClosed
	}
	current := s.allocations[pair]
	if current != nil {
		if _, valid := current.lease.Valid(); !valid {
			s.udp.RevokeEndpoint(req.ClientDisco[0], req.ClientDisco[1])
			delete(s.allocations, pair)
			current = nil
		}
	}
	account := lease.AccountID()
	count := 0
	for _, a := range s.allocations {
		if a.lease.AccountID() == account {
			count++
		}
	}
	if current == nil && (len(s.allocations) >= MaxAllocations || count >= MaxAccountAllocations) {
		s.denied.Add(1)
		return nil, derpquic.ErrOverload
	}
	limit := s.accounts[account]
	if limit == nil {
		limit = new(rate)
		s.accounts[account] = limit
	}
	if !limit.allow(time.Now(), 8, 8) {
		s.denied.Add(1)
		return nil, derpquic.ErrOverload
	}
	endpoint, err := s.udp.AllocateEndpointWithPolicy(req.ClientDisco[0], req.ClientDisco[1], expires, MaxAllocations)
	if err != nil {
		return nil, err
	}
	if _, valid = lease.Valid(); !valid {
		s.udp.RevokeEndpoint(req.ClientDisco[0], req.ClientDisco[1])
		return nil, derpquic.ErrAdmission
	}
	s.allocations[pair] = &allocation{lease, expires}
	s.admitted.Add(1)
	response := &disco.AllocateUDPRelayEndpointResponse{Generation: req.Generation, UDPRelayEndpoint: disco.UDPRelayEndpoint{ServerDisco: endpoint.ServerDisco, ClientDisco: endpoint.ClientDisco, LamportID: endpoint.LamportID, VNI: endpoint.VNI, BindLifetime: min(endpoint.BindLifetime.Duration, time.Until(expires)), SteadyStateLifetime: min(endpoint.SteadyStateLifetime.Duration, time.Until(expires)), AddrPorts: endpoint.AddrPorts}}
	out := append([]byte(nil), disco.Magic...)
	out = s.config.DiscoPrivate.Public().AppendTo(out)
	out = append(out, s.config.DiscoPrivate.Shared(sender).Seal(response.AppendMarshal(nil))...)
	return out, nil
}
func (s *Server) authorizePacket(a, b key.DiscoPublic) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	allocation := s.allocations[key.NewSortedPairOfDiscoPublic(a, b)]
	if allocation == nil {
		return false
	}
	if _, ok := allocation.lease.Valid(); !ok {
		return false
	}
	return true
}
func (s *Server) maintain() {
	defer close(s.done)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			for pair, a := range s.allocations {
				expiry, valid := a.lease.Valid()
				p := pair.Get()
				if !valid {
					s.udp.RevokeEndpoint(p[0], p[1])
					delete(s.allocations, pair)
					continue
				}
				if expiry.After(a.expires) {
					if _, err := s.udp.AllocateEndpointWithPolicy(p[0], p[1], expiry, MaxAllocations); err != nil {
						s.udp.RevokeEndpoint(p[0], p[1])
						delete(s.allocations, pair)
					} else {
						a.expires = expiry
					}
				}
			}
			for account := range s.accounts {
				live := false
				for _, a := range s.allocations {
					if a.lease.AccountID() == account {
						live = true
						break
					}
				}
				if !live {
					delete(s.accounts, account)
					delete(s.dataRates, account)
				}
			}
			s.mu.Unlock()
		}
	}
}
func (s *Server) Snapshot() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{len(s.allocations), s.admitted.Load(), s.denied.Load(), s.packets.Load()}
}
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cancel()
	s.mu.Unlock()
	err := s.udp.Close()
	<-s.done
	return err
}

func (s *Server) allowForward(a, b key.DiscoPublic) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.allocations[key.NewSortedPairOfDiscoPublic(a, b)]
	if s.closed || entry == nil {
		return false
	}
	if _, ok := entry.lease.Valid(); !ok {
		return false
	}
	account := entry.lease.AccountID()
	limit := s.dataRates[account]
	if limit == nil {
		limit = new(rate)
		s.dataRates[account] = limit
	}
	if !limit.allow(time.Now(), 4096, 256) {
		s.denied.Add(1)
		return false
	}
	s.packets.Add(1)
	return true
}
