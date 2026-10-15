package derpquic

import (
	"errors"
	"tailscale.com/types/key"
	"testing"
	"time"
)

func TestAdmittedRelayCarriersOutliveGrantAndRevoke(t *testing.T) {
	for _, transport := range []string{"quic", "wss"} {
		t.Run(transport, func(t *testing.T) {
			f := newFixture(t)
			s, address, _ := f.start("127.0.0.1:0")
			ka, kb := key.NewNode().Public(), key.NewNode().Public()
			ca, cb := certificate(t), certificate(t)
			ga, gb := f.grant(ka, ca, "a"), f.grant(kb, cb, "b")
			expires := time.Now().Add(2 * time.Second).Unix()
			ga.ExpiresAt = expires
			gb.ExpiresAt = expires
			pairScopes(&ga, &gb)
			var a Carrier
			if transport == "wss" {
				a, _ = f.wss(s, ca, ga, nil)
			} else {
				a = f.client(address, ca, ga)
			}
			b := f.client(address, cb, gb)
			if err := a.Connect(t.Context()); err != nil {
				t.Fatal(err)
			}
			connect(t, b)
			if err := a.Send(kb, []byte("before")); err != nil {
				t.Fatal(err)
			}
			receive(t, b, ka, []byte("before"))
			time.Sleep(time.Until(time.Unix(expires, 0)) + 350*time.Millisecond)
			if err := a.Send(kb, []byte("after")); err != nil {
				t.Fatal("expiry ended admitted relay", err)
			}
			receive(t, b, ka, []byte("after"))
			// An expired proof cannot authenticate a replacement connection.
			fresh := f.client(address, ca, ga)
			if err := fresh.Connect(t.Context()); !errors.Is(err, ErrExpired) {
				t.Fatal("expired grant admitted replacement", err)
			}
			closed := make(chan error, 1)
			go func() { _, _, err := a.RecvDetail(); closed <- err }()
			if err := s.Revoke("account", "a", ga.Generation+1); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-closed:
				if err == nil {
					t.Fatal("revocation left carrier readable")
				}
			case <-time.After(time.Second):
				t.Fatal("revocation did not close carrier")
			}
			before := s.Snapshot().Forwarded
			if err := a.Send(kb, []byte("revoked")); err == nil {
				t.Fatal("revoked carrier accepted traffic")
			}
			if s.Snapshot().Forwarded != before {
				t.Fatal("revoked carrier forwarded traffic")
			}
		})
	}
}
