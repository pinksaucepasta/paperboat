// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"tailscale.com/tailcfg"
	"tailscale.com/tstest/integration"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/wgengine/filter"
)

func TestAuthorityPeerValidation(t *testing.T) {
	local := netip.MustParseAddr("fd7a:115c:a1e0::1")
	peer := netip.MustParseAddr("fd7a:115c:a1e0::2")
	k1, k2 := key.NewNode().Public(), key.NewNode().Public()
	for _, tt := range []struct {
		name  string
		peers map[key.NodePublic]netip.Addr
		bad   bool
	}{
		{"empty-deny", nil, false},
		{"allocated", map[key.NodePublic]netip.Addr{k1: peer}, false},
		{"zero-key", map[key.NodePublic]netip.Addr{{}: peer}, true},
		{"own-address", map[key.NodePublic]netip.Addr{k1: local}, true},
		{"duplicate", map[key.NodePublic]netip.Addr{k1: peer, k2: peer}, true},
		{"outside-prefix", map[key.NodePublic]netip.Addr{k1: netip.MustParseAddr("fd00::2")}, true},
		{"zone", map[key.NodePublic]netip.Addr{k1: peer.WithZone("eth0")}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateAllowedPeers(local, tt.peers); (err != nil) != tt.bad {
				t.Fatalf("validation error = %v, want invalid=%v", err, tt.bad)
			}
		})
	}
	s := &Server{Key: key.NewNode(), LocalAddr: local}
	if err := s.ReplaceAllowedPeers(map[key.NodePublic]netip.Addr{k1: peer}); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceAllowedPeers(nil); err != nil {
		t.Fatal(err)
	}
	if s.AllowedPeers == nil || len(s.AllowedPeers) != 0 {
		t.Fatal("nil replacement must remain explicit empty-deny")
	}
	s.Key = key.NodePrivate{}
	if err := s.validateAuthority(); err == nil {
		t.Fatal("missing identity accepted")
	}
}

func TestAuthoritySharedEngineDialsTwoPeersAndRevokesOne(t *testing.T) {
	dm := integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1")
	region := dm.Regions[1]
	cliKey, firstKey, secondKey := key.NewNode(), key.NewNode(), key.NewNode()
	cliAddr := netip.MustParseAddr("fd7a:115c:a1e0::10")
	firstAddr := netip.MustParseAddr("fd7a:115c:a1e0::11")
	secondAddr := netip.MustParseAddr("fd7a:115c:a1e0::12")
	startPeer := func(private key.NodePrivate, address netip.Addr) *Server {
		server := &Server{Key: private, LocalAddr: address, AllowedPeers: map[key.NodePublic]netip.Addr{cliKey.Public(): cliAddr}, Region: region, ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) {
			return func(connection ConnPacketConn) {
				buffer := make([]byte, 64)
				for {
					n, err := connection.Read(buffer)
					if err != nil {
						return
					}
					_, _ = connection.Write(buffer[:n])
				}
			}
		}}
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = server.Close() })
		return server
	}
	first := startPeer(firstKey, firstAddr)
	second := startPeer(secondKey, secondAddr)
	shared := &Server{Key: cliKey, LocalAddr: cliAddr, AllowedPeers: map[key.NodePublic]netip.Addr{firstKey.Public(): firstAddr, secondKey.Public(): secondAddr}, Region: region, ServedUDPPorts: []filter.PortRange{}}
	if err := shared.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	dial := func(peer *Server, private key.NodePrivate, address netip.Addr, payload string) ConnPacketConn {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		connection, err := shared.DialAuthorizedUDP(ctx, private.Public(), peer.lb.discoPublic(), netip.AddrPortFrom(address, 443))
		if err != nil {
			t.Fatal(err)
		}
		_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
		if _, err = connection.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, len(payload))
		if n, err := connection.Read(buffer); err != nil || n != len(payload) || string(buffer) != payload {
			t.Fatalf("echo=%q n=%d err=%v", buffer, n, err)
		}
		_ = connection.SetDeadline(time.Time{})
		return connection
	}
	firstFlow := dial(first, firstKey, firstAddr, "first")
	defer firstFlow.Close()
	secondFlow := dial(second, secondKey, secondAddr, "second")
	defer secondFlow.Close()
	if err := shared.ReplaceAllowedPeers(map[key.NodePublic]netip.Addr{secondKey.Public(): secondAddr}); err != nil {
		t.Fatal(err)
	}
	_ = firstFlow.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = firstFlow.Write([]byte("revoked"))
	buffer := make([]byte, 7)
	if _, err := firstFlow.Read(buffer); err == nil {
		t.Fatal("removed peer flow remained usable")
	}
	_ = secondFlow.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := secondFlow.Write([]byte("kept")); err != nil {
		t.Fatal(err)
	}
	buffer = make([]byte, 4)
	if n, err := secondFlow.Read(buffer); err != nil || n != 4 || string(buffer) != "kept" {
		t.Fatalf("unrelated peer after removal=%q n=%d err=%v", buffer, n, err)
	}
}

func TestAuthorityRoutesAndAdmission(t *testing.T) {
	local, peer := netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("fd7a:115c:a1e0::2")
	k := key.NewNode().Public()
	b := newLocoBackend(key.NewNode())
	b.addr, b.addrPrefix = local, pfxOf(local)
	b.allowedPeers = map[key.NodePublic]netip.Addr{}
	b.logf = func(string, ...any) {}
	if b.onMeow(k, key.DiscoPublic{}) {
		t.Fatal("empty authority policy admitted peer")
	}
	b.clients = map[key.NodePublic]*tailcfg.Node{k: {Key: k, Addresses: []netip.Prefix{pfxOf(peer)}, AllowedIPs: []netip.Prefix{pfxOf(peer)}}}
	if got, ok := b.peerByIP(peer); !ok || got != k {
		t.Fatal("server ignored allocated peer address")
	}
	delete(b.clients, k)
	if _, ok := b.peerConfig(k); ok {
		t.Fatal("removed peer retained WireGuard configuration")
	}
	if _, ok := b.peerByIP(peer); ok {
		t.Fatal("removed peer retained route")
	}
}

func TestAuthorityPacketFilter(t *testing.T) {
	local, peer := netip.MustParseAddr("fd7a:115c:a1e0::1"), netip.MustParseAddr("fd7a:115c:a1e0::2")
	other := netip.MustParseAddr("fd7a:115c:a1e0::3")
	k := key.NewNode().Public()
	b := newLocoBackend(key.NewNode())
	b.addr, b.addrPrefix = local, pfxOf(local)
	b.allowedPeers = map[key.NodePublic]netip.Addr{k: peer}
	b.logf = func(string, ...any) {}
	s := &Server{lb: b, ServedUDPPorts: []filter.PortRange{{First: 443, Last: 443}}, OnUDP: func(uint16) func(ConnPacketConn) { return nil }}
	f := s.buildFilter()
	if f.Check(peer, local, 443, ipproto.UDP) != filter.Accept {
		t.Fatal("allocated source rejected")
	}
	if f.Check(other, local, 443, ipproto.UDP) != filter.Drop {
		t.Fatal("unallocated source admitted")
	}
	if f.Check(peer, local, 444, ipproto.UDP) != filter.Drop {
		t.Fatal("unserved port admitted")
	}
	if f.Check(peer, other, 443, ipproto.UDP) != filter.Drop {
		t.Fatal("proxy destination admitted")
	}
	b.allowedPeers = nil
	if s.buildFilter().Check(peer, local, 443, ipproto.UDP) != filter.Drop {
		t.Fatal("empty policy admitted source")
	}
}
