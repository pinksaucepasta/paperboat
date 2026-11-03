package derpquic

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"net/http"
	"net/url"
	"testing"

	"tailscale.com/types/key"
)

func TestRelayGenerationsKeepTheServingRootAndShareItsRevocation(t *testing.T) {
	f := newFixture(t)
	server, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	certA, certB := certificate(t), certificate(t)
	a, b := f.grant(ka, certA, "client"), f.grant(kb, certB, "machine")
	pairScopes(&a, &b)
	open := func(cert tls.Certificate, grant Grant, namespace [32]byte) *Client {
		client := NewClient(ClientConfig{Address: address, Namespace: namespace, GenerationDisco: key.NewDisco().Public(), TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{cert}}, Credential: func(context.Context) (string, error) { return f.token(grant), nil }})
		t.Cleanup(func() { client.Close() })
		connect(t, client)
		return client
	}
	oldA, oldB := open(certA, a, [32]byte{}), open(certB, b, [32]byte{})
	if err := oldA.Send(kb, []byte("before")); err != nil {
		t.Fatal(err)
	}
	receive(t, oldB, ka, []byte("before"))
	namespace := sha256.Sum256([]byte("machine-signed-generation-certificate"))
	newA, newB := open(certA, a, namespace), open(certB, b, namespace)
	for _, pair := range [][2]*Client{{oldA, oldB}, {newA, newB}} {
		if err := pair[0].Send(kb, []byte("independent")); err != nil {
			t.Fatal(err)
		}
		receive(t, pair[1], ka, []byte("independent"))
	}
	if len(server.AuthoritySubjects()) != 2 {
		t.Fatal("transport generations created additional authority identities")
	}
	// A generation without a matching receiver must never fall through to
	// the primary carrier merely because its root key is authorized.
	isolated := open(certA, a, sha256.Sum256([]byte("other-generation")))
	dropped := server.Snapshot().Dropped
	if err := isolated.SendControl(kb, []byte("isolated")); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return server.Snapshot().Dropped > dropped })
	if err := server.Revoke(b.AccountID, b.EndpointID, 2); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return server.Snapshot().Connections == 3 })
	if err := oldA.Send(kb, []byte("revoked")); err != nil {
		t.Fatal(err)
	}
	if err := newA.Send(kb, []byte("revoked")); err != nil {
		t.Fatal(err)
	}
}

// A blocked UDP path must retain the primary connection while the delegated
// generation uses the same relay authority over HTTPS instead.
func TestRelayGenerationMixedQUICWSS(t *testing.T) {
	f := newFixture(t)
	server, address, _ := f.start("127.0.0.1:0")
	ka, kb := key.NewNode().Public(), key.NewNode().Public()
	ca, cb := certificate(t), certificate(t)
	a, b := f.grant(ka, ca, "client"), f.grant(kb, cb, "machine")
	pairScopes(&a, &b)
	primaryA, primaryB := f.client(address, ca, a), f.client(address, cb, b)
	connect(t, primaryA)
	connect(t, primaryB)
	namespace := sha256.Sum256([]byte("delegated-certificate"))
	client := NewClient(ClientConfig{Address: address, Namespace: namespace, GenerationDisco: key.NewDisco().Public(), TLS: &tls.Config{RootCAs: f.roots, ServerName: "localhost", Certificates: []tls.Certificate{ca}}, Credential: func(context.Context) (string, error) { return f.token(a), nil }})
	t.Cleanup(func() { client.Close() })
	connect(t, client)
	proxy, connects := connectProxy(t)
	fallback, _ := f.wss(server, cb, b, func(*http.Request) (*url.URL, error) { return proxy, nil })
	fallback.config.Namespace = namespace
	fallback.config.GenerationDisco = key.NewDisco().Public()
	connectWSS(t, fallback)
	if connects.Load() != 1 {
		t.Fatal("generation did not use the HTTPS proxy")
	}
	if err := client.Send(kb, []byte("delegated")); err != nil {
		t.Fatal(err)
	}
	receiveWSS(t, fallback, ka, []byte("delegated"))
	if err := fallback.Send(ka, []byte("reply")); err != nil {
		t.Fatal(err)
	}
	receive(t, client, kb, []byte("reply"))
	if err := primaryA.Send(kb, []byte("serving")); err != nil {
		t.Fatal(err)
	}
	receive(t, primaryB, ka, []byte("serving"))
	if err := server.Revoke(b.AccountID, b.EndpointID, 2); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return server.Snapshot().Connections == 2 })
}
