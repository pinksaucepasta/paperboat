//go:build linux || darwin

package runtime

import (
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
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativesession"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/mesh"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/relay/derpquic"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
)

// promoteFixture is one enrolled machine (state root, signed network, control
// server) with a CLI peer, run without root: an old primary generation and a
// delegated one share its custody, as two carrier processes do during pb update.
type promoteFixture struct {
	root, issuer    string
	signer          ed25519.PrivateKey
	control         *httptest.Server
	stop            chan struct{}
	clientTLS       tls.Certificate
	clientAuthority *tailnet.Authority
	client          *native.Owner
	primaryAddress  mesh.Addr
	relay           tailnet.RegionalNode
	relayRoots      *x509.CertPool
	mu              sync.Mutex
	network         tailnet.NetworkConfiguration // the machine's signed configuration
	clientNetwork   tailnet.NetworkConfiguration
	// primaryTaken lets the generation report the machine's endpoints once it took the primary role.
	primaryTaken      bool
	endpointsReported bool
}

func newPromoteFixture(t *testing.T) *promoteFixture {
	t.Helper()
	f := &promoteFixture{root: t.TempDir(), stop: make(chan struct{})}
	public, signer, _ := ed25519.GenerateKey(rand.Reader)
	f.signer = signer
	f.control = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(t, public, w, r) }))
	f.issuer = f.control.URL
	t.Cleanup(f.control.Close)
	t.Cleanup(func() { close(f.stop) }) // runs first: held event waits return before the server closes
	writeFixtureFile(t, filepath.Join(f.root, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.control.Certificate().Raw}))
	f.enroll(t)
	f.startRelay(t)
	f.prepareNetwork(t)
	return f
}

func writeFixtureFile(t *testing.T, path string, body []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *promoteFixture) enroll(t *testing.T) {
	t.Helper()
	store, err := runtimeidentity.Open(runtimeidentity.Config{StateRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	now := time.Now().UTC()
	if err := store.SaveRegistration(runtimeidentity.Registration{ServerURL: f.issuer, MachineID: "machine_target", EnvironmentID: "env_target", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(f.root, "inbox"), InstallationGeneration: 1, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	identity, _ := json.Marshal(enrollment.RuntimeIdentity{Version: 1, HelperID: "hlp_target", MachineID: "machine_target", EnvironmentID: "env_target", Credential: "synthetic-target-credential-for-local-test", ExpiresAt: now.Add(time.Hour), KeyID: key.ID, InstallationGeneration: 1})
	writeFixtureFile(t, filepath.Join(f.root, "runtime-identity.json"), identity)
	endpoint, err := store.PeerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	certificate, err := endpointidentity.Sign(private, endpointidentity.Claims{AccountID: "account_target", Role: endpointidentity.RoleMachine, EndpointID: "machine_target", QUICPublicKey: endpoint.QUICPublicKey(), Generation: 1, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := certificate.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerEndpointCertificate(public, raw, now); err != nil {
		t.Fatal(err)
	}
}

type promoteKeys struct{ key ed25519.PublicKey }

func (k promoteKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return k.key, true, nil
}

func (f *promoteFixture) store() config.ProfileStore {
	return config.ProfileStore{Path: filepath.Join(f.root, "peer-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "peer-network", "secrets")}}
}

// prepareNetwork signs the machine's network with one CLI peer allowed to open terminals.
func (f *promoteFixture) prepareNetwork(t *testing.T) {
	t.Helper()
	_, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	public := f.signer.Public().(ed25519.PublicKey)
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: f.store(), Issuer: f.issuer, Self: self, Keys: promoteKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	self.WireGuardPublicKey, self.KeyGeneration, err = authority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	self.KeyGeneration++
	if self.DiscoPublicKey, err = authority.DiscoveryPublicKey(); err != nil {
		t.Fatal(err)
	}
	self.VirtualAddress = "fd7a:115c:a1e0::2"
	f.clientTLS, _ = newBrowserTerminalClientCertificate(t, time.Now())
	clientFingerprint := certificateFingerprint(f.clientTLS)
	clientPublic := f.clientTLS.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	client := tailnet.NetworkBinding{AccountID: self.AccountID, EndpointID: "cli_target", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: clientFingerprint, QUICPublicKey: base64.RawURLEncoding.EncodeToString(clientPublic), VirtualAddress: "fd7a:115c:a1e0::1"}
	clientStore := config.ProfileStore{Path: filepath.Join(f.root, "client-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "client-network", "secrets")}}
	f.clientAuthority, err = tailnet.NewAuthority(tailnet.AuthorityOptions{Store: clientStore, Issuer: f.issuer, Self: client, Keys: promoteKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.clientAuthority.Close() })
	client.WireGuardPublicKey, client.KeyGeneration, err = f.clientAuthority.PrepareKey(false)
	if err != nil {
		t.Fatal(err)
	}
	client.KeyGeneration++
	if client.DiscoPublicKey, err = f.clientAuthority.DiscoveryPublicKey(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	f.network = tailnet.NetworkConfiguration{Version: 1, Issuer: f.issuer, Audience: "paperboat-network", IssuedAt: now - 1, ExpiresAt: now + 300, Generation: 1, Self: self, Peers: []tailnet.NetworkPeer{{Identity: client, Scopes: []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "access_target", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300}}}}}
	if err := authority.Apply(t.Context(), f.token(t, f.network)); err != nil {
		t.Fatal(err)
	}
}

func (f *promoteFixture) token(t *testing.T, configuration tailnet.NetworkConfiguration) string {
	return f.sign(t, "paperboat-network-config+jwt", configuration)
}

func (f *promoteFixture) sign(t *testing.T, typ string, claims any) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "target", "typ": typ})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Error(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.signer, []byte(unsigned)))
}

func (f *promoteFixture) serve(t *testing.T, public ed25519.PublicKey, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case machineevents.Path:
		select {
		case <-f.stop:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	case "/.well-known/jwks.json":
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "target", "use": "sig", "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(public)}}})
	case "/v1/machine-peer-network/config":
		var request clientapi.PeerNetworkConfigurationRequest
		decodeErr := json.NewDecoder(r.Body).Decode(&request)
		f.mu.Lock()
		primary := f.primaryTaken
		f.endpointsReported = f.endpointsReported || request.Endpoints != nil
		network := f.network
		f.mu.Unlock()
		if decodeErr != nil || !primary && (request.Endpoints != nil || request.MembershipGrants) {
			t.Error("a delegated generation modified the primary endpoints or relay membership")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		candidates, grants := f.regionalTokens(t, network)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": clientapi.PeerNetworkConfigurationResult{Configuration: f.token(t, network), CandidateSet: candidates, RelayGrants: grants, EndpointReports: true}})
	case "/v1/machine-peer-network/register":
		t.Error("a carrier generation attempted to register a machine network identity")
		w.WriteHeader(http.StatusBadRequest)
	case "/v1/helper-trust/revocations":
		io.WriteString(w, `{"jtis":[]}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// echoApps marks each stream with its generation, then echoes it.
func echoApps(t *testing.T, mark byte) *nativesession.Service {
	t.Helper()
	apps, err := nativesession.New(nativesession.Config{Authorize: func(context.Context, streamauth.Header) (string, error) { return "access_target", nil }, ServeStream: func(_ context.Context, _ streamauth.Header, c net.Conn) error {
		if _, err := c.Write([]byte{mark}); err != nil {
			return err
		}
		_, err := io.Copy(c, c)
		return err
	}, ServeTransfer: func(context.Context, net.Conn) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return apps
}

// startPrimary runs the old primary generation on a pinned port: the client's
// signed endpoint stays valid for it. admit gates its new connections, as the
// production peer's AdmitNew does at the end of the overlap.
func (f *promoteFixture) startPrimary(t *testing.T, ctx context.Context, admit func() bool) *native.Owner {
	t.Helper()
	tlsConfig, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	listener := newPinnedPacketListener(t)
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: f.store(), Issuer: f.issuer, Self: self, Keys: promoteKeys{f.signer.Public().(ed25519.PublicKey)}, TestOnlyPacketListener: listener})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	primary, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig, AdmitNew: admit})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Listen(nil); err != nil {
		t.Fatal(err)
	}
	var port uint16
	select {
	case port = <-listener.bound:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	f.mu.Lock()
	f.clientNetwork = f.network
	f.mu.Unlock()
	f.applyClient(t, ctx, func(c *tailnet.NetworkConfiguration) {
		c.Peers[0].Endpoints = []string{primaryAddress(t, port).String()}
	})
	if _, err := f.clientAuthority.ConfigureRegionalRelays(f.relayTLS(&tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{f.clientTLS}})); err != nil {
		t.Fatal(err)
	}
	f.client, err = native.NewOwner(native.Config{Authority: f.clientAuthority, TLS: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{f.clientTLS}}}) //nolint:gosec // peer identity is pinned by the signed network
	if err != nil {
		t.Fatal(err)
	}
	if f.primaryAddress, err = f.clientAuthority.Descriptor("machine_target"); err != nil {
		t.Fatal(err)
	}
	apps := echoApps(t, 'P')
	run, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- primary.Listen(run, nil, apps.Serve) }()
	t.Cleanup(func() { cancel(); f.client.Close(); primary.Close(); <-done; apps.Wait() })
	return primary
}

// applyClient signs the CLI's view of the machine, with change applied.
func (f *promoteFixture) applyClient(t *testing.T, ctx context.Context, change func(*tailnet.NetworkConfiguration)) {
	t.Helper()
	f.mu.Lock()
	c := f.clientNetwork
	c.Peers = append([]tailnet.NetworkPeer(nil), c.Peers...)
	if c.Self.EndpointID != "cli_target" {
		scope := c.Peers[0].Scopes[0]
		scope.Direction = "dial"
		c.Self, c.Peers = c.Peers[0].Identity, []tailnet.NetworkPeer{{Identity: f.network.Self, Scopes: []tailnet.NetworkScope{scope}}}
	}
	c.Generation++
	c.IssuedAt = time.Now().Unix()
	change(&c)
	f.clientNetwork = c
	f.mu.Unlock()
	f.applyAuthority(t, ctx, f.clientAuthority, c)
}

// applyAuthority applies a signed refresh in its real order: configuration, then candidates, then grants.
func (f *promoteFixture) applyAuthority(t *testing.T, ctx context.Context, authority *tailnet.Authority, c tailnet.NetworkConfiguration) {
	t.Helper()
	if err := authority.Apply(ctx, f.token(t, c)); err != nil {
		t.Fatal(err)
	}
	candidates, grants := f.regionalTokens(t, c)
	if err := authority.ApplyRegionalCandidates(ctx, candidates); err != nil {
		t.Fatal(err)
	}
	if err := authority.ApplyRelayGrants(ctx, grants); err != nil {
		t.Fatal(err)
	}
}

// startGeneration starts the update's delegated generation from the same
// enrolled state on its own authority, as the separate carrier process does.
// ports, when set, records the generation's UDP binds.
func (f *promoteFixture) startGeneration(t *testing.T, ctx context.Context, ports *udpPortRecorder) (*productionNativePeerService, *productionNativePeerGeneration) {
	t.Helper()
	tlsConfig, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	options := tailnet.AuthorityOptions{Store: f.store(), Issuer: f.issuer, Self: self, Keys: promoteKeys{f.signer.Public().(ed25519.PublicKey)}}
	if ports != nil {
		options.TestOnlyPacketListener = ports
	}
	authority, err := tailnet.NewAuthority(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	network := f.network
	f.mu.Unlock()
	candidates, grants := f.regionalTokens(t, network)
	if err := errors.Join(authority.ApplyRegionalCandidates(ctx, candidates), authority.ApplyRelayGrants(ctx, grants)); err != nil {
		t.Fatal(err)
	}
	if _, err := authority.ConfigureRegionalRelays(f.relayTLS(tlsConfig)); err != nil {
		t.Fatal(err)
	}
	owner, err := native.NewOwner(native.Config{Authority: authority, TLS: tlsConfig})
	if err != nil {
		t.Fatal(err)
	}
	_, source, err := installsource.Current()
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadCarrierIdentity(ctx, "test", f.environ)
	if err != nil {
		t.Fatal(err)
	}
	api := clientapi.New(base.controlURL.String(), config.Credential{}, &http.Client{Transport: base.transport, Timeout: 15 * time.Second})
	api.SetMachineAuth(base.machineIdentity)
	run, cancel := context.WithCancel(t.Context())
	s := &productionNativePeerService{config: productionNativePeerConfig{carrierGeneration: source.SHA256[:32]}}
	g := &productionNativePeerGeneration{cancel: cancel, owner: owner, apps: echoApps(t, 'G'), errors: make(chan error, 4), authority: authority, requestRefresh: authority.RequestUrgentRefresh, control: &generationNetwork{NetworkAPI: api}}
	if err := s.startDelegatedGeneration(run, ctx, g, s.config.carrierGeneration); err != nil {
		cancel()
		_ = owner.Close()
		t.Fatal(err)
	}
	s.current = g
	t.Cleanup(func() { _ = g.stop() })
	return s, g
}

func (f *promoteFixture) environ(key string) string {
	return map[string]string{"PAPERBOAT_RUNTIME_STATE_ROOT": f.root, "PAPERBOAT_RUNTIME_LISTEN_ADDRESS": "127.0.0.1:1", "PAPERBOAT_MACHINE_ID": "machine_target", "PAPERBOAT_CONTROL_URL": f.issuer, "PAPERBOAT_CONTROL_CA_FILE": filepath.Join(f.root, "ca.pem"), "PAPERBOAT_WORKSPACE_ROOT": f.root}[key]
}

// openMarked opens a terminal stream on connection and returns it with the generation mark it was served by.
func openMarked(t *testing.T, ctx context.Context, connection *native.Session) (net.Conn, byte) {
	t.Helper()
	header, err := streamauth.New("target", "terminal", "target", "credential_target", time.Now().Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := connection.OpenAuthorized(ctx, header, "access_target", "terminal")
	if err != nil {
		t.Fatal("a generation refused the signed terminal grant", err)
	}
	t.Cleanup(func() { stream.Close() })
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	var mark [1]byte
	if _, err := io.ReadFull(stream, mark[:]); err != nil {
		t.Fatal("no generation answered the stream", err)
	}
	_ = stream.SetDeadline(time.Time{})
	return stream, mark[0]
}

// echo proves a held stream still carries bytes both ways.
func echo(t *testing.T, stream net.Conn, what string) {
	t.Helper()
	_ = stream.SetDeadline(time.Now().Add(3 * time.Second))
	defer stream.SetDeadline(time.Time{})
	if _, err := stream.Write([]byte(what)); err != nil {
		t.Fatal(what, err)
	}
	got := make([]byte, len(what))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != what {
		t.Fatalf("%s: echo %q, %v", what, got, err)
	}
}

// pinnedPacketListener binds the primary's IPv4 socket to one probed port, so
// the client's signed endpoint names it (another test's engine took the first
// random port on CI once: main job 127099348562).
type pinnedPacketListener struct {
	port  uint16
	bound chan uint16
}

func newPinnedPacketListener(t *testing.T) *pinnedPacketListener {
	t.Helper()
	probe, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(probe.LocalAddr().(*net.UDPAddr).Port)
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	return &pinnedPacketListener{port: port, bound: make(chan uint16, 16)}
}

func (l *pinnedPacketListener) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	if network == "udp4" {
		address = net.JoinHostPort("", strconv.Itoa(int(l.port)))
	}
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err == nil && network == "udp4" {
		select {
		case l.bound <- l.port:
		default:
		}
	}
	return socket, err
}

// udpPortRecorder records each IPv4 port a generation binds.
type udpPortRecorder struct{ bound chan uint16 }

func (l *udpPortRecorder) ListenPacket(ctx context.Context, network, address string) (net.PacketConn, error) {
	socket, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err == nil && network == "udp4" {
		select {
		case l.bound <- socket.LocalAddr().(*net.UDPAddr).AddrPort().Port():
		default:
		}
	}
	return socket, err
}

// primaryAddress is this host's first global IPv4 address: signed endpoints are never loopback.
func primaryAddress(t *testing.T, port uint16) netip.AddrPort {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range addresses {
		prefix, err := netip.ParsePrefix(raw.String())
		if err == nil && prefix.Addr().Is4() && prefix.Addr().IsGlobalUnicast() && !prefix.Addr().IsLoopback() {
			return netip.AddrPortFrom(prefix.Addr(), port)
		}
	}
	t.Skip("needs a non-loopback IPv4 interface")
	return netip.AddrPort{}
}

// startRelay runs the account's one regional relay.
func (f *promoteFixture) startRelay(t *testing.T) {
	t.Helper()
	public, private, _ := ed25519.GenerateKey(rand.Reader)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: []string{"localhost"}}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	f.relayRoots = x509.NewCertPool()
	f.relayRoots.AddCert(certificate)
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay, err := derpquic.NewServer(derpquic.Verifier{Issuer: f.issuer, NodeID: "relay_target", NodeGeneration: 1, ProcessEpoch: "epoch_target", Keys: map[string]ed25519.PublicKey{"target": f.signer.Public().(ed25519.PublicKey)}})
	if err != nil {
		t.Fatal(err)
	}
	run, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- relay.Serve(run, socket, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{raw}, PrivateKey: private}}})
	}()
	t.Cleanup(func() { stop(); _ = relay.Close(); _ = socket.Close(); <-done; relay.Wait() })
	f.relay = tailnet.RegionalNode{NodeID: "relay_target", NodeGeneration: 1, ProcessEpoch: "epoch_target", Region: "test", FailureDomain: "rack-a", Roles: []string{"relay"}, Transports: []string{"derp_quic"}, EndpointHost: "127.0.0.1", EndpointQUICPort: uint16(socket.LocalAddr().(*net.UDPAddr).Port), State: "ready", CapacityLimit: 100, CapacityUsed: 1}
}

func (f *promoteFixture) relayTLS(endpoint *tls.Config) *tls.Config {
	c := endpoint.Clone()
	c.RootCAs, c.ServerName, c.InsecureSkipVerify = f.relayRoots, "localhost", false
	return c
}

// regionalTokens are the server's candidate set and relay grant for c's endpoint, issued now.
func (f *promoteFixture) regionalTokens(t *testing.T, c tailnet.NetworkConfiguration) (string, []string) {
	now := time.Now().Unix()
	node := f.relay
	node.ObservedAt, node.CapacityObservedAt, node.ExpiresAt = now, now, now+90
	expires := min(now+60, c.ExpiresAt)
	candidates := tailnet.RegionalCandidates{Schema: "paperboat.regional-candidates.v1", Issuer: c.Issuer, Audience: "paperboat-regional-candidates", AccountID: c.Self.AccountID, EndpointID: c.Self.EndpointID, AuthorizationGeneration: c.Generation, Generation: c.Generation, IssuedAt: now, ExpiresAt: expires, Nodes: []tailnet.RegionalNode{node}}
	peers := make([]derpquic.Peer, 0, len(c.Peers))
	for _, peer := range c.Peers {
		scopes := make([]derpquic.Scope, len(peer.Scopes))
		for i, s := range peer.Scopes {
			scopes[i] = derpquic.Scope{ResourceKind: s.ResourceKind, ResourceID: s.ResourceID, ResourceGeneration: s.ResourceGeneration, Capability: s.Capability, Direction: s.Direction, Port: s.Port, ExpiresAt: s.ExpiresAt}
		}
		peers = append(peers, derpquic.Peer{WireGuardPublicKey: peer.Identity.WireGuardPublicKey, DiscoPublicKey: peer.Identity.DiscoPublicKey, Scopes: scopes})
	}
	grant := derpquic.Grant{Version: 1, Issuer: c.Issuer, Audience: "paperboat-relay", IssuedAt: now, ExpiresAt: expires, Generation: c.Generation, AccountID: c.Self.AccountID, EndpointID: c.Self.EndpointID, WireGuardPublicKey: c.Self.WireGuardPublicKey, DiscoPublicKey: c.Self.DiscoPublicKey, CertificateFingerprint: c.Self.QUICCertificateFingerprint, QUICPublicKey: c.Self.QUICPublicKey, NodeID: node.NodeID, NodeGeneration: node.NodeGeneration, ProcessEpoch: node.ProcessEpoch, Peers: peers}
	return f.sign(t, "paperboat-regional-candidates+jwt", candidates), []string{f.sign(t, "paperboat-relay-grant+jwt", grant)}
}

// certificateFingerprint is a QUIC leaf's binding fingerprint: hex SHA-256 of its DER.
func certificateFingerprint(c tls.Certificate) string {
	sum := sha256.Sum256(c.Certificate[0])
	return hex.EncodeToString(sum[:])
}
