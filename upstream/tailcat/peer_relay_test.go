// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

package tailcat

import (
	"net/netip"
	"testing"

	"tailscale.com/tailcfg"
	"tailscale.com/tailcfg/peercap"
	"tailscale.com/types/key"
	"tailscale.com/types/netmap"
	"tailscale.com/wgengine/filter"
)

func TestPeerRelayDiscoveryDoesNotGrantApplicationAccess(t *testing.T) {
	b := newLocoBackend(key.NewNode(), PresharedKey{})
	b.authority = true
	relay := &tailcfg.Node{ID: 1, Key: key.NewNode().Public(), DiscoKey: key.NewDisco().Public(), HomeDERP: 1, Addresses: []netip.Prefix{netip.MustParsePrefix("fd7a:115c:a1e0::77/128")}, AllowedIPs: []netip.Prefix{allIPv6}}
	if err := b.setPeerRelayNodes([]*tailcfg.Node{relay}); err != nil {
		t.Fatal(err)
	}
	relayAddr := relay.Addresses[0].Addr()
	relayKey := relay.Key
	relay.Addresses[0] = netip.MustParsePrefix("fd7a:115c:a1e0::88/128")
	nm := new(netmap.NetworkMap)
	b.appendRelayNodes(nm)
	if len(nm.Peers) != 1 || nm.Peers[0].Addresses().At(0).Addr() != relayAddr || nm.Peers[0].AllowedIPs().Len() != 0 || nm.Peers[0].Cap() < 121 || nm.Peers[0].ID() < 1<<60 {
		t.Fatal("relay copy or discovery metadata invalid")
	}
	if _, ok := b.peerConfig(relayKey); ok {
		t.Fatal("relay received WireGuard application config")
	}
	if _, ok := b.peerByIP(relayAddr); ok {
		t.Fatal("relay received application route")
	}
	f := filter.New(b.relayFilterMatches(), nil, nil, nil, nil, t.Logf)
	if !f.CapsWithValues(relayAddr, b.addr).HasCapability(peercap.RelayTarget) {
		t.Fatal("relay capability absent")
	}
	for _, m := range b.relayFilterMatches() {
		if len(m.Dsts) != 0 {
			t.Fatal("relay capability also grants packet destinations")
		}
	}
	b.serverPub = key.NewNode().Public()
	b.serverAddr = netip.MustParseAddr("fd7a:115c:a1e0::22")
	if _, ok := b.peerConfig(relayKey); ok {
		t.Fatal("client accepts relay application peer")
	}
	if _, ok := b.peerByIP(relayAddr); ok {
		t.Fatal("client routes applications to relay")
	}
}
func TestPeerRelayValidation(t *testing.T) {
	b := newLocoBackend(key.NewNode(), PresharedKey{})
	node := &tailcfg.Node{Key: key.NewNode().Public(), DiscoKey: key.NewDisco().Public(), HomeDERP: 1, Addresses: []netip.Prefix{netip.MustParsePrefix("fd7a:115c:a1e0::77/128")}}
	for _, nodes := range [][]*tailcfg.Node{{nil}, {node, node}, {{Key: b.pub, DiscoKey: node.DiscoKey, HomeDERP: 1, Addresses: node.Addresses}}, {{Key: node.Key, HomeDERP: 1, Addresses: node.Addresses}}, {{Key: node.Key, DiscoKey: node.DiscoKey, Addresses: node.Addresses}}} {
		if err := b.setPeerRelayNodes(nodes); err == nil {
			t.Fatal("invalid relay discovery configuration accepted")
		}
	}
}
