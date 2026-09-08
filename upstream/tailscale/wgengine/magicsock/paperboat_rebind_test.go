// Copyright (c) Paperboat contributors
// SPDX-License-Identifier: BSD-3-Clause

package magicsock

import (
	"net/netip"
	udprelay "tailscale.com/net/udprelay/endpoint"
	"tailscale.com/tstime/mono"
	"tailscale.com/types/key"
	"testing"
	"time"
)

func TestRelayRediscoveryAfterConnectivityChange(t *testing.T) {
	c := newConn(t.Logf)
	c.relayManager.hasPeerRelayServers.Store(true)
	now := mono.Now()
	addr := epAddr{ap: netip.MustParseAddrPort("127.0.0.1:12345")}
	addr.vni.Set(1)
	ep := &endpoint{c: c, relayCapable: true, bestAddr: addrQuality{epAddr: addr}, trustBestAddrUntil: now.Add(time.Minute), lastUDPRelayPathDiscovery: now}
	if ep.wantUDPRelayPathDiscoveryLocked(now) {
		t.Fatal("trusted recently discovered relay should be throttled")
	}
	ep.noteConnectivityChange()
	if ep.bestAddr.ap.IsValid() {
		t.Fatal("connectivity change retained stale route")
	}
	if !ep.wantUDPRelayPathDiscoveryLocked(mono.Now()) {
		t.Fatal("connectivity change left relay rediscovery throttled")
	}
}

// Completed relay handshakes are not cached as outstanding work: rediscovery
// of the same allocation must bind again with a newer source-binding generation.
func TestRelayRediscoveryRebindsSameAllocation(t *testing.T) {
	c := newConn(t.Logf)
	ep := &endpoint{c: c}
	rm := &relayManager{}
	rm.init()
	<-rm.runLoopStoppedCh
	se := udprelay.ServerEndpoint{ServerDisco: key.NewDisco().Public(), LamportID: 1, VNI: 1}
	event := newRelayServerEndpointEvent{wlb: endpointWithLastBest{ep: ep}, se: se}
	rm.handleNewServerEndpointRunLoop(event)
	first := rm.handshakeWorkByServerDiscoVNI[serverDiscoVNI{se.ServerDisco, se.VNI}]
	if first == nil {
		t.Fatal("first binding not started")
	}
	// The run loop normally consumes this completed handshake event.
	rm.handleHandshakeWorkDoneRunLoop(<-first.doneCh)
	ep.noteConnectivityChange()
	event.wlb.lastBest = ep.bestAddr
	event.wlb.lastBestIsTrusted = mono.Now().Before(ep.trustBestAddrUntil)
	rm.handleNewServerEndpointRunLoop(event)
	second := rm.handshakeWorkByServerDiscoVNI[serverDiscoVNI{se.ServerDisco, se.VNI}]
	if second == nil {
		t.Fatal("identical allocation suppressed binding after connectivity change")
	}
	if second.handshakeGen <= first.handshakeGen {
		t.Fatal("binding reused stale generation")
	}
	rm.stopWorkRunLoop(ep)
}
