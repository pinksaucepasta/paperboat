package native_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	//paperboat:allow-source-policy tailscale-import owner=peer-networking reason=fault-injection-test
	"tailscale.com/tailcfg"
)

// boundPorts records the UDP ports an engine binds, so the test can deliver them as the host's endpoints.
type boundPorts struct {
	mu    sync.Mutex
	ports []int
}

func (b *boundPorts) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	pc, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err == nil {
		b.mu.Lock()
		b.ports = append(b.ports, pc.LocalAddr().(*net.UDPAddr).Port)
		b.mu.Unlock()
	}
	return pc, err
}

// lanAddress is a non-loopback IPv4 address of this host: a delivered endpoint is never loopback.
func lanAddress(t *testing.T) netip.Addr {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil && prefix.Addr().Is4() && !prefix.Addr().IsLoopback() && !prefix.Addr().IsLinkLocalUnicast() {
			return prefix.Addr()
		}
	}
	t.Skip("this host has no non-loopback IPv4 address")
	return netip.Addr{}
}

// F20-02/04: right after pairing, a daemon dialing the machine it runs on reached nothing for 20 s: its first
// configuration carried no endpoints for the host (the server withheld peers' endpoints until a client had
// reported its own) and the relay was not usable yet. With the host's endpoints in the client's first
// configuration the dial goes direct with no relay at all; a withdrawn grant is still refused.
func TestSameHostDialRightAfterPairingNeedsNoRelay(t *testing.T) {
	if testing.Short() {
		t.Skip("real native QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	lan := lanAddress(t)
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	serverTLS, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::71"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::72"}
	clientSelf := bindTestQUICKey(clientBinding)
	clientAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}, Issuer: "https://api.example.test", Self: clientSelf, Keys: testKeys{"native_test": signerPublic}, LocalMachineID: "machine_test"})
	if err != nil {
		t.Fatal(err)
	}
	defer clientAuthority.Close()
	ports := &boundPorts{}
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic}, ports)
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	now := time.Now().Unix()
	serverConfig := testConfiguration(now, 1, serverBinding, clientBinding, "accept")
	applyTestConfiguration(t, serverAuthority, signerPrivate, serverConfig)
	serverOwner, err := native.NewOwner(native.Config{Authority: serverAuthority, TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer serverOwner.Close()
	// The relay is down: its region points at a closed port, so every DERP send fails as on soft-h-11052022.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	deadRegion := &tailcfg.DERPRegion{RegionID: 900, RegionCode: "dead", Nodes: []*tailcfg.DERPNode{{Name: "900a", RegionID: 900, HostName: "127.0.0.1", IPv4: "127.0.0.1", IPv6: "none", DERPPort: deadPort, STUNPort: -1}}}
	descriptor := startTestServer(t, serverOwner, serverAuthority, deadRegion)
	ports.mu.Lock()
	var endpoints []string
	for _, port := range ports.ports {
		if endpoint := netip.AddrPortFrom(lan, uint16(port)).String(); !slices.Contains(endpoints, endpoint) && port != 0 {
			endpoints = append(endpoints, endpoint)
		}
	}
	ports.mu.Unlock()
	if len(endpoints) == 0 {
		t.Fatal("the host engine bound no UDP port")
	}
	// The client's first configuration: the host's endpoints are delivered with it (server deliverEndpoints).
	clientConfig := testConfiguration(now, 1, clientBinding, serverBinding, "dial")
	clientConfig.Peers[0].Endpoints = endpoints[:min(len(endpoints), tailnet.MaxPeerEndpoints)]
	applyTestConfiguration(t, clientAuthority, signerPrivate, clientConfig)
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()

	started := time.Now()
	dialCtx, cancelDial := context.WithTimeout(ctx, 10*time.Second)
	session, err := clientOwner.Dial(dialCtx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	cancelDial()
	if err != nil {
		for e := error(err); e != nil; e = errors.Unwrap(e) {
			t.Logf("chain: %T %v", e, e)
		}
		t.Fatalf("same-host dial with no relay: %v", err)
	}
	defer session.Close()
	header, err := streamauth.New("operation_terminal", "terminal", "stream_terminal", "credential_terminal", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := session.OpenAuthorized(ctx, header, "grant_test", "terminal")
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("same-host")
	if _, err := stream.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != string(payload) {
		t.Fatalf("echo %q: %v", got, err)
	}
	_ = stream.Close()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("same-host dial took %v", elapsed)
	}

	// The grant is withdrawn: a new stream on the same session is refused.
	revoked := clientConfig
	revoked.Generation, revoked.IssuedAt = 2, time.Now().Unix()
	other := clientConfig.Peers[0]
	other.Scopes = []tailnet.NetworkScope{other.Scopes[0]}
	other.Scopes[0].ResourceID = "grant_other" // the machine still admits this client, for another grant only
	revoked.Peers = []tailnet.NetworkPeer{other}
	applyTestConfiguration(t, clientAuthority, signerPrivate, revoked)
	if refused, err := session.OpenAuthorized(ctx, header, "grant_test", "terminal"); err == nil {
		_ = refused.Close()
		t.Fatal("a withdrawn grant opened a stream")
	}
}

// A dial before any path exists (no endpoints of the host delivered yet, relay down) is refused at once with a typed
// "still starting" error naming what is missing, never a silent wait for the dial deadline (soft-a-11051819).
func TestDialBeforeAnyPathExistsFailsFastWithPathNotReady(t *testing.T) {
	if testing.Short() {
		t.Skip("real native QUIC integration")
	}
	t.Setenv("IN_TS_TEST", "true")
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientTLS, clientFingerprint := testTLS(t, "cli")
	_, serverFingerprint := testTLS(t, "machine")
	clientBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "cli_test", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, VirtualAddress: "fd7a:115c:a1e0::73"}
	serverBinding := tailnet.NetworkBinding{AccountID: "account_test", EndpointID: "machine_test", Role: "machine", MachineID: "machine_test", EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: serverFingerprint, VirtualAddress: "fd7a:115c:a1e0::74"}
	clientAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: config.ProfileStore{Path: t.TempDir(), Secrets: testSecrets{}}, Issuer: "https://api.example.test", Self: bindTestQUICKey(clientBinding), Keys: testKeys{"native_test": signerPublic}, LocalMachineID: "machine_test"})
	if err != nil {
		t.Fatal(err)
	}
	defer clientAuthority.Close()
	serverAuthority := testAuthority(t, serverBinding, testKeys{"native_test": signerPublic})
	clientBinding.WireGuardPublicKey, clientBinding.KeyGeneration = prepareBinding(t, clientAuthority)
	serverBinding.WireGuardPublicKey, serverBinding.KeyGeneration = prepareBinding(t, serverAuthority)
	applyTestConfiguration(t, clientAuthority, signerPrivate, testConfiguration(time.Now().Unix(), 1, clientBinding, serverBinding, "dial"))
	descriptor, err := clientAuthority.Descriptor(serverBinding.EndpointID)
	if err != nil {
		t.Fatal(err)
	}
	clientOwner, err := native.NewOwner(native.Config{Authority: clientAuthority, TLS: clientTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer clientOwner.Close()
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, err := clientOwner.Dial(ctx, descriptor, serverBinding.EndpointID, peerquic.ClassInteractive)
	if err == nil {
		_ = session.Close()
		t.Fatal("a dial with no path succeeded")
	}
	var notReady *native.PathNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("dial before any path: %v, want PathNotReadyError", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the refusal took %v, want at once", elapsed)
	}
}
