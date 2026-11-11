package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"go.uber.org/goleak"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// Keep real sockets and their deadline/close behavior while blocking direct data.
type regionalDirectBlock struct{ directFilter }

type regionalRelay struct {
	node tailnet.RegionalNode
	addr string
	stop func()
}

// regionalDisagreement bounds how long regional status may lag the relay that carries the session. The handshake
// reaches the server on b as soon as it probes there; recovery reports b only once its probes to the candidates the
// server does not hold have timed out (regionalProbeTimeout, 5 s). One second covers the exchange that follows.
const regionalDisagreement = 6 * time.Second

func TestNativeRegionalFailoverPreservesSession(t *testing.T) { testNativeRegionalFailover(t, false) }

// A control outage longer than the signed candidate set withdraws the relays; when Paperboat answers again they
// are selected and carry data within regionalRecoveryBound.
func TestNativeRegionalRecoversWhenControlReturnsAfterTheSetExpired(t *testing.T) {
	testNativeRegionalFailover(t, true)
}

// regionalRecoveryBound is one normal refresh (the tests renew every 5 s) and a few seconds.
const regionalRecoveryBound = 10 * time.Second

func testNativeRegionalFailover(t *testing.T, expiry bool) {
	if testing.Short() {
		t.Skip("real two-node DERP/QUIC integration")
	}
	leaks := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Second)
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
	applyRegionalAuthority(t, serverAuthority, signer, serverConfig, serverTLS, serverDisco, clientDisco, []tailnet.RegionalNode{b.node})
	clientRegions, err := clientAuthority.ConfigureRegionalRelays(regionalTLS(clientTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	serverRegions, err := serverAuthority.ConfigureRegionalRelays(regionalTLS(serverTLS, roots))
	if err != nil {
		t.Fatal(err)
	}
	if len(clientRegions) != len(nodes) || len(serverRegions) != 1 {
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
		// The server signs a fresh expiry with every configuration; a fixed one ended this long test at 300 s.
		for _, config := range []*tailnet.NetworkConfiguration{&clientConfig, &serverConfig} {
			config.ExpiresAt = config.IssuedAt + 300
			for i := range config.Peers[0].Scopes {
				config.Peers[0].Scopes[i].ExpiresAt = config.ExpiresAt
			}
		}
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
	// Establish before the 15-second candidate freshness window expires so
	// a failure reports the actual startup state, not later lease withdrawal.
	dialCtx, stopDial := context.WithTimeout(ctx, 10*time.Second)
	session, err := clientOwner.Dial(dialCtx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	stopDial()
	if err != nil {
		t.Fatalf("initial regional dial: %v; client=%+v server=%+v", err, clientAuthority.RegionalStatus(), serverAuthority.RegionalStatus())
	}
	assertRegionalBytes(t, ctx, session, "before-failover")
	// The dial can finish before recovery reports its choice: an unanswered handshake is also asked on the other
	// relays, and the answer's region (b) carries the session. Data and control may disagree only until recovery has
	// timed out the candidates the server lacks and reports b with reduced redundancy; that window is measured and
	// bounded, not waited out.
	carried := time.Now()
	for got := clientAuthority.RegionalStatus(); got.NodeID != b.node.NodeID || got.Redundancy != tailnet.RedundancyReduced; got = clientAuthority.RegionalStatus() {
		if window := time.Since(carried); window > regionalDisagreement {
			t.Fatalf("control still disagrees with the data path %v after the session was carried by b: %+v", window, got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("control agreed with the data path %v after the session was carried", time.Since(carried).Round(time.Millisecond))
	refresh()
	waitRegional := func(within time.Duration, wantDifferent string, redundancy tailnet.Redundancy) tailnet.RegionalStatus {
		deadline := time.Now().Add(within)
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
	initial := waitRegional(25*time.Second, "", tailnet.RedundancyAvailable)
	if !expiry {
		failed := a
		if initial.NodeID == b.node.NodeID {
			failed = b
		}
		failed.stop()
		failover := waitRegional(25*time.Second, initial.NodeID, tailnet.RedundancyReduced)
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
	}
	// Stop control refresh for longer than the 15 s observation window, inside the signed candidate set (60 s here;
	// relaylimits.MaxGrantLifetime in production): the relays stay selected and carry data while Paperboat is
	// unreachable (offline until revoked), instead of being withdrawn as their observations age.
	if expiry {
		// No refresh until the 60 s set (and with it the relay grants) ends: the relays are withdrawn.
		for deadline := time.Now().Add(90 * time.Second); time.Now().Before(deadline) && clientAuthority.RegionalStatus().Reason != "no_authorized_common_node"; {
			time.Sleep(100 * time.Millisecond)
		}
		if got := clientAuthority.RegionalStatus(); got.Reason != "no_authorized_common_node" {
			t.Fatalf("relays still usable after their signed set expired: %+v", got)
		}
		time.Sleep(10 * time.Second) // the recovery worker keeps probing the expired set; its failures must not slow recovery
		returned := time.Now()
		refresh()
		waitRegional(regionalRecoveryBound, "", tailnet.RedundancyAvailable)
		assertRegionalBytes(t, ctx, session, "after-set-expiry-recovery")
		t.Logf("relays re-selected and carrying data %s after control returned", time.Since(returned).Round(100*time.Millisecond))
		return
	}
	time.Sleep(20 * time.Second)
	if got := clientAuthority.RegionalStatus(); got.NodeID == "" || got.Reason == "no_authorized_common_node" {
		t.Fatalf("relays withdrawn %s into a control outage, inside their signed set: %+v", 20*time.Second, got)
	}
	assertRegionalBytes(t, ctx, session, "during-control-outage")
	refresh()
	waitRegional(25*time.Second, "", tailnet.RedundancyAvailable)
	assertRegionalBytes(t, ctx, session, "after-control-recovery")
}

func regionalTLS(endpoint *tls.Config, roots *x509.CertPool) *tls.Config {
	c := endpoint.Clone()
	c.RootCAs, c.ServerName, c.InsecureSkipVerify = roots, "localhost", false
	return c
}

func applyRegionalAuthority(t *testing.T, authority *tailnet.Authority, signer ed25519.PrivateKey, config tailnet.NetworkConfiguration, endpointTLS *tls.Config, selfDisco, peerDisco string, nodes []tailnet.RegionalNode) {
	t.Helper()
	// One authorization generation has one stable signed grant, including when
	// an independent carrier consumes the same cached server response later.
	now := config.IssuedAt
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
