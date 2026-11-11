package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
)

// F5-10: a team member's CLI (account_member) and the shared machine (account_host) hold only standing machine_access
// scopes for each other (no machine_network, no expiring session scope), and the relay is their only path. The relay
// grants carry those scopes stamped with the grant expiry (peernetwork.RelayGrantKeepsStanding), so the relay pairs them
// and a stream opens and echoes; with grants that omit the pair (the server before this change) the open fails.
func TestTeamMemberReachesTheSharedHostOverTheRelayWithStandingScopesOnly(t *testing.T) {
	for _, paired := range []bool{true, false} {
		name := map[bool]string{true: "relay grants carry the standing scopes", false: "relay grants omit them"}[paired]
		t.Run(name, func(t *testing.T) { teamStandingOverRelay(t, paired) })
	}
}

func teamStandingOverRelay(t *testing.T, paired bool) {
	t.Setenv("IN_TS_TEST", "true")
	t.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	signerPublic, signerPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "team-cli")
	serverTLS, serverFingerprint := testTLS(t, "team-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_member", EndpointID: "cli_member", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::31"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_host", EndpointID: "machine_shared", Role: "machine", MachineID: "machine_shared", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::32"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().UTC().Truncate(time.Second)
	// What the server's documents carry (teamHostPeers, offlineAccessPeers): resource = the host's endpoint, generation
	// = the deciding policy version, expires_at 0.
	standing := func(direction string) []tailnet.NetworkScope {
		var scopes []tailnet.NetworkScope
		for _, capability := range []string{"exec", "managed_ssh"} {
			scopes = append(scopes, tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "machine_shared", ResourceGeneration: 7, Capability: capability, Direction: direction, Port: tailnet.NetworkPort})
		}
		return scopes
	}
	clientConfig := testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept")
	clientConfig.Peers[0].Scopes, serverConfig.Peers[0].Scopes = standing("dial"), standing("accept")
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)

	relayTLS, _ := testTLS(t, "localhost")
	relayCertificate, err := x509.ParseCertificate(relayTLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	relayRoots := x509.NewCertPool()
	relayRoots.AddCert(relayCertificate)
	relaySocket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer relaySocket.Close()
	const nodeID, processEpoch = "relay_team", "epoch_team"
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: "https://api.example.test", NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: processEpoch, Keys: map[string]ed25519.PublicKey{"native_test": signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() { relayDone <- relay.Serve(relayCtx, relaySocket, relayTLS) }()
	t.Cleanup(func() {
		stopRelay()
		_ = relay.Close()
		select {
		case <-relayDone:
		case <-time.After(2 * time.Second):
			t.Error("DERP/QUIC relay did not stop")
		}
		relay.Wait()
	})
	port := relaySocket.LocalAddr().(*net.UDPAddr).Port
	node := tailnet.RegionalNode{NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: processEpoch, Region: "test", FailureDomain: "test-a", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now.Unix(), ExpiresAt: now.Add(time.Minute).Unix(), CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now.Unix()}
	// The grant states each standing scope with the grant's expiry; the unpaired variant names no peer at all.
	grantShape := func(g *peernetwork.RelayGrant) {
		if !paired {
			g.Peers = nil
			return
		}
		for i := range g.Peers[0].Scopes {
			g.Peers[0].Scopes[i].ExpiresAt = g.ExpiresAt
		}
	}
	applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node, grantShape)
	_ = applyConfiguredRelay(t, clientAuthority, clientTLS, relayRoots, node)
	applyRelayAuthority(t, serverAuthority, signerPrivate, serverConfig, serverTLS, node, grantShape)
	serverRegion := applyConfiguredRelay(t, serverAuthority, serverTLS, relayRoots, node)

	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	server, err := serverAuthority.Listen(serverRegion)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	go func() {
		_ = serverOwner.Listen(serveCtx, serverRegion, func(c context.Context, session *native.Session) error {
			for {
				stream, _, acceptErr := session.AcceptAuthorized(c, func(context.Context, streamauth.Header) (string, error) {
					return "operation_never_published", nil // admitted from the host's standing rule
				})
				if errors.Is(acceptErr, native.ErrStreamRejected) {
					continue
				}
				if acceptErr != nil {
					return acceptErr
				}
				go func() { defer stream.Close(); _, _ = io.Copy(stream, stream) }()
			}
		})
	}()

	attempt, stopAttempt := ctx, func() {}
	if !paired {
		attempt, stopAttempt = context.WithTimeout(ctx, 4*time.Second)
	}
	defer stopAttempt()
	open := func() error {
		session, err := clientOwner.Dial(attempt, server.Address(), "machine_shared", peerquic.ClassInteractive)
		if err != nil {
			return err
		}
		header, err := streamauth.New("operation_team", "exec", "stream_team", "credential", time.Now().Add(time.Minute), 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := session.OpenAuthorized(attempt, header, "machine_shared", "exec")
		if err != nil {
			return err
		}
		defer stream.Close()
		_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := stream.Write([]byte("ssh-to-the-shared-host")); err != nil {
			return err
		}
		reply := make([]byte, len("ssh-to-the-shared-host"))
		_, err = io.ReadFull(stream, reply)
		return err
	}
	err = open()
	switch {
	case paired && err != nil:
		t.Fatalf("member stream over the relay with standing scopes only: %v", err)
	case !paired && err == nil:
		t.Fatal("a stream opened although no relay grant paired the member with the host")
	}
	if paired && relay.Snapshot().Forwarded == 0 {
		t.Fatal("the stream did not go through the relay")
	}
}
