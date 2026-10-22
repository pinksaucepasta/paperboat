package derpquic

import (
	"context"
	"crypto/tls"
	"testing"

	"tailscale.com/types/key"
)

// An endpoint with more peers than one grant may name (64) sends one grant per chunk of peers; the
// relay adds them up on its connection, so a peer named only in a later chunk is reachable.
func TestGrantsOfOneGenerationAddUpBeyondSixtyFourPeers(t *testing.T) {
	for _, chunked := range []bool{true, false} {
		f := newFixture(t)
		s, address, _ := f.start("127.0.0.1:0")
		ka, kb := key.NewNode().Public(), key.NewNode().Public()
		ca, cb := certificate(t), certificate(t)
		ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
		pairScopes(&ga, &gb)
		toB := ga.Peers[0]
		var others []Peer
		for i := 0; i < 69; i++ {
			others = append(others, Peer{WireGuardPublicKey: KeyString(key.NewNode().Public()), Scopes: toB.Scopes})
		}
		first, second := ga, ga
		first.Peers = others[:64]
		second.Peers = append(append([]Peer(nil), others[64:]...), toB)
		credential := f.token(first) + "\n" + f.token(second)
		if !chunked {
			credential = f.token(first)
		}
		a := NewClient(ClientConfig{Address: address, TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{ca}}, Credential: func(context.Context) (string, error) { return credential, nil }})
		t.Cleanup(func() { a.Close() })
		b := f.client(address, cb, gb)
		connect(t, a)
		connect(t, b)
		if err := a.SendControl(kb, []byte("beyond-64")); err != nil {
			t.Fatal(err)
		}
		if chunked {
			eventually(t, func() bool { return s.Snapshot().Forwarded > 0 })
		} else {
			eventually(t, func() bool { return s.Snapshot().Dropped > 0 })
			if s.Snapshot().Forwarded != 0 {
				t.Fatal("a peer the endpoint's grant does not name was reachable")
			}
		}
	}
}
