package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"go.uber.org/goleak"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// Keep real sockets and their deadline/close behavior while blocking direct data.
type regionalDirectBlock struct{ directFilter }

type regionalRelay struct {
	node tailnet.RegionalNode
	addr string
	stop func()
}

func TestNativeRegionalFailoverPreservesSession(t *testing.T) {
	if testing.Short() {
		t.Skip("real two-node DERP/QUIC integration")
	}
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 80*time.Second)
	defer cancel()

	pub, signer, _ := ed25519.GenerateKey(nil)
	clientTLS, clientFP := testTLS(t, "regional-client")
	serverTLS, serverFP := testTLS(t, "regional-server")
	clientBinding := tailnet.NetworkBinding{AccountID: "regional", EndpointID: "regional-client", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFP, VirtualAddress: "fd7a:115c:a1e0::41"}
	serverBinding := tailnet.NetworkBinding{AccountID: "regional", EndpointID: "regional-server", Role: "machine", MachineID: "regional-server", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFP, VirtualAddress: "fd7a:115c:a1e0::42"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": pub}, &regionalDirectBlock{})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": pub}, &regionalDirectBlock{})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	clientDisco, err := clientAuthority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	serverDisco, err := serverAuthority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	clientConfig := testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, clientAuthority, signer, clientConfig)
	applyTestConfiguration(t, serverAuthority, signer, serverConfig)

	relayTLS, _ := testTLS(t, "localhost")
	cert, _ := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	start := func(id, domain, addr string) *regionalRelay {
		pc, listenErr := net.ListenPacket("udp4", addr)
		if listenErr != nil {
			t.Fatal(listenErr)
		}
		epoch := "epoch-" + id
		r, newErr := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Keys: map[string]ed25519.PublicKey{"native_test": pub}})
		if newErr != nil {
			t.Fatal(newErr)
		}
		rctx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- r.Serve(rctx, pc, relayTLS) }()
		var stopped bool
		shutdown := func() {
			if stopped {
				return
			}
			stopped = true
			stop()
			_ = r.Close()
			_ = pc.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Error("relay did not stop")
			}
			r.Wait()
		}
		port := pc.LocalAddr().(*net.UDPAddr).Port
		return &regionalRelay{addr: fmt.Sprintf("127.0.0.1:%d", port), stop: shutdown, node: tailnet.RegionalNode{NodeID: id, NodeGeneration: 1, ProcessEpoch: epoch, Region: "test", FailureDomain: domain, Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(90 * time.Second).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}}
	}
	a, b := start("regional-a", "rack-a", "127.0.0.1:0"), start("regional-b", "rack-b", "127.0.0.1:0")
	defer func() { a.stop(); b.stop() }()
	nodes := []tailnet.RegionalNode{a.node, b.node}
	applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, nodes)
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, nodes[1:])
	clientRegions, err := clientAuthority.ConfigureRegionalRelays(regionalTLS(clientTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	serverRegions, err := serverAuthority.ConfigureRegionalRelays(regionalTLS(serverTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientRegions) != 2 || len(serverRegions) != 1 {
		t.Fatalf("regional shortlist=%d/%d", len(clientRegions), len(serverRegions))
	}

	clientOwner, _ := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	defer clientOwner.Close()
	serverOwner, _ := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	defer serverOwner.Close()
	refresh := func() {
		clientConfig.Generation++
		serverConfig.Generation++
		clientConfig.IssuedAt = time.Now().Unix()
		serverConfig.IssuedAt = clientConfig.IssuedAt
		applyTestConfiguration(t, clientAuthority, signer, clientConfig)
		applyTestConfiguration(t, serverAuthority, signer, serverConfig)
		tick := time.Now()
		fresh := append([]tailnet.RegionalNode(nil), nodes...)
		for i := range fresh {
			fresh[i].ObservedAt = tick.Unix()
			fresh[i].CapacityObservedAt = tick.Unix()
			fresh[i].ExpiresAt = tick.Add(60 * time.Second).Unix()
		}
		applyRegionalAuthority(t, clientAuthority, signer, clientConfig, clientTLS, clientDisco, serverDisco, fresh)
		applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, fresh)
	}
	descriptor := startRelayEchoServer(t, serverOwner, serverAuthority, serverRegions[0])
	session, err := clientOwner.Dial(ctx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("initial regional dial: %v; client=%+v server=%+v", err, clientAuthority.RegionalStatus(), serverAuthority.RegionalStatus())
	}
	assertRegionalBytes(t, ctx, session, "before-failover")
	if got := clientAuthority.RegionalStatus(); got.NodeID != b.node.NodeID || got.Redundancy != tailnet.RedundancyReduced {
		t.Fatalf("asymmetric candidate intersection: %+v", got)
	}
	refresh()
	waitRegional := func(wantDifferent string, redundancy tailnet.Redundancy) tailnet.RegionalStatus {
		deadline := time.Now().Add(25 * time.Second)
		nextRefresh := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if time.Now().After(nextRefresh) {
				refresh()
				nextRefresh = time.Now().Add(5 * time.Second)
			}
			s := clientAuthority.RegionalStatus()
			if s.NodeID != "" && s.NodeID != wantDifferent && s.Redundancy == redundancy && (redundancy != tailnet.RedundancyAvailable || s.BackupNodeID != "") {
				return s
			}
			time.Sleep(100 * time.Millisecond)
		}
		stack := make([]byte, 1<<20)
		n := runtime.Stack(stack, true)
		for _, block := range strings.Split(string(stack[:n]), "\n\n") {
			if strings.Contains(block, "regionalRecovery") {
				t.Log(block)
			}
		}
		t.Fatalf("regional status did not converge: %+v", clientAuthority.RegionalStatus())
		return tailnet.RegionalStatus{}
	}
	initial := waitRegional("", tailnet.RedundancyAvailable)
	failed := a
	if initial.NodeID == b.node.NodeID {
		failed = b
	}
	failed.stop()
	failover := waitRegional(initial.NodeID, tailnet.RedundancyReduced)
	assertRegionalBytes(t, ctx, session, "after-failover")

	restored := start(failed.node.NodeID, failed.node.FailureDomain, failed.addr)
	if failed == a {
		a = restored
	} else {
		b = restored
	}
	for deadline := time.Now().Add(11 * time.Second); time.Now().Before(deadline); {
		time.Sleep(4 * time.Second)
		refresh()
	}
	afterRestore := clientAuthority.RegionalStatus()
	if afterRestore.NodeID != failover.NodeID {
		t.Fatalf("restored node caused rapid flap: before=%+v after=%+v", failover, afterRestore)
	}
	if afterRestore.Redundancy != tailnet.RedundancyAvailable || afterRestore.BackupNodeID == "" {
		t.Fatalf("restored failure-domain backup unavailable: %+v", afterRestore)
	}
	assertRegionalBytes(t, ctx, session, "after-restore")
	// Stop control refresh. Cached signed health expires without widening scope.
	expiryDeadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(expiryDeadline) && clientAuthority.RegionalStatus().Reason != "no_authorized_common_node" {
		time.Sleep(100 * time.Millisecond)
	}
	if clientAuthority.RegionalStatus().Reason != "no_authorized_common_node" {
		t.Fatalf("expired control authority remains usable: %+v", clientAuthority.RegionalStatus())
	}
	h, _ := streamauth.New("regional-expired", "terminal", "regional-expired-stream", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	expired, expiredErr := session.OpenAuthorized(ctx, h, "grant_test", "terminal")
	if expiredErr == nil {
		_ = expired.SetDeadline(time.Now().Add(time.Second))
		_, writeErr := expired.Write([]byte("must-not-forward"))
		got := make([]byte, 16)
		_, readErr := io.ReadFull(expired, got)
		_ = expired.Close()
		if writeErr == nil && readErr == nil {
			t.Fatal("regional data forwarded after signed node authority expired")
		}
	}
	refresh()
	waitRegional("", tailnet.RedundancyAvailable)
	assertRegionalBytes(t, ctx, session, "after-control-recovery")
}

func regionalTLS(endpoint *tls.Config, roots *x509.CertPool) *tls.Config {
	c := endpoint.Clone()
	c.RootCAs, c.ServerName, c.InsecureSkipVerify = roots, "localhost", false
	return c
}

func applyRegionalAuthority(t *testing.T, authority *tailnet.Authority, signer ed25519.PrivateKey, config tailnet.NetworkConfiguration, endpointTLS *tls.Config, selfDisco, peerDisco string, nodes []tailnet.RegionalNode) {
	t.Helper()
	now := time.Now().Unix()
	expires := min(now+60, config.ExpiresAt)
	for _, n := range nodes {
		expires = min(expires, n.ExpiresAt)
	}
	candidates := tailnet.RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: config.Issuer, Audience: "paperboat-regional-candidates", AccountID: config.Self.AccountID, EndpointID: config.Self.EndpointID, AuthorizationGeneration: config.Generation, Generation: config.Generation, IssuedAt: now, ExpiresAt: expires, Nodes: nodes}
	if err := authority.ApplyRegionalCandidates(t.Context(), signedRelayTestJWT(t, signer, "paperboat-regional-candidates+jwt", candidates)); err != nil {
		t.Fatal(err)
	}
	scopes := make([]derpquic.Scope, len(config.Peers[0].Scopes))
	for i, s := range config.Peers[0].Scopes {
		scopes[i] = derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt}
	}
	fingerprint := sha256.Sum256(endpointTLS.Certificates[0].Certificate[0])
	tokens := make([]string, 0, len(nodes))
	for _, n := range nodes {
		g := derpquic.Grant{Version: 1, Issuer: config.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: expires, Generation: config.Generation, AccountID: config.Self.AccountID, EndpointID: config.Self.EndpointID, WireGuardPublicKey: config.Self.WireGuardPublicKey, DiscoPublicKey: selfDisco, CertificateFingerprint: base16(fingerprint[:]), QUICPublicKey: config.Self.QUICPublicKey, NodeID: n.NodeID, NodeGeneration: n.NodeGeneration, ProcessEpoch: n.ProcessEpoch, Peers: []derpquic.Peer{{WireGuardPublicKey: config.Peers[0].Identity.WireGuardPublicKey, DiscoPublicKey: peerDisco, Scopes: scopes}}}
		tokens = append(tokens, signedRelayTestJWT(t, signer, "paperboat-relay-grant+jwt", g))
	}
	if err := authority.ApplyRelayGrants(t.Context(), tokens); err != nil {
		t.Fatal(err)
	}
}

func assertRegionalBytes(t *testing.T, ctx context.Context, session *native.Session, value string) {
	t.Helper()
	h, _ := streamauth.New("regional-op", "terminal", "regional-stream", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	s, err := session.OpenAuthorized(ctx, h, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = s.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(value))
	if _, err = io.ReadFull(s, got); err != nil || string(got) != value {
		t.Fatalf("regional bytes=%q err=%v", got, err)
	}
}
