// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

package udprelay

import (
	"bytes"
	"errors"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"tailscale.com/disco"
	"tailscale.com/net/packet"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
	"tailscale.com/util/usermetric"
)

func paperboatServer(t *testing.T) *Server {
	t.Helper()
	deregisterMetrics()
	s, err := NewServer(t.Logf, 0, true, new(usermetric.Registry), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetStaticAddrPorts(views.SliceOf([]netip.AddrPort{netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), s.uc4Port)}))
	return s
}
func TestPaperboatPolicyAllocationBoundsRenewal(t *testing.T) {
	s := paperboatServer(t)
	a, b, c := key.NewDisco().Public(), key.NewDisco().Public(), key.NewDisco().Public()
	deadline := time.Now().Add(time.Minute)
	first, err := s.AllocateEndpointWithPolicy(a, b, deadline, 1)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := s.AllocateEndpointWithPolicy(a, b, deadline.Add(time.Second), 1)
	if err != nil || first.VNI != renewed.VNI || first.LamportID != renewed.LamportID {
		t.Fatal("renewal did not preserve live allocation")
	}
	if renewed.SteadyStateLifetime.Duration > 61*time.Second {
		t.Fatal("advertised lifetime outlives authority")
	}
	if _, err := s.AllocateEndpointWithPolicy(a, c, deadline, 1); !errors.Is(err, ErrEndpointLimit) {
		t.Fatalf("allocation limit: %v", err)
	}
	if _, err := s.AllocateEndpoint(a, b); !errors.Is(err, ErrEndpointPolicy) {
		t.Fatal("upstream allocation removed policy")
	}
	for _, tc := range []struct {
		a, b   key.DiscoPublic
		expiry time.Time
		max    int
	}{{a, b, time.Now().Add(-time.Second), 1}, {a, b, deadline, 0}, {a, a, deadline, 1}, {key.DiscoPublic{}, b, deadline, 1}} {
		if _, err := s.AllocateEndpointWithPolicy(tc.a, tc.b, tc.expiry, tc.max); !errors.Is(err, ErrEndpointPolicy) {
			t.Fatalf("invalid policy: %v", err)
		}
	}
	s.mu.Lock()
	old := s.serverEndpointByDisco[key.NewSortedPairOfDiscoPublic(a, b)]
	s.mu.Unlock()
	old.mu.Lock()
	old.policyDeadline = mono.Now().Add(-time.Second)
	old.mu.Unlock()
	replacement, err := s.AllocateEndpointWithPolicy(a, b, deadline, 1)
	if err != nil || replacement.VNI == first.VNI || replacement.LamportID <= first.LamportID {
		t.Fatal("expired allocation was reused")
	}
	old.mu.Lock()
	closed := old.closed
	old.mu.Unlock()
	if !closed {
		t.Fatal("expired allocation pointer remains open")
	}
	s.RevokeEndpoint(a, b)
	s.RevokeEndpoint(a, b)
	if _, ok := s.serverEndpointByVNI.Load(replacement.VNI); ok {
		t.Fatal("revoked VNI remains indexed")
	}
	if _, err := s.AllocateEndpointWithPolicy(a, c, deadline, 1); err != nil {
		t.Fatalf("revocation did not release capacity: %v", err)
	}
}
func TestPaperboatPolicyLiveForwardingAndAuthorization(t *testing.T) {
	s := paperboatServer(t)
	a, b := key.NewDisco(), key.NewDisco()
	se, err := s.AllocateEndpointWithPolicy(a.Public(), b.Public(), time.Now().Add(time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	var permitted atomic.Bool
	permitted.Store(true)
	s.SetEndpointAuthorizer(func(x, y key.DiscoPublic) bool {
		return key.NewSortedPairOfDiscoPublic(x, y) == key.NewSortedPairOfDiscoPublic(a.Public(), b.Public()) && permitted.Load()
	})
	ca := newTestClient(t, se.VNI, se.AddrPorts[0], a, b.Public(), se.ServerDisco)
	defer ca.close()
	cb := newTestClient(t, se.VNI, se.AddrPorts[0], b, a.Public(), se.ServerDisco)
	defer cb.close()
	ca.handshake(t)
	cb.handshake(t)
	// Ensure final bind answers have been processed before sending unreliable data.
	s.mu.Lock()
	e := s.serverEndpointByDisco[key.NewSortedPairOfDiscoPublic(a.Public(), b.Public())]
	s.mu.Unlock()
	until := time.Now().Add(time.Second)
	for {
		e.mu.Lock()
		bound := e.isBoundLocked()
		e.mu.Unlock()
		if bound {
			break
		}
		if time.Now().After(until) {
			t.Fatal("bind did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	payload := bytes.Repeat([]byte{4}, 1312)
	ca.writeDataPkt(t, payload)
	if got := cb.readDataPkt(t); !bytes.Equal(got, payload) {
		t.Fatal("packet bytes changed")
	}
	permitted.Store(false)
	ca.writeDataPkt(t, payload)
	cb.uc.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if n, err := cb.uc.Read(make([]byte, 4096)); err == nil || n != 0 {
		t.Fatal("revoked live authorization forwarded traffic")
	}
	permitted.Store(true)
	ca.writeDataPkt(t, make([]byte, 2049))
	cb.uc.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if n, err := cb.uc.Read(make([]byte, 4096)); err == nil || n != 0 {
		t.Fatal("oversized packet forwarded")
	}
	s.RevokeEndpoint(a.Public(), b.Public())
	e.mu.Lock()
	from := e.boundAddrPorts[0]
	e.mu.Unlock()
	if _, to := e.handleDataPacket(from, payload, mono.Now()); to.IsValid() {
		t.Fatal("retained revoked endpoint pointer forwarded")
	}
}
func TestPaperboatAbsoluteExpiryFencesBindAndData(t *testing.T) {
	s := paperboatServer(t)
	a, b := key.NewDisco().Public(), key.NewDisco().Public()
	se, err := s.AllocateEndpointWithPolicy(a, b, time.Now().Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	e := s.serverEndpointByDisco[key.NewSortedPairOfDiscoPublic(a, b)]
	s.mu.Unlock()
	now := mono.Now()
	from := netip.MustParseAddrPort("127.0.0.1:10001")
	e.mu.Lock()
	e.boundAddrPorts = [2]netip.AddrPort{from, netip.MustParseAddrPort("127.0.0.1:10002")}
	e.policyDeadline = now
	e.mu.Unlock()
	if _, to := e.handleDataPacket(from, make([]byte, 1312+packet.GeneveFixedHeaderLength), now); to.IsValid() {
		t.Fatal("expired active data forwarded")
	}
	request := &disco.BindUDPRelayEndpoint{BindUDPRelayEndpointCommon: disco.BindUDPRelayEndpointCommon{VNI: se.VNI, Generation: 1, RemoteKey: b}}
	if _, to := e.handleDiscoControlMsg(from, 0, request, s.discoPublic, s.getMACSecrets(now), now, s.metrics); to.IsValid() {
		t.Fatal("expired bind produced amplification")
	}
}

func TestPaperboatForwardingQuotaIgnoresForgedSources(t *testing.T) {
	s := paperboatServer(t)
	a, b := key.NewDisco().Public(), key.NewDisco().Public()
	se, err := s.AllocateEndpointWithPolicy(a, b, time.Now().Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	e := s.serverEndpointByDisco[key.NewSortedPairOfDiscoPublic(a, b)]
	s.mu.Unlock()
	source := netip.MustParseAddrPort("127.0.0.1:10001")
	e.mu.Lock()
	e.boundAddrPorts = [2]netip.AddrPort{source, netip.MustParseAddrPort("127.0.0.1:10002")}
	e.mu.Unlock()
	var calls atomic.Int32
	s.SetForwardingLimiter(func(x, y key.DiscoPublic) bool { calls.Add(1); return false })
	data := make([]byte, packet.GeneveFixedHeaderLength+1312)
	header := packet.GeneveHeader{Protocol: packet.GeneveProtocolWireGuard}
	header.VNI.Set(se.VNI)
	if err := header.Encode(data); err != nil {
		t.Fatal(err)
	}
	if _, to, _ := s.handlePacket(netip.MustParseAddrPort("127.0.0.1:10003"), data); to.IsValid() || calls.Load() != 0 {
		t.Fatal("forged source debited forwarding quota")
	}
	if _, to, _ := s.handlePacket(source, data); to.IsValid() || calls.Load() != 1 {
		t.Fatal("bound source did not honor forwarding quota")
	}
	s.SetForwardingLimiter(nil)
	if write, to, _ := s.handlePacket(source, data); !to.IsValid() || len(write) != len(data) {
		t.Fatal("quota release did not recover forwarding")
	}
}
