package derpquic

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

// An endpoint with more peers than one grant may name (64) sends one grant per chunk of peers; the
// relay adds them up on its connection, so the last peer of a 1,000-machine
// account is reachable even when it is named only in the final chunk.
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
		for i := 0; i < 998; i++ {
			others = append(others, Peer{WireGuardPublicKey: KeyString(key.NewNode().Public()), Scopes: toB.Scopes})
		}
		all := append(others, toB)
		var parts []string
		for start := 0; start < len(all); start += MaxGrantPeers {
			chunk := ga
			chunk.Peers = all[start:min(start+MaxGrantPeers, len(all))]
			parts = append(parts, f.token(chunk))
		}
		credential := strings.Join(parts, "\n")
		if !chunked {
			credential = parts[0]
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
			receive(t, b, ka, []byte("beyond-64"))
			eventually(t, func() bool { return s.Snapshot().Forwarded > 0 })
		} else {
			eventually(t, func() bool { return s.Snapshot().Dropped > 0 })
			if s.Snapshot().Forwarded != 0 {
				t.Fatal("a peer the endpoint's grant does not name was reachable")
			}
		}
	}
}
