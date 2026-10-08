package derpquic

import (
	"context"
	"reflect"
	"time"

	"tailscale.com/types/key"
)

// ControlHandler consumes upstream discovery control for the registered local
// service. It must honor cancellation and must not retain request payloads.
type ControlHandler func(context.Context, ControlRequest) ([]byte, error)
type localService struct {
	identity ServiceDescriptor
	key      key.NodePublic
	handle   ControlHandler
}
type controlLease struct{ pair *PairLease }

type ControlRequest struct {
	authorization *controlLease
	server        *Server
	source        *peerConn
	Payload       []byte
}

func (s *Server) SetControlService(identity ServiceDescriptor, handler ControlHandler) error {
	if !identity.Valid() || handler == nil {
		return ErrAdmission
	}
	k, _ := ParseKey(identity.WireGuardPublicKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil || s.service != nil {
		return ErrProtocol
	}
	s.service = &localService{identity, k, handler}
	return nil
}
func (s *Server) serviceAllowedLocked(p *peerConn) bool {
	return s.service != nil && p != nil && s.peers[p.key] == p && p.grant.ExpiresAt > time.Now().Unix() && p.grant.PeerRelay != nil && *p.grant.PeerRelay == s.service.identity && (!s.draining || time.Now().Before(s.deadline))
}
func (r ControlRequest) SourceDisco() (key.DiscoPublic, error) {
	r.server.mu.Lock()
	defer r.server.mu.Unlock()
	if !r.server.serviceAllowedLocked(r.source) {
		return key.DiscoPublic{}, ErrAdmission
	}
	return ParseDiscoKey(r.source.grant.DiscoPublicKey)
}
func (r ControlRequest) AuthorizePair(a, b key.DiscoPublic) (*PairLease, error) {
	s := r.server
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining {
		return nil, ErrOverload
	}
	if a.IsZero() || b.IsZero() || a == b || !s.serviceAllowedLocked(r.source) {
		return nil, ErrAdmission
	}
	var first, second *peerConn
	ak, bk := DiscoKeyString(a), DiscoKeyString(b)
	for _, p := range s.peers {
		switch p.grant.DiscoPublicKey {
		case ak:
			if first != nil {
				return nil, ErrAdmission
			}
			first = p
		case bk:
			if second != nil {
				return nil, ErrAdmission
			}
			second = p
		}
	}
	if first == nil || second == nil || r.source != first && r.source != second {
		return nil, ErrAdmission
	}
	lease := &PairLease{server: s, a: first, b: second, disco: [2]key.DiscoPublic{a, b}, account: first.grant.AccountID, fences: [2]*grantFence{s.fences[grantID(first.grant)], s.fences[grantID(second.grant)]}}
	if _, ok := lease.validLocked(); !ok {
		return nil, ErrAdmission
	}
	r.authorization.pair = lease
	return lease, nil
}

// PairLease retains verified pair authority across DERP reconnects. Control
// replies additionally require the original authenticated connection owners.
// Renewal can extend its deadline only through fresh verified compatible grants.
type PairLease struct {
	server  *Server
	a, b    *peerConn
	disco   [2]key.DiscoPublic
	account string
	fences  [2]*grantFence
}

func (l *PairLease) AccountID() string { return l.account }
func (l *PairLease) validLocked() (time.Time, bool) {
	s := l.server
	if !s.serviceAllowedLocked(l.a) || !s.serviceAllowedLocked(l.b) || !s.authorizedLocked(l.a, l.b, time.Now()) || l.a.grant.DiscoPublicKey != DiscoKeyString(l.disco[0]) || l.b.grant.DiscoPublicKey != DiscoKeyString(l.disco[1]) {
		return time.Time{}, false
	}
	return l.capabilityValidLocked()
}
func (l *PairLease) capabilityValidLocked() (time.Time, bool) {
	s := l.server
	now := time.Now()
	if s.service == nil || s.draining && !now.Before(s.deadline) {
		return time.Time{}, false
	}
	for _, f := range l.fences {
		if f == nil || s.fences[grantID(f.Grant)] != f || f.PeerRelay == nil || *f.PeerRelay != s.service.identity {
			return time.Time{}, false
		}
	}
	a, b := l.fences[0].Grant, l.fences[1].Grant
	if !allowed(a, b, now) || a.DiscoPublicKey != DiscoKeyString(l.disco[0]) || b.DiscoPublicKey != DiscoKeyString(l.disco[1]) {
		return time.Time{}, false
	}
	match := func(g, other Grant) bool {
		for _, p := range g.Peers {
			if p.WireGuardPublicKey == other.WireGuardPublicKey {
				return p.DiscoPublicKey == other.DiscoPublicKey
			}
		}
		return false
	}
	if !match(a, b) || !match(b, a) {
		return time.Time{}, false
	}
	expires := time.Unix(min(a.ExpiresAt, b.ExpiresAt), 0)
	if s.draining && s.deadline.Before(expires) {
		expires = s.deadline
	}
	return expires, true
}

func (l *PairLease) Valid() (time.Time, bool) {
	l.server.mu.Lock()
	defer l.server.mu.Unlock()
	return l.capabilityValidLocked()
}

// tryControlService returns true only when the packet addressed our local service.
func (s *Server) tryControlService(p *peerConn, msg packet) bool {
	s.mu.Lock()
	service := s.service
	if service == nil || msg.peer != service.key {
		s.mu.Unlock()
		return false
	}
	valid := s.serviceAllowedLocked(p)
	s.mu.Unlock()
	if !valid {
		s.dropped.Add(1)
		return true
	}
	ctx, cancel := context.WithTimeout(p.conn.Context(), 5*time.Second)
	authorization := new(controlLease)
	reply, err := service.handle(ctx, ControlRequest{server: s, source: p, Payload: msg.data, authorization: authorization})
	cancel()
	if err != nil || len(reply) == 0 || len(reply) > MaxPacket {
		s.dropped.Add(1)
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lease := authorization.pair
	validPair := false
	if lease != nil {
		_, validPair = lease.validLocked()
	}
	if !s.serviceAllowedLocked(p) || !validPair {
		s.dropped.Add(1)
		return true
	}
	select {
	case p.out <- queued{packet: packet{peer: service.key, data: reply, control: true}, source: p, service: true, lease: lease}:
	default:
		s.dropped.Add(1)
	}
	return true
}

// A fence is retained only while its signed authority is continuously compatible.
// Revoke and incompatible renewals replace the record, permanently fencing old leases.
type grantFence struct{ Grant }

func grantID(g Grant) string { return g.AccountID + "\x00" + g.EndpointID }
func sameGrantAuthority(a, b Grant) bool {
	normalize := func(g Grant) Grant {
		g.IssuedAt = 0
		g.ExpiresAt = 0
		g.Generation = 0
		g.Peers = append([]Peer(nil), g.Peers...)
		for i := range g.Peers {
			g.Peers[i].Scopes = append([]Scope(nil), g.Peers[i].Scopes...)
			for j := range g.Peers[i].Scopes {
				g.Peers[i].Scopes[j].ExpiresAt = 0
			}
		}
		return g
	}
	return reflect.DeepEqual(normalize(a), normalize(b))
}
