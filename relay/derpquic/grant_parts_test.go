package derpquic

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

// An endpoint with more memberships than one grant may carry (256) sends one grant per chunk; the relay adds them up
// on its connection, so a membership named only in the final chunk still pairs.
func TestGrantsOfOneGenerationAddUpBeyondOnePartOfMemberships(t *testing.T) {
	for _, chunked := range []bool{true, false} {
		f := newFixture(t)
		s, address, _ := f.start("127.0.0.1:0")
		ka, kb := key.NewNode().Public(), key.NewNode().Public()
		ca, cb := certificate(t), certificate(t)
		ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
		pairScopes(&ga, &gb)
		pair := ga.Memberships[0]
		var all []Scope
		for i := 0; i < 998; i++ {
			other := pair
			other.ResourceID = fmt.Sprint("other_", i)
			all = append(all, other)
		}
		all = append(all, pair)
		var parts []string
		const perPart = 256
		for start := 0; start < len(all); start += perPart {
			chunk := ga
			chunk.Memberships = all[start:min(start+perPart, len(all))]
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
		if err := a.SendControl(kb, []byte("beyond-one-part")); err != nil {
			t.Fatal(err)
		}
		if chunked {
			receive(t, b, ka, []byte("beyond-one-part"))
			eventually(t, func() bool { return s.Snapshot().Forwarded > 0 })
		} else {
			eventually(t, func() bool { return s.Snapshot().Dropped > 0 })
			if s.Snapshot().Forwarded != 0 {
				t.Fatal("a membership the endpoint's first part does not carry was usable")
			}
		}
	}
}
