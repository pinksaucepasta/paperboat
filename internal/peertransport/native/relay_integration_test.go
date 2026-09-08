package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/nativeprivate"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
)

func TestNativeSessionOverAuthenticatedDERPQUIC(t *testing.T) {
	t.Setenv("IN_TS_TEST", "true")
	t.Setenv("TS_DEBUG_ALWAYS_USE_DERP", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	signerPublic, signerPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS, clientFingerprint := testTLS(t, "relay-cli")
	serverTLS, serverFingerprint := testTLS(t, "relay-machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_relay", EndpointID: "cli_relay", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::21"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_relay", EndpointID: "machine_relay", Role: "machine", MachineID: "machine_relay", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::22"}
	clientAuthority := testAuthority(t, clientBinding, testKeys{"native_test": signerPublic})
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().UTC().Truncate(time.Second)
	clientConfig := testConfiguration(now.Unix(), 1, clientBinding, serverBinding, "dial")
	serverConfig := testConfiguration(now.Unix(), 1, serverBinding, clientBinding, "accept")
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
	const nodeID, processEpoch = "relay_test", "epoch_test"
	verifier := derpquic.Verifier{Issuer: "https://api.example.test", NodeID: nodeID, NodeGeneration: 1, ProcessEpoch: processEpoch, Keys: map[string]ed25519.PublicKey{"native_test": signerPublic}}
	relay, err := derpquic.NewServer(verifier)
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
	applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node)
	_ = applyConfiguredRelay(t, clientAuthority, clientTLS, relayRoots, node)
	applyRelayAuthority(t, serverAuthority, signerPrivate, serverConfig, serverTLS, node)
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
	descriptor := startRelayEchoServer(t, serverOwner, serverAuthority, serverRegion)
	session, err := clientOwner.Dial(ctx, descriptor, "machine_relay", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}

	header, err := streamauth.New("operation_relay", "file_transfer", "stream_relay", "credential_file_transfer", now.Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_test", "file_transfer")
	if err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, derpquic.MaxPacket*4)
	for i := range payload {
		payload[i] = byte(i)
	}
	if _, err = stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err = io.ReadFull(stream, received); err != nil || string(received) != string(payload) {
		t.Fatalf("relay application bytes differ: %v", err)
	}
	_ = stream.Close()
	if relay.Snapshot().Forwarded == 0 {
		t.Fatal("native session did not forward through authenticated DERP")
	}
	target, _ := json.Marshal(nativeprivate.Binding{Schema: nativeprivate.SchemaV1, ResourceKind: "tunnel", ResourceID: "tun_relay", ResourceGeneration: 1, RouteID: "route_relay", RouteGeneration: 1, TargetGeneration: 1, OwnerEndpointID: "machine_relay", Protocol: "tcp", TargetScheme: "tcp", TargetAddress: "127.0.0.1:22", ExpiresAt: now.Add(time.Minute)})
	privateHeader, _ := streamauth.NewNativePrivate("operation_private_relay", "private_tcp", "stream_private_relay", "credential_private_tcp", now.Add(time.Minute), 1<<20, target)
	privateStream, err := session.OpenAuthorized(ctx, privateHeader, "grant_test", "private_access")
	if err != nil {
		t.Fatal(err)
	}
	var privateReady [1]byte
	if _, err = io.ReadFull(privateStream, privateReady[:]); err != nil || privateReady[0] != 0 {
		t.Fatalf("private readiness=%v err=%v", privateReady, err)
	}
	if _, err = privateStream.Write([]byte("native-private-relay")); err != nil {
		t.Fatal(err)
	}
	privateReply := make([]byte, len("native-private-relay"))
	if _, err = io.ReadFull(privateStream, privateReply); err != nil || string(privateReply) != "native-private-relay" {
		t.Fatalf("private relay bytes=%q err=%v", privateReply, err)
	}
	_ = privateStream.Close()
	cancelled, cancelOpen := context.WithCancel(ctx)
	cancelOpen()
	if _, err := session.Open(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled relay open: %v", err)
	}

	clientConfig.Generation++
	clientConfig.Peers = nil
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	if err := clientAuthority.ApplyRelayGrants(ctx, nil); err != nil {
		t.Fatal(err)
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancelProbe()
	if probe, probeErr := session.Open(probeCtx); probeErr == nil {
		_ = probe.Close()
		t.Fatal("session survived scoped relay authority removal")
	}
	clientConfig.Generation++
	clientConfig.IssuedAt = time.Now().Unix()
	clientConfig.ExpiresAt = clientConfig.IssuedAt + 300
	clientConfig.Peers = testPeers(serverBinding, "dial", clientConfig.ExpiresAt)
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	applyRelayAuthority(t, clientAuthority, signerPrivate, clientConfig, clientTLS, node)
	reconnected, err := clientOwner.Dial(ctx, descriptor, "machine_relay", peerquic.ClassInteractive)
	if err != nil {
		t.Fatal(err)
	}
	reconnectHeader, _ := streamauth.New("operation_relay_reconnect", "terminal", "stream_relay_reconnect", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	reconnectStream, err := reconnected.OpenAuthorized(ctx, reconnectHeader, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reconnectStream.Write([]byte("relay-reconnected")); err != nil {
		t.Fatal(err)
	}
	reconnectReply := make([]byte, len("relay-reconnected"))
	if _, err = io.ReadFull(reconnectStream, reconnectReply); err != nil || string(reconnectReply) != "relay-reconnected" {
		t.Fatalf("relay reconnect bytes=%q err=%v", reconnectReply, err)
	}
	_ = reconnectStream.Close()
}

func startRelayEchoServer(t *testing.T, owner *native.Owner, authority *tailnet.Authority, region *tailcfg.DERPRegion) tailcat.Addr {
	t.Helper()
	server, err := authority.Listen(region)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		_ = owner.Listen(ctx, region, func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, header, acceptErr := session.AcceptAuthorized(serveCtx, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.Credential != "credential_"+header.Consumer {
						return "", errors.New("credential rejected")
					}
					return "grant_test", nil
				})
				if acceptErr != nil {
					return acceptErr
				}
				go func(connection net.Conn, header streamauth.Header) {
					defer connection.Close()
					if header.Consumer == "private_tcp" {
						_, _ = connection.Write([]byte{0})
					}
					_, _ = io.Copy(connection, connection)
				}(stream, header)
			}
		})
	}()
	return server.Address()
}

func applyRelayAuthority(t *testing.T, authority *tailnet.Authority, signer ed25519.PrivateKey, config tailnet.NetworkConfiguration, endpointTLS *tls.Config, node tailnet.RegionalNode, mutate ...func(*derpquic.Grant)) {
	t.Helper()
	now := time.Now().Unix()
	expires := min(now+60, node.ExpiresAt, config.ExpiresAt)
	candidates := tailnet.RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: config.Issuer, Audience: "paperboat-regional-candidates", AccountID: config.Self.AccountID, EndpointID: config.Self.EndpointID, AuthorizationGeneration: config.Generation, Generation: config.Generation, IssuedAt: now, ExpiresAt: expires, Nodes: []tailnet.RegionalNode{node}}
	if err := authority.ApplyRegionalCandidates(t.Context(), signedRelayTestJWT(t, signer, "paperboat-regional-candidates+jwt", candidates)); err != nil {
		t.Fatal(err)
	}
	scopes := make([]derpquic.Scope, len(config.Peers[0].Scopes))
	for i, scope := range config.Peers[0].Scopes {
		scopes[i] = derpquic.Scope{ResourceKind: scope.ResourceKind, ResourceID: scope.ResourceID, ResourceGeneration: scope.ResourceGeneration, Capability: scope.Capability, Direction: scope.Direction, Port: scope.Port, ExpiresAt: scope.ExpiresAt}
	}
	fingerprint := sha256.Sum256(endpointTLS.Certificates[0].Certificate[0])
	grant := derpquic.Grant{Version: 1, Issuer: config.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: expires, Generation: config.Generation, AccountID: config.Self.AccountID, EndpointID: config.Self.EndpointID, WireGuardPublicKey: config.Self.WireGuardPublicKey, CertificateFingerprint: base16(fingerprint[:]), QUICPublicKey: config.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Peers: []derpquic.Peer{{WireGuardPublicKey: config.Peers[0].Identity.WireGuardPublicKey, Scopes: scopes}}}
	if len(mutate) > 0 {
		mutate[0](&grant)
	}
	if err := authority.ApplyRelayGrants(t.Context(), []string{signedRelayTestJWT(t, signer, "paperboat-relay-grant+jwt", grant)}); err != nil {
		t.Fatal(err)
	}
}

func applyConfiguredRelay(t *testing.T, authority *tailnet.Authority, endpointTLS *tls.Config, roots *x509.CertPool, node tailnet.RegionalNode) *tailcfg.DERPRegion {
	t.Helper()
	config := endpointTLS.Clone()
	config.InsecureSkipVerify = false
	config.RootCAs = roots
	config.ServerName = "localhost"
	region, err := authority.ConfigureRelay(node.NodeID, config)
	if err != nil {
		t.Fatal(err)
	}
	return region
}

func signedRelayTestJWT(t *testing.T, signer ed25519.PrivateKey, typ string, claims any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": typ, "kid": "native_test"})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer, []byte(unsigned)))
}

func base16(value []byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(value)*2)
	for i, b := range value {
		encoded[i*2], encoded[i*2+1] = digits[b>>4], digits[b&15]
	}
	return string(encoded)
}
