// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

package mesh

import (
	"encoding/hex"
	"net/netip"
	"testing"

	"go4.org/mem"
	"tailscale.com/types/ipproto"
	"tailscale.com/types/key"
	"tailscale.com/wgengine/filter"
)

func TestStartRequiresExplicitAuthority(t *testing.T) {
	k := key.NewNode()
	addr := netip.MustParseAddr("fd7a:115c:a1e0::1")
	for name, server := range map[string]*Server{
		"zero":               {},
		"missing-key":        {LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{}},
		"missing-allocation": {Key: k, AllowedPeers: map[key.NodePublic]netip.Addr{}},
		"missing-policy":     {Key: k, LocalAddr: addr},
		"self-key":           {Key: k, LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{k.Public(): netip.MustParseAddr("fd7a:115c:a1e0::2")}},
		"negative-idle":      {Key: k, LocalAddr: addr, AllowedPeers: map[key.NodePublic]netip.Addr{}, UDPIdleTimeout: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := server.Start(); err == nil {
				server.Close()
				t.Fatal("invalid authority started")
			}
			if server.lb != nil {
				t.Fatal("invalid authority created network resources")
			}
		})
	}
}

func TestFilterNeverAdmitsTCPOrUnspecifiedUDPPorts(t *testing.T) {
	peer := netip.MustParseAddr("fd7a:115c:a1e0::2")
	b := newLocoBackend(key.NewNode())
	b.addr = netip.MustParseAddr("fd7a:115c:a1e0::1")
	b.addrPrefix = pfxOf(b.addr)
	b.allowedPeers = map[key.NodePublic]netip.Addr{key.NewNode().Public(): peer}
	s := &Server{lb: b, OnUDP: func(uint16) func(ConnPacketConn) { return nil }}
	if s.buildFilter().Check(peer, b.addr, 443, ipproto.UDP) != filter.Drop {
		t.Fatal("unspecified UDP ports admitted")
	}
	s.ServedUDPPorts = []filter.PortRange{{First: 443, Last: 443}}
	if s.buildFilter().CheckTCP(peer, b.addr, 443) != filter.Drop {
		t.Fatal("TCP admitted")
	}
	if s.buildFilter().Check(peer, b.addr, 443, ipproto.UDP) != filter.Accept {
		t.Fatal("authorized UDP denied")
	}
	s.OnUDP = nil
	if s.buildFilter().Check(peer, b.addr, 443, ipproto.UDP) != filter.Drop {
		t.Fatal("outbound-only engine admitted new inbound UDP")
	}
}

func TestDiscoveryDerivationRemainsPinned(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i)
	}
	got := discoPrivateForNode(key.NodePrivateFromRaw32(mem.B(raw[:]))).Public()
	const want = "586a55ca01992289bc4cf4d6f738e00b4c175fe61be2dcdc576f8dd11ff42b5a"
	wantRaw, err := hex.DecodeString(want)
	if err != nil {
		t.Fatal(err)
	}
	if got != key.DiscoPrivateFromRaw32(mem.B(wantRaw)).Public() {
		t.Fatal("discovery identity changed from pinned Tailcat derivation")
	}
}

func TestBootstrapRejectsZeroDiscoveryKey(t *testing.T) {
	b := newLocoBackend(key.NewNode())
	peer := key.NewNode().Public()
	b.allowedPeers = map[key.NodePublic]netip.Addr{peer: netip.MustParseAddr("fd7a:115c:a1e0::2")}
	if b.onMeow(peer, key.DiscoPublic{}) {
		t.Fatal("zero discovery key admitted")
	}
	if len(b.clients) != 0 {
		t.Fatal("malformed bootstrap changed admission state")
	}
}
