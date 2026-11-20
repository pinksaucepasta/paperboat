package derpquic

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"testing"
	"time"

	"tailscale.com/derp"
	"tailscale.com/types/key"
)

// namespaced connects a carrier of the given generation namespace to node n.
func namespaced(t *testing.T, n *meshNode, cert tls.Certificate, g Grant, namespace [32]byte) *Client {
	t.Helper()
	token := n.f.token(g)
	c := NewClient(ClientConfig{Address: n.addr, Namespace: namespace, GenerationDisco: key.NewDisco().Public(),
		TLS: &tls.Config{RootCAs: n.f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return token, nil }})
	t.Cleanup(func() { c.Close() })
	connect(t, c)
	return c
}

// networkScope is the account's mesh network membership: it admits control traffic (controlAdmitted) without pairing.
func networkScope(g *Grant) {
	g.Memberships = append(g.Memberships, Scope{ResourceKind: "machine_network", ResourceID: "network_account", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: g.ExpiresAt})
}

// TestMeshControlAcrossNamespaceRefused: a compromised mesh peer forwards packets, control and data, from one carrier
// generation to another of the same account; the destination relay delivers neither.
func TestMeshControlAcrossNamespaceRefused(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	ga, gb := a.f.grant(ka, certA, "a"), b.f.grant(kb, certB, "b")
	pairScopes(&ga, &gb)
	networkScope(&ga)
	networkScope(&gb)
	other := sha256.Sum256([]byte("other-generation"))
	namespaced(t, a, certA, ga, [32]byte{})
	cb := namespaced(t, b, certB, gb, other)
	dst := routeKey(kb, other)
	eventually(t, func() bool { return a.mesh.holder(dst) != nil })
	a.mesh.mu.RLock()
	l := a.mesh.links["relay-b"]
	a.mesh.mu.RUnlock()
	for _, control := range []bool{true, false} {
		before := b.mesh.drops[dropUnpaired].Load()
		eventually(t, func() bool {
			l.out.push(queued{packet: packet{peer: ka, data: payloadOf(100), control: control}, dst: dst})
			return b.mesh.drops[dropUnpaired].Load() > before
		})
	}
	if !silent(cb, 200*time.Millisecond) || b.mesh.received.Load() != 0 || b.server.Snapshot().BytesForwarded != 0 {
		t.Fatal("a packet from another carrier namespace was delivered")
	}
}

// TestMeshControlSameNamespaceAllowed: inside one namespace a control packet of two network members still crosses.
func TestMeshControlSameNamespaceAllowed(t *testing.T) {
	a, b := meshPair(t, true)
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	ga, gb := a.f.grant(ka, certA, "a"), b.f.grant(kb, certB, "b")
	networkScope(&ga)
	networkScope(&gb) // network members, not paired: only control may pass
	namespace := sha256.Sum256([]byte("same-generation"))
	ca := namespaced(t, a, certA, ga, namespace)
	cb := namespaced(t, b, certB, gb, namespace)
	eventually(t, func() bool { return a.mesh.holder(routeKey(kb, namespace)) != nil })
	got := make(chan bool, 1)
	go func() {
		for {
			m, _, err := cb.RecvDetail()
			if err != nil {
				got <- false
				return
			}
			if p, ok := m.(derp.ReceivedPacket); ok && p.Source == ka {
				got <- true
				return
			}
		}
	}()
	tick, deadline := time.NewTicker(20*time.Millisecond), time.After(waitBound)
	defer tick.Stop()
	for {
		select {
		case ok := <-got:
			if !ok {
				t.Fatal("receiver closed")
			}
			return
		case <-tick.C:
			ca.SendControl(kb, []byte("control"))
		case <-deadline:
			t.Fatal("control packet inside one namespace was not delivered over the mesh")
		}
	}
}
