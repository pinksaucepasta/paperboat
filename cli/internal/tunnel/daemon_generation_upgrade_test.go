package tunnel

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// cloneAuthority is what a second daemon process resolves for the same CLI session: the same certificate and keys,
// in its own memory (a generation's runtime clears its copy when it closes).
func cloneAuthority(a clientauthority.Authority) clientauthority.Authority {
	b := a
	b.RootPublic = bytes.Clone(a.RootPublic)
	b.TrustedKeys = nil
	for _, k := range a.TrustedKeys {
		k.PublicKey = bytes.Clone(k.PublicKey)
		b.TrustedKeys = append(b.TrustedKeys, k)
	}
	b.LocalKeys.RootPrivate = bytes.Clone(a.LocalKeys.RootPrivate)
	b.LocalKeys.QUICPrivate = bytes.Clone(a.LocalKeys.QUICPrivate)
	b.LocalCertificateRaw = bytes.Clone(a.LocalCertificateRaw)
	b.MachineCertificateRaw = bytes.Clone(a.MachineCertificateRaw)
	return b
}

// upgradeNetwork is the control plane of one account during a daemon upgrade: every registered daemon generation of
// the session is a separate peer of the machine, with the session's grant (peersessions.ownerEndpoints).
type upgradeNetwork struct {
	t                *testing.T
	mu               sync.Mutex
	issuer, account  string
	signer           ed25519.PrivateKey
	node             tailnet.RegionalNode
	machine          tailnet.NetworkBinding
	machineAuthority *tailnet.Authority
	clients          map[string]tailnet.NetworkBinding
	generation       int64
}

func (n *upgradeNetwork) scope(direction string, expires int64) tailnet.NetworkScope {
	return tailnet.NetworkScope{ResourceKind: "machine_access", ResourceID: "access_connected", ResourceGeneration: 1, Capability: "managed_ssh", Direction: direction, Port: tailnet.NetworkPort, ExpiresAt: expires}
}

// publishLocked signs and applies the machine's configuration with every live generation.
func (n *upgradeNetwork) publishLocked() {
	n.generation++
	now := time.Now().Unix()
	config := tailnet.NetworkConfiguration{Version: 1, Issuer: n.issuer, Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: uint64(n.generation), Self: n.machine}
	for _, client := range n.clients {
		config.Peers = append(config.Peers, tailnet.NetworkPeer{Identity: client, Scopes: []tailnet.NetworkScope{n.scope("accept", now+300)}})
	}
	connectedApply(n.t, n.machineAuthority, n.signer, config, n.node)
}

func (n *upgradeNetwork) clientTokens(endpoint string) api.PeerNetworkConfigurationResult {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.generation++
	now := time.Now().Unix()
	config := tailnet.NetworkConfiguration{Version: 1, Issuer: n.issuer, Audience: "paperboat-network", IssuedAt: now, ExpiresAt: now + 300, Generation: uint64(n.generation), Self: n.clients[endpoint],
		Peers: []tailnet.NetworkPeer{{Identity: n.machine, Scopes: []tailnet.NetworkScope{n.scope("dial", now+300)}}}}
	candidate, grant := connectedRegionalTokens(n.t, n.signer, config, n.node)
	return api.PeerNetworkConfigurationResult{Configuration: connectedSignedJWT(n.t, n.signer, "paperboat-network-config+jwt", config), CandidateSet: candidate, RelayGrants: []string{grant}}
}

func (n *upgradeNetwork) api(endpoint string, quicPublic []byte, fingerprint string, address int) connectedNetworkAPI {
	return connectedNetworkAPI{register: func(request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		n.mu.Lock()
		defer n.mu.Unlock()
		binding := tailnet.NetworkBinding{AccountID: n.account, EndpointID: endpoint, Role: "cli", EndpointGeneration: 1, KeyGeneration: request.ExpectedKeyGeneration + 1, WireGuardPublicKey: request.WireGuardPublicKey, DiscoPublicKey: request.DiscoPublicKey, QUICCertificateFingerprint: fingerprint, QUICPublicKey: base64.RawURLEncoding.EncodeToString(quicPublic), VirtualAddress: fmt.Sprintf("fd7a:115c:a1e0::%x", address)}
		n.clients[endpoint] = binding
		n.publishLocked()
		return api.PeerNetworkRegistrationResult{KeyGeneration: binding.KeyGeneration, VirtualAddress: binding.VirtualAddress}, nil
	}, config: func() (api.PeerNetworkConfigurationResult, error) { return n.clientTokens(endpoint), nil }}
}

// withdraw drops a generation from the machine, as the server does for Withdraw, eviction or revocation.
func (n *upgradeNetwork) withdraw(endpoint string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.clients, endpoint)
	n.publishLocked()
}

// A forwarded stream held by the old daemon generation keeps flowing while the new generation registers its own
// identity and serves new connects: no gap above 1 s, same connection, no reconnect. Withdrawing (or revoking) the
// old generation ends that held stream at once, and the new generation keeps working.
func TestHeldStreamSurvivesDaemonGenerationUpgrade(t *testing.T) {
	if testing.Short() {
		t.Skip("real authenticated DERP/QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	issuer, accountID, session, machineID := "https://api.example.test", "account_connected", "cli_connected", "machine_connected"
	store := connectedStore(t)
	identity, cliLeaf, machineLeaf := connectedEndpointAuthority(t, store, issuer, accountID, session, machineID)
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
	relayCtx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan error, 1)
	go func() {
		relayDone <- relay.Serve(relayCtx, relaySocket, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{relayCertificate}})
	}()
	defer func() { stopRelay(); _ = relay.Close(); _ = relaySocket.Close(); <-relayDone; relay.Wait() }()
	now := time.Now().Unix()
	node := tailnet.RegionalNode{NodeID: "relay_connected", NodeGeneration: 1, ProcessEpoch: "epoch_connected", Region: "test", FailureDomain: "local", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(relaySocket.LocalAddr().(*net.UDPAddr).Port), State: "ready", ObservedAt: now, ExpiresAt: now + 90, CapacityLimit: 100, CapacityUsed: 1, CapacityObservedAt: now}

	machine := tailnet.NetworkBinding{AccountID: accountID, EndpointID: machineID, Role: "machine", MachineID: machineID, EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: identity.MachineCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.MachineCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::92"}
	machineAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: machine, Keys: connectedNetworkKeys{signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer machineAuthority.Close()
	if machine.WireGuardPublicKey, machine.KeyGeneration, err = machineAuthority.PrepareKey(false); err != nil {
		t.Fatal(err)
	}
	machine.KeyGeneration++
	machine.DiscoPublicKey, _ = machineAuthority.DiscoveryPublicKey()
	network := &upgradeNetwork{t: t, issuer: issuer, account: accountID, signer: signerPrivate, node: node, machine: machine, machineAuthority: machineAuthority, clients: map[string]tailnet.NetworkBinding{}}
	network.mu.Lock()
	network.publishLocked()
	network.mu.Unlock()
	regions, err := machineAuthority.ConfigureRegionalRelays(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: relayRoots, ServerName: "localhost", Certificates: []tls.Certificate{machineLeaf}})
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
	var peersMu sync.Mutex
	servedBy := map[string]int{}
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- machineOwner.Listen(ctx, regions[0], func(serveCtx context.Context, s *native.Session) error {
			for {
				stream, _, acceptErr := s.AcceptAuthorized(serveCtx, func(_ context.Context, header streamauth.Header) (string, error) {
					if header.Consumer != "ssh" || header.Credential != "credential_ssh" {
						return "", errors.New("credential rejected")
					}
					return "access_connected", nil
				})
				if acceptErr != nil {
					return acceptErr
				}
				peersMu.Lock()
				servedBy[s.PeerID()]++
				peersMu.Unlock()
				go func(connection net.Conn) { defer connection.Close(); _, _ = io.Copy(connection, connection) }(stream)
			}
		})
	}()

	start := func(generation string, authority clientauthority.Authority, address int) (*cliNativeRuntime, string) {
		endpoint, ok := peernetwork.DaemonEndpointID(session, generation)
		if !ok {
			t.Fatal("daemon endpoint")
		}
		clientHTTP := &http.Client{Transport: connectedRoundTripper(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/.well-known/jwks.json" {
				return nil, errors.New("unexpected HTTP request")
			}
			body := fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"connected_key","x":"%s"}]}`, base64.RawURLEncoding.EncodeToString(signerPublic))
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
		})}
		relayTrust := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: relayRoots, ServerName: "localhost", Certificates: []tls.Certificate{cliLeaf}}
		runtime, err := newCLINativeRuntime(ctx, issuer, store, network.api(endpoint, authority.LocalCertificate.Claims.QUICPublicKey, authority.LocalCertificate.Fingerprint(), address), clientHTTP, relayTrust, accountID, endpoint, authority, "")
		if err != nil {
			t.Fatal(err)
		}
		return runtime, endpoint
	}
	expires := time.Now().Add(time.Minute).UTC().Truncate(time.Second)
	target := &resolver.TerminalTarget{EnvironmentID: "env_connected", Auth: resolver.AuthTarget{Token: "credential_ssh", ExpiresAt: expires.Format(time.RFC3339), ResourceID: "access_connected"}}
	application := peerApplication{operationID: "operation_connected", stream: "ssh", raw: func(_ context.Context, stream io.ReadWriteCloser) (Conn, error) {
		return &connectedRawConn{Conn: stream.(net.Conn)}, nil
	}}
	open := func(runtime *cliNativeRuntime) net.Conn {
		t.Helper()
		openCtx, cancelOpen := context.WithTimeout(ctx, 10*time.Second)
		defer cancelOpen()
		connection, err := runtime.openApplication(openCtx, machineID, "ssh", application, target, time.Now)
		if err != nil {
			t.Fatalf("open application: %v", err)
		}
		return connection.(*connectedRawConn).Conn
	}

	oldRuntime, oldEndpoint := start("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", cloneAuthority(identity), 0x91)
	defer oldRuntime.Close()
	held := open(oldRuntime)
	// The held stream echoes one byte every 20 ms; the largest gap between echoes is the interruption a user sees.
	type echoResult struct {
		maxGap time.Duration
		err    error
	}
	stopEcho := make(chan struct{})
	echoed := make(chan echoResult, 1)
	go func() {
		var result echoResult
		last := time.Now()
		buffer := []byte{0}
		for i := 0; ; i++ {
			select {
			case <-stopEcho:
				echoed <- result
				return
			default:
			}
			_ = held.SetDeadline(time.Now().Add(5 * time.Second))
			buffer[0] = byte(i)
			if _, result.err = held.Write(buffer); result.err == nil {
				_, result.err = io.ReadFull(held, buffer)
			}
			if result.err != nil || buffer[0] != byte(i) {
				if result.err == nil {
					result.err = fmt.Errorf("echo %d returned %d", i, buffer[0])
				}
				echoed <- result
				return
			}
			result.maxGap = max(result.maxGap, time.Since(last))
			last = time.Now()
			time.Sleep(20 * time.Millisecond)
		}
	}()
	time.Sleep(500 * time.Millisecond)

	// The upgrade: the new generation registers its own key and serves a new connect while the old one holds its stream.
	newRuntime, newEndpoint := start("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", cloneAuthority(identity), 0x93)
	defer newRuntime.Close()
	fresh := open(newRuntime)
	_ = fresh.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fresh.Write([]byte("new-generation")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("new-generation"))
	if _, err := io.ReadFull(fresh, got); err != nil || string(got) != "new-generation" {
		t.Fatalf("new generation echo %q: %v", got, err)
	}
	time.Sleep(2 * time.Second)
	close(stopEcho)
	result := <-echoed
	if result.err != nil || result.maxGap > time.Second {
		t.Fatalf("held stream across the upgrade: max gap %v, error %v", result.maxGap, result.err)
	}
	peersMu.Lock()
	if servedBy[oldEndpoint] != 1 || servedBy[newEndpoint] != 1 {
		t.Fatalf("streams served per generation: %v", servedBy)
	}
	peersMu.Unlock()

	// Withdrawing (or revoking) the old generation ends what it still holds at once.
	network.withdraw(oldEndpoint)
	_ = held.SetDeadline(time.Now().Add(10 * time.Second))
	began := time.Now()
	for {
		if _, err := held.Write([]byte{1}); err != nil {
			break
		}
		if _, err := io.ReadFull(held, make([]byte, 1)); err != nil {
			break
		}
		if time.Since(began) > 8*time.Second {
			t.Fatal("withdrawn generation's held stream kept flowing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ended := time.Since(began); ended > 5*time.Second {
		t.Fatalf("withdrawn generation's held stream ended after %v", ended)
	}
	_ = fresh.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fresh.Write([]byte("x")); err != nil {
		t.Fatalf("new generation after the old one withdrew: %v", err)
	}
	if _, err := io.ReadFull(fresh, make([]byte, 1)); err != nil {
		t.Fatalf("new generation after the old one withdrew: %v", err)
	}
	_ = fresh.Close()
	cancel()
	select {
	case <-listenDone:
	case <-time.After(2 * time.Second):
		t.Fatal("machine listener did not stop")
	}
}
