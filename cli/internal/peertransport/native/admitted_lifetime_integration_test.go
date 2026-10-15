package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=admitted-native-lifetime-regression
	"tailscale.com/tstest/integration"
)

func TestAdmittedNativeStreamsOutliveCredentialsAndCloseOnRevocation(t *testing.T) {
	for _, personal := range []bool{true, false} {
		t.Run(map[bool]string{true: "personal", false: "cross_account"}[personal], func(t *testing.T) { testAdmittedNativeLifetime(t, personal) })
	}
}

func testAdmittedNativeLifetime(t *testing.T, personal bool) {
	t.Setenv("IN_TS_TEST", "true")
	dm := integration.RunDERPAndSTUN(t, func(string, ...any) {}, "127.0.0.1")
	pub, signer, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFP := testTLS(t, "cli")
	serverTLS, serverFP := testTLS(t, "machine")
	cb := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::1"}
	sb := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::2"}
	if !personal {
		sb.AccountID = "account_other"
	}
	ca := testAuthority(t, cb, testKeys{"native_test": pub})
	sa := testAuthority(t, sb, testKeys{"native_test": pub})
	cb.WireGuardPublicKey, cb.KeyGeneration = prepareBinding(t, ca)
	sb.WireGuardPublicKey, sb.KeyGeneration = prepareBinding(t, sa)
	now := time.Now().Unix()
	cc := testConfiguration(now, 1, cb, sb, "dial")
	sc := testConfiguration(now, 1, sb, cb, "accept")
	// Both the snapshot and individual operation scopes expire during active I/O.
	expires := now + 4
	for _, cfg := range []*tailnet.NetworkConfiguration{&cc, &sc} {
		cfg.ExpiresAt = expires
		if personal {
			cfg.Peers[0].Scopes = append(cfg.Peers[0].Scopes, tailnet.NetworkScope{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: cfg.Peers[0].Scopes[0].Direction, Port: tailnet.NetworkPort})
		}
		cfg.Peers[0].Scopes = append(cfg.Peers[0].Scopes, tailnet.NetworkScope{ResourceKind: "config_compare", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "config_compare", Direction: cfg.Peers[0].Scopes[0].Direction, Port: tailnet.NetworkPort})
		cfg.Peers[0].Scopes = append(cfg.Peers[0].Scopes, tailnet.NetworkScope{ResourceKind: "inspector", ResourceID: "grant_test", ResourceGeneration: 1, Capability: "inspector", Direction: cfg.Peers[0].Scopes[0].Direction, Port: tailnet.NetworkPort})
		for i := range cfg.Peers[0].Scopes {
			cfg.Peers[0].Scopes[i].ExpiresAt = expires
		}
	}
	applyTestConfiguration(t, ca, signer, cc)
	applyTestConfiguration(t, sa, signer, sc)
	co, err := native.NewOwner(native.Config{Authority: ca, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer co.Close()
	so, err := native.NewOwner(native.Config{Authority: sa, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer so.Close()
	apps, err := nativesession.New(nativesession.Config{Authorize: func(context.Context, streamauth.Header) (string, error) { return "grant_test", nil }, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error { _, e := io.Copy(c, c); return e }, ServeTransfer: func(_ context.Context, c net.Conn) error { _, e := io.Copy(c, c); return e }})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := sa.Listen(dm.Regions[1])
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	done := make(chan struct{})
	go func() { defer close(done); _ = so.Listen(ctx, dm.Regions[1], apps.Serve) }()
	defer func() { cancel(); so.Close(); <-done; apps.Wait() }()
	session, err := co.Dial(ctx, listener.Address(), "machine_test", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, authority := range []*tailnet.Authority{ca, sa} {
		waitNativeScopeRefs(t, authority, 1)
	}
	paths := []struct{ consumer, capability string }{{"terminal", "terminal"}, {"exec", "exec"}, {"ssh", "managed_ssh"}, {"file_transfer", "file_transfer"}, {"private_tcp", "private_access"}, {"private_http", "private_access"}, {"config_compare", "config_compare"}, {"inspector", "inspector"}}
	streams := make([]net.Conn, len(paths))
	for i, p := range paths {
		h, e := streamauth.New("operation_"+p.consumer, p.consumer, "stream_"+p.consumer, "credential", time.Unix(expires, 0), 1<<20)
		if p.consumer == "private_tcp" || p.consumer == "private_http" || p.consumer == "inspector" {
			h, e = streamauth.NewNativePrivate("operation_"+p.consumer, p.consumer, "stream_"+p.consumer, "credential", time.Unix(expires, 0), 1<<20, []byte(`{}`))
		}
		if e != nil {
			t.Fatal(e)
		}
		streams[i], err = session.OpenAuthorized(ctx, h, "grant_test", p.capability)
		if err != nil {
			t.Fatal(err)
		}
		defer streams[i].Close()
		lifetimeEcho(t, streams[i], p.consumer)
	}
	for _, authority := range []*tailnet.Authority{ca, sa} {
		if refs := authority.AdmittedScopes(); len(refs) != map[bool]int{true: 4, false: 3}[personal] {
			t.Fatalf("live native lease refs=%d unexpected", len(refs))
		}
	}
	time.Sleep(time.Until(time.Unix(expires, 0)) + 30*time.Millisecond)
	for i, p := range paths {
		t.Run(p.consumer, func(t *testing.T) { lifetimeEcho(t, streams[i], p.consumer) })
	}
	// A current signed snapshot retains the original expiry for live scopes.
	// Refreshing network configuration must not fabricate a new credential TTL.
	for _, cfg := range []*tailnet.NetworkConfiguration{&cc, &sc} {
		cfg.Generation++
		cfg.IssuedAt = time.Now().Unix()
		cfg.ExpiresAt = cfg.IssuedAt + 60
	}
	applyTestConfiguration(t, ca, signer, cc)
	applyTestConfiguration(t, sa, signer, sc)
	for i, p := range paths {
		lifetimeEcho(t, streams[i], p.consumer)
	}
	// Expired credentials still cannot join, even though existing streams survive.
	h, _ := streamauth.New("new_operation", "exec", "new_stream", "fresh", time.Now().Add(time.Minute), 1<<20)
	if c, e := session.OpenAuthorized(ctx, h, "grant_test", "exec"); e == nil {
		c.Close()
		t.Fatal("expired scope admitted new work")
	}
	if !personal {
		// Resource-only carriers retain the actual signed backing authority even
		// after every application stream closes. A different remaining resource
		// must not hide withdrawal of the carrier's original authority.
		for _, stream := range streams {
			_ = stream.Close()
		}
		for _, authority := range []*tailnet.Authority{ca, sa} {
			waitNativeScopeRefs(t, authority, 1)
		}
		sc.Generation++
		sc.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "config_compare", ResourceID: "other_assignment", ResourceGeneration: 1, Capability: "config_compare", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: sc.ExpiresAt}}
		applyTestConfiguration(t, sa, signer, sc)
		for _, authority := range []*tailnet.Authority{ca, sa} {
			waitNativeScopeRefs(t, authority, 0)
		}
		return
	}
	// A newer signed snapshot explicitly withdraws the resource.
	sc.Generation++
	sc.IssuedAt = time.Now().Unix()
	sc.ExpiresAt = sc.IssuedAt + 60
	sc.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_pair", ResourceGeneration: 1, Capability: "connect", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: sc.ExpiresAt}}
	applyTestConfiguration(t, sa, signer, sc)
	for i, p := range paths {
		streams[i].SetReadDeadline(time.Now().Add(time.Second))
		var b [1]byte
		if _, e := streams[i].Read(b[:]); e == nil {
			t.Fatalf("%s survived revocation", p.consumer)
		} else if n, ok := e.(net.Error); ok && n.Timeout() {
			t.Fatalf("%s revocation did not close blocked I/O", p.consumer)
		}
		_ = streams[i].Close()
	}
	// Idle carriers retain their signed network scope until disconnect.
	for _, authority := range []*tailnet.Authority{ca, sa} {
		waitNativeScopeRefs(t, authority, 1)
	}
	_ = session.Close()
	for _, authority := range []*tailnet.Authority{ca, sa} {
		limit := time.NewTimer(time.Second)
		for len(authority.AdmittedScopes()) != 0 {
			select {
			case <-limit.C:
				t.Fatal("closed native leases retained historical scope refs")
			case <-time.After(time.Millisecond):
			}
		}
		limit.Stop()
	}
}

func lifetimeEcho(t *testing.T, c net.Conn, path string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte{42}); err != nil {
		t.Fatalf("%s admitted write: %v", path, err)
	}
	var b [1]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || b[0] != 42 {
		t.Fatalf("%s admitted read: %v", path, err)
	}
	c.SetDeadline(time.Time{})
}

func waitNativeScopeRefs(t *testing.T, authority *tailnet.Authority, want int) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(authority.AdmittedScopes()) != want {
		select {
		case <-deadline.C:
			t.Fatalf("native scope refs=%d want=%d", len(authority.AdmittedScopes()), want)
		case <-time.After(time.Millisecond):
		}
	}
}
