package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/derpquic"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

type connectedNetworkKeys struct{ public ed25519.PublicKey }

func (k connectedNetworkKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return k.public, true, nil
}

type connectedNetworkAPI struct {
	register func(api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error)
	config   func() (api.PeerNetworkConfigurationResult, error)
}

type connectedRoundTripper func(*http.Request) (*http.Response, error)

func (f connectedRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (a connectedNetworkAPI) RegisterPeerNetwork(_ context.Context, request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
	return a.register(request)
}
func (a connectedNetworkAPI) PeerNetworkConfiguration(context.Context, string) (api.PeerNetworkConfigurationResult, error) {
	return a.config()
}

func connectedSignedJWT(t *testing.T, private ed25519.PrivateKey, typ string, value any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": typ, "kid": "connected_key"})
	body, _ := json.Marshal(value)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, []byte(unsigned)))
}

func connectedEndpointAuthority(t *testing.T, store config.ProfileStore, issuer, accountID, cliID, machineID string) (clientauthority.Authority, tls.Certificate, tls.Certificate) {
	t.Helper()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerAccountRootPublic(issuer, accountID, rootPublic); err != nil {
		t.Fatal(err)
	}
	cliKeys, err := store.PeerEndpointKeys(issuer, accountID, cliID)
	if err != nil {
		t.Fatal(err)
	}
	machineKeys, err := store.PeerEndpointKeys(issuer, accountID, machineID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	cliCertificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleCLI, EndpointID: cliID, NoisePublicKey: cliKeys.NoisePublic, QUICPublicKey: cliKeys.QUICPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	machineCertificate, err := endpointidentity.Sign(rootPrivate, endpointidentity.Claims{AccountID: accountID, Role: endpointidentity.RoleMachine, EndpointID: machineID, NoisePublicKey: machineKeys.NoisePublic, QUICPublicKey: machineKeys.QUICPrivate.Public().(ed25519.PublicKey), Generation: 1, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	cliRaw, _ := cliCertificate.MarshalBinary()
	machineRaw, _ := machineCertificate.MarshalBinary()
	if _, err := store.SavePeerCertificate(issuer, cliID, cliRaw); err != nil {
		t.Fatal(err)
	}
	cliTLS, err := endpointidentity.NewTLSCertificate(cliCertificate, rootPublic, cliKeys.QUICPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	machineTLS, err := endpointidentity.NewTLSCertificate(machineCertificate, rootPublic, machineKeys.QUICPrivate, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rootFingerprint := sha256.Sum256(rootPublic)
	trusted := []endpointidentity.TrustedKey{{KeyID: "aek_" + hex.EncodeToString(rootFingerprint[:]), PublicKey: rootPublic, Fingerprint: rootFingerprint, Generation: 1}}
	return clientauthority.Authority{RootPublic: rootPublic, TrustedKeys: trusted, LocalKeys: cliKeys, LocalCertificate: cliCertificate, LocalCertificateRaw: cliRaw, MachineCertificate: machineCertificate, MachineCertificateKeyID: trusted[0].KeyID, MachineCertificateRaw: machineRaw}, cliTLS, machineTLS
}

func connectedStore(t *testing.T) config.ProfileStore {
	t.Helper()
	root := t.TempDir()
	return config.ProfileStore{Path: filepath.Join(root, "profiles.json"), Secrets: config.FileSecretStore{Dir: filepath.Join(root, "secrets")}}
}

type connectedConfigState struct {
	mu         sync.Mutex
	client     tailnet.NetworkBinding
	clientNet  tailnet.NetworkConfiguration
	machineNet tailnet.NetworkConfiguration
	network    string
	candidate  string
	grants     []string
}

func connectedServerTLS(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{raw}, PrivateKey: private}
	parsed, _ := x509.ParseCertificate(raw)
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return certificate, roots
}

func connectedConfiguration(now int64, issuer string, self, peer tailnet.NetworkBinding, direction string) tailnet.NetworkConfiguration {
	return tailnet.NetworkConfiguration{Version: 1, Issuer: issuer, Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: 1, Self: self, Peers: []tailnet.NetworkPeer{{Identity: peer, Scopes: []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "access_connected", ResourceGeneration: 1, Capability: "terminal", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: now + 300}}}}}
}

func connectedRegionalTokens(t *testing.T, signer ed25519.PrivateKey, configuration tailnet.NetworkConfiguration, node tailnet.RegionalNode) (string, string) {
	t.Helper()
	now := time.Now().Unix()
	expires := now + 60
	candidates := tailnet.RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: configuration.Issuer, Audience: "paperboat-regional-candidates", AccountID: configuration.Self.AccountID, EndpointID: configuration.Self.EndpointID, AuthorizationGeneration: configuration.Generation, Generation: configuration.Generation, IssuedAt: now, ExpiresAt: expires, Nodes: []tailnet.RegionalNode{node}}
	grant := derpquic.Grant{Version: 1, Issuer: configuration.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: expires, Generation: configuration.Generation, AccountID: configuration.Self.AccountID, EndpointID: configuration.Self.EndpointID, WireGuardPublicKey: configuration.Self.WireGuardPublicKey, DiscoPublicKey: configuration.Self.DiscoPublicKey, CertificateFingerprint: configuration.Self.QUICCertificateFingerprint, QUICPublicKey: configuration.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch}
	for _, peer := range configuration.Peers {
		entry := derpquic.Peer{WireGuardPublicKey: peer.Identity.WireGuardPublicKey, DiscoPublicKey: peer.Identity.DiscoPublicKey}
		for _, scope := range peer.Scopes {
			entry.Scopes = append(entry.Scopes, derpquic.Scope{ResourceKind: scope.ResourceKind, ResourceID: scope.ResourceID, ResourceGeneration: scope.ResourceGeneration, Capability: scope.Capability, Direction: scope.Direction, Port: scope.Port, ExpiresAt: scope.ExpiresAt})
		}
		grant.Peers = append(grant.Peers, entry)
	}
	return connectedSignedJWT(t, signer, "paperboat-regional-candidates+jwt", candidates), connectedSignedJWT(t, signer, "paperboat-relay-grant+jwt", grant)
}

func connectedApply(t *testing.T, authority *tailnet.Authority, signer ed25519.PrivateKey, configuration tailnet.NetworkConfiguration, node tailnet.RegionalNode) {
	t.Helper()
	if err := authority.Apply(t.Context(), connectedSignedJWT(t, signer, "paperboat-network-config+jwt", configuration)); err != nil {
		t.Fatal(err)
	}
	candidates, grant := connectedRegionalTokens(t, signer, configuration, node)
	if err := authority.ApplyRegionalCandidates(t.Context(), candidates); err != nil {
		t.Fatal(err)
	}
	if err := authority.ApplyRelayGrants(t.Context(), []string{grant}); err != nil {
		t.Fatal(err)
	}
}

type connectedRawConn struct{ net.Conn }

func (c *connectedRawConn) Resize(uint16, uint16) error { return nil }
func (c *connectedRawConn) Wait() (int, error)          { return 0, nil }

func TestProductionCLINativeRuntimeConnectsOrdinaryApplicationAndReusesOwner(t *testing.T) {
	for _, mode := range []string{"quic", "wss_fallback", "quic_sender_wss_receiver", "receiver_authority_refresh"} {
		t.Run(mode, func(t *testing.T) { connectedProductionRuntime(t, mode) })
	}
}

func connectedProductionRuntime(t *testing.T, mode string) {
	forcedWSS := mode == "wss_fallback"
	mixed := mode == "quic_sender_wss_receiver"
	refresh := mode == "receiver_authority_refresh"
	if testing.Short() {
		t.Skip("real authenticated DERP/QUIC production-constructor integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	issuer := "https://api.example.test"
	accountID, cliID, machineID := "account_connected", "cli_connected", "machine_connected"
	store := connectedStore(t)
	identity, cliLeaf, machineLeaf := connectedEndpointAuthority(t, store, issuer, accountID, cliID, machineID)
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	relayCertificate, relayRoots := connectedServerTLS(t)
	relaySocket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: issuer, NodeID: "relay_connected", NodeGeneration: 1, ProcessEpoch: "epoch_connected", Keys: map[string]ed25519.PublicKey{"connected_key": signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	// In the fallback case this bound UDP socket deliberately never responds:
	// QUIC cannot succeed, while the same authenticated relay serves WSS over TLS.
	defer relaySocket.Close()
	defer relay.Wait()
	defer relay.Close()
	var wssRequests, machineWSS, clientQUIC atomic.Int32
	var tcpPort uint16
	if forcedWSS || mixed {
		handler := relay.WSSHandler()
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wssRequests.Add(1)
			if r.TLS != nil && len(r.TLS.PeerCertificates) == 1 && bytes.Equal(r.TLS.PeerCertificates[0].Raw, machineLeaf.Certificate[0]) {
				machineWSS.Add(1)
			}
			handler.ServeHTTP(w, r)
		}))
		server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{relayCertificate}, ClientAuth: tls.RequestClientCert}
		server.StartTLS()
		defer server.Close()
		tcpPort = uint16(server.Listener.Addr().(*net.TCPAddr).Port)
	}
	if !forcedWSS {
		relayCtx, stopRelay := context.WithCancel(ctx)
		relayDone := make(chan error, 1)
		go func() {
			relayDone <- relay.Serve(relayCtx, relaySocket, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{relayCertificate}, VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 1 && bytes.Equal(state.PeerCertificates[0].PublicKey.(ed25519.PublicKey), identity.LocalCertificate.Claims.QUICPublicKey) {
					clientQUIC.Add(1)
				}
				return nil
			}})
		}()
		defer func() { stopRelay(); _ = relay.Close(); _ = relaySocket.Close(); <-relayDone }()
	}
	now := time.Now().Unix()
	port := relaySocket.LocalAddr().(*net.UDPAddr).Port
	node := tailnet.RegionalNode{NodeID: "relay_connected", NodeGeneration: 1, ProcessEpoch: "epoch_connected", Region: "test", FailureDomain: "local", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(port), State: "ready", ObservedAt: now, ExpiresAt: now + 90, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}
	if forcedWSS {
		node.Transports = append(node.Transports, "derp_wss")
		node.EndpointTCPPort = tcpPort
	}
	machineNode := node
	if mixed {
		blackhole, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer blackhole.Close()
		machineNode.EndpointQUICPort = uint16(blackhole.LocalAddr().(*net.UDPAddr).Port)
		machineNode.EndpointTCPPort = tcpPort
		machineNode.Transports = []string{"derp_quic", "derp_wss"}
	}
	machineBinding := tailnet.NetworkBinding{AccountID: accountID, EndpointID: machineID, Role: "machine", MachineID: machineID, EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: identity.MachineCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.MachineCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::92"}
	machineAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: machineBinding, Keys: connectedNetworkKeys{signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer machineAuthority.Close()
	machineBinding.WireGuardPublicKey, machineBinding.KeyGeneration, err = machineAuthority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	machineBinding.KeyGeneration++
	machineBinding.DiscoPublicKey, _ = machineAuthority.DiscoveryPublicKey()
	state := &connectedConfigState{}
	networkAPI := connectedNetworkAPI{register: func(request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		state.mu.Lock()
		defer state.mu.Unlock()
		client := tailnet.NetworkBinding{AccountID: accountID, EndpointID: cliID, Role: "cli", EndpointGeneration: 1, KeyGeneration: request.ExpectedKeyGeneration + 1, WireGuardPublicKey: request.WireGuardPublicKey, DiscoPublicKey: request.DiscoPublicKey, QUICCertificateFingerprint: request.QUICCertificateFingerprint, QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::91"}
		clientConfig := connectedConfiguration(now, issuer, client, machineBinding, "dial")
		machineConfig := connectedConfiguration(now, issuer, machineBinding, client, "accept")
		initialMachine := machineConfig
		if refresh {
			initialMachine.Peers = nil
		}
		connectedApply(t, machineAuthority, signerPrivate, initialMachine, machineNode)
		state.client = client
		state.clientNet = clientConfig
		state.machineNet = machineConfig
		state.network = connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", clientConfig)
		var grant string
		state.candidate, grant = connectedRegionalTokens(t, signerPrivate, clientConfig, node)
		state.grants = []string{grant}
		return api.PeerNetworkRegistrationResult{KeyGeneration: client.KeyGeneration, VirtualAddress: client.VirtualAddress}, nil
	}, config: func() (api.PeerNetworkConfigurationResult, error) {
		state.mu.Lock()
		defer state.mu.Unlock()
		return api.PeerNetworkConfigurationResult{Configuration: state.network, CandidateSet: state.candidate, RelayGrants: state.grants}, nil
	}}
	clientHTTP := &http.Client{Transport: connectedRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/jwks.json" {
			return nil, errors.New("unexpected HTTP request")
		}
		body := fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"connected_key","x":"%s"}]}`, base64.RawURLEncoding.EncodeToString(signerPublic))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	relayTrust := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: relayRoots, ServerName: "localhost", Certificates: []tls.Certificate{cliLeaf}}
	runtime, err := newCLINativeRuntime(ctx, issuer, store, networkAPI, clientHTTP, relayTrust, accountID, cliID, identity)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	machineRelayTLS := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: relayRoots, ServerName: "localhost", Certificates: []tls.Certificate{machineLeaf}}
	regions, err := machineAuthority.ConfigureRegionalRelays(machineRelayTLS)
	if err != nil {
		t.Fatal(err)
	}
	machineOwner, err := native.NewOwner(native.Config{Authority: machineAuthority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{machineLeaf}}}) //nolint:gosec
	if err != nil {
		t.Fatal(err)
	}
	defer machineOwner.Close()
	if _, err = machineAuthority.Listen(regions[0]); err != nil {
		t.Fatal(err)
	}
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- machineOwner.Listen(ctx, regions[0], func(serveCtx context.Context, session *native.Session) error {
			for {
				stream, _, acceptErr := session.AcceptAuthorized(serveCtx, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.Consumer != "ssh" || header.Credential != "credential_ssh" {
						return "", errors.New("credential rejected")
					}
					return "access_connected", nil
				})
				if acceptErr != nil {
					return acceptErr
				}
				go func(connection net.Conn) { defer connection.Close(); _, _ = io.Copy(connection, connection) }(stream)
			}
		})
	}()
	if refresh {
		// Prove that the existing receiver registered its initial no-peer grant.
		deadline := time.NewTimer(5 * time.Second)
		ticker := time.NewTicker(10 * time.Millisecond)
		for relay.Snapshot().Accepted == 0 {
			select {
			case <-deadline.C:
				t.Fatal("initial no-peer receiver did not connect")
			case <-ticker.C:
			}
		}
		deadline.Stop()
		ticker.Stop()
		if err := relay.Revoke(accountID, machineID, 2); err != nil {
			t.Fatal(err)
		}
		oldConfig := state.machineNet
		oldConfig.Peers = nil
		_, oldGrant := connectedRegionalTokens(t, signerPrivate, oldConfig, machineNode)
		stale := derpquic.NewClient(derpquic.ClientConfig{Address: relaySocket.LocalAddr().String(), TLS: machineRelayTLS, Credential: func(context.Context) (string, error) { return oldGrant, nil }})
		staleCtx, cancelStale := context.WithTimeout(ctx, 5*time.Second)
		staleErr := stale.Connect(staleCtx)
		cancelStale()
		_ = stale.Close()
		var fatal interface{ Fatal() bool }
		if !errors.As(staleErr, &fatal) || !fatal.Fatal() {
			t.Fatalf("revoked grant not rejected fatally: %v", staleErr)
		}
		// Observe fail-closed retirement before making a genuinely newer grant
		// available; the same owner and endpoint keys must recover afterwards.
		deadline = time.NewTimer(15 * time.Second)
		ticker = time.NewTicker(10 * time.Millisecond)
	observation:
		for machineAuthority.RegionalStatus().Reason != "regional_admission_denied" {
			select {
			case <-deadline.C:
				t.Logf("receiver revocation status: %+v; relay: %+v", machineAuthority.RegionalStatus(), relay.Snapshot())
				break observation
			case <-ticker.C:
			}
		}
		deadline.Stop()
		ticker.Stop()
		state.mu.Lock()
		revokedStats := relay.Snapshot()
		if revokedStats.Denied == 0 || revokedStats.Connections != 0 {
			t.Fatalf("old receiver authority not fenced: %+v", revokedStats)
		}
		node.ObservedAt, node.CapacityObservedAt = time.Now().Unix(), time.Now().Unix()
		machineNode.ObservedAt, machineNode.CapacityObservedAt = node.ObservedAt, node.CapacityObservedAt
		state.clientNet.Generation, state.machineNet.Generation = 3, 3
		clientConfig, machineConfig := state.clientNet, state.machineNet
		state.network = connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", clientConfig)
		var grant string
		state.candidate, grant = connectedRegionalTokens(t, signerPrivate, clientConfig, node)
		state.grants = []string{grant}
		state.mu.Unlock()
		connectedApply(t, machineAuthority, signerPrivate, machineConfig, machineNode)
		connectedApply(t, runtime.authority, signerPrivate, clientConfig, node)
	}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	target := &resolver.TerminalTarget{EnvironmentID: "env_connected", Auth: resolver.AuthTarget{Token: "credential_ssh", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "access_connected"}}
	application := peerApplication{operationID: "operation_connected", stream: "ssh", raw: func(_ context.Context, stream io.ReadWriteCloser) (Conn, error) {
		return &connectedRawConn{Conn: stream.(net.Conn)}, nil
	}}
	for _, payload := range []string{"first-production-open", "second-production-open"} {
		openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
		connection, openErr := runtime.openApplication(openCtx, machineID, "ssh", application, target, time.Now)
		cancelOpen()
		if openErr != nil {
			select {
			case listenErr := <-listenDone:
				t.Fatalf("open application: %v; listener: %v; client regional: %+v; machine regional: %+v", openErr, listenErr, runtime.authority.RegionalStatus(), machineAuthority.RegionalStatus())
			default:
				t.Fatalf("open application: %v; client regional: %+v; machine regional: %+v", openErr, runtime.authority.RegionalStatus(), machineAuthority.RegionalStatus())
			}
		}
		_ = connection.(*connectedRawConn).SetDeadline(time.Now().Add(5 * time.Second))
		if _, openErr = connection.Write([]byte(payload)); openErr != nil {
			t.Fatal(openErr)
		}
		got := make([]byte, len(payload))
		if _, openErr = io.ReadFull(connection, got); openErr != nil || string(got) != payload {
			t.Fatalf("payload=%q err=%v", got, openErr)
		}
		_ = connection.Close()
	}
	if mixed && (clientQUIC.Load() == 0 || machineWSS.Load() == 0 || wssRequests.Load() != machineWSS.Load()) {
		t.Fatalf("mixed paths not established: client QUIC=%d machine WSS=%d total WSS=%d", clientQUIC.Load(), machineWSS.Load(), wssRequests.Load())
	}
	if forcedWSS && wssRequests.Load() < 2 {
		t.Fatalf("WSS fallback did not connect both endpoints: requests=%d", wssRequests.Load())
	}
	state.mu.Lock()
	clientRevoked, machineRevoked := state.clientNet, state.machineNet
	state.mu.Unlock()
	revokedAt := time.Now().Unix()
	clientRevoked.Generation++
	clientRevoked.IssuedAt, clientRevoked.ExpiresAt, clientRevoked.Peers = revokedAt, revokedAt+300, nil
	machineRevoked.Generation++
	machineRevoked.IssuedAt, machineRevoked.ExpiresAt, machineRevoked.Peers = revokedAt, revokedAt+300, nil
	if err = runtime.authority.Apply(ctx, connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", clientRevoked)); err != nil {
		t.Fatal(err)
	}
	if err = machineAuthority.Apply(ctx, connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", machineRevoked)); err != nil {
		t.Fatal(err)
	}
	revokedCtx, cancelRevoked := context.WithTimeout(ctx, 2*time.Second)
	defer cancelRevoked()
	if connection, openErr := runtime.openApplication(revokedCtx, machineID, "ssh", application, target, time.Now); openErr == nil {
		_ = connection.Close()
		t.Fatal("revoked peer opened another application")
	}
	cancel()
	select {
	case <-listenDone:
	case <-time.After(2 * time.Second):
		t.Fatal("machine listener did not stop")
	}
}
