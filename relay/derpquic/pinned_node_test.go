package derpquic

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/nodetls"
	"tailscale.com/types/key"
)

func TestSignedNodePinForQUICAndWSS(t *testing.T) {
	for _, transport := range []string{"quic", "wss"} {
		for _, mismatch := range []bool{false, true} {
			t.Run(transport+map[bool]string{false: "/correct", true: "/mismatch"}[mismatch], func(t *testing.T) {
				f := newFixture(t)
				server, address, _ := f.start("127.0.0.1:0")
				cert := certificate(t)
				g := f.grant(key.NewNode().Public(), cert, "endpoint")
				// Client identity and grant authorization remain mandatory after pinning.
				otherCert := certificate(t)
				other := f.grant(key.NewNode().Public(), otherCert, "other")
				pairScopes(&g, &other)
				digest := sha256.Sum256(f.tls.Certificates[0].Leaf.RawSubjectPublicKeyInfo)
				if mismatch {
					digest[0] ^= 1
				}
				config, err := nodetls.ClientConfig(&tls.Config{Certificates: []tls.Certificate{cert}, ServerName: "127.0.0.1"}, base64.RawURLEncoding.EncodeToString(digest[:]))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if transport == "quic" {
					client := NewClient(ClientConfig{Address: address, TLS: config, Credential: func(context.Context) (string, error) { return f.token(g), nil }})
					defer client.Close()
					err = client.Connect(ctx)
				} else {
					client, _ := f.wss(server, cert, g, nil)
					client.config.TLS = config
					err = client.Connect(ctx)
				}
				if (err == nil) == mismatch {
					t.Fatalf("connect error = %v", err)
				}
			})
		}
	}
}
