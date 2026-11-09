package derpquic

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"tailscale.com/types/key"
)

// The parts of one authentication arrive over up to the read deadline; a part that expired by the
// time the last one arrived must not be admitted on the clock read before the first.
func TestGrantPartExpiredBeforeAdmissionIsRefused(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
	pairScopes(&ga, &gb)
	b := f.client(address, cb, gb)
	connect(t, f.client(address, ca, ga))
	connect(t, b)
	b.mu.Lock()
	cc := b.current
	b.mu.Unlock()

	now := time.Now().Unix()
	short, fresh := gb, gb
	short.Generation, fresh.Generation = gb.Generation+1, gb.Generation+1
	short.IssuedAt, short.ExpiresAt = now-10, now+1
	short.Peers = []Peer{{WireGuardPublicKey: gb.Peers[0].WireGuardPublicKey, Scopes: []Scope{gb.Peers[0].Scopes[0]}}}
	short.Peers[0].Scopes[0].ExpiresAt = short.ExpiresAt
	// Hold the write lock throughout so the client's own credential refresh cannot come in between.
	cc.writeMu.Lock()
	err := writeFrame(cc.stream, frameAuthBatch, append(binary.BigEndian.AppendUint32(nil, 2), f.token(short)...))
	if err == nil {
		time.Sleep(time.Until(time.Unix(now+2, 0).Add(100 * time.Millisecond)))
		err = writeFrame(cc.stream, frameAuth, []byte(f.token(fresh)))
	}
	cc.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	s.mu.Lock()
	defer s.mu.Unlock()
	// Refused as expired, the connection keeps its admitted grant; admitted, the newer generation
	// replaces it (and fences the client's own refresh of the old one, dropping the connection).
	if peer := s.peers[kb]; peer == nil || peer.grant.Generation != gb.Generation {
		t.Fatal("a grant part that expired before admission was admitted")
	}
}

type nullStream struct{}

func (nullStream) Read([]byte) (int, error)         { return 0, io.EOF }
func (nullStream) Write(b []byte) (int, error)      { return len(b), nil }
func (nullStream) SetReadDeadline(time.Time) error  { return nil }
func (nullStream) SetWriteDeadline(time.Time) error { return nil }

// Admission judges the grant at the clock it is given, not at the wall clock.
func TestAdmitAuthPartsUsesTheGivenClock(t *testing.T) {
	f := newFixture(t)
	s, err := NewServer(f.verifier)
	if err != nil {
		t.Fatal(err)
	}
	cert := f.tls.Certificates[0]
	g := f.grant(key.NewNode().Public(), cert, "clocked")
	p := &peerConn{stream: nullStream{}, tlsState: tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert.Leaf}}, rate: newBudget(1 << 20)}
	now := time.Now()
	if err := s.admitAuthParts(p, []byte(f.token(g)), 1, now.Add(2*time.Minute)); !errors.Is(err, ErrExpired) {
		t.Fatalf("a grant expired at the given clock was admitted: %v", err)
	}
	if err := s.admitAuthParts(p, []byte(f.token(g)), 1, now); err != nil {
		t.Fatalf("a live grant was refused: %v", err)
	}
}
