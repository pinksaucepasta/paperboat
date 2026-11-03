//go:build linux || darwin

package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/enrollment"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/nativeproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/native"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/streamauth"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/shared/peertransport/endpointidentity"
	"github.com/quic-go/quic-go"
)

func writeTargetJSON(t *testing.T, path string, value any, mode os.FileMode) {
	t.Helper()
	body, ok := value.([]byte)
	if !ok {
		var err error
		body, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
}

func (f *targetCarrierFixture) identity(t *testing.T) *runtimeidentity.Store {
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
	writeTargetJSON(t, filepath.Join(f.root, "runtime-identity.json"), enrollment.RuntimeIdentity{Version: 1, HelperID: "hlp_target", MachineID: "machine_target", EnvironmentID: "env_target", Credential: "synthetic-target-credential-for-local-test", ExpiresAt: now.Add(time.Hour), KeyID: key.ID, InstallationGeneration: 1}, 0600)
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
	return store
}

type targetCarrierKeys struct{ key ed25519.PublicKey }

func (k targetCarrierKeys) Lookup(context.Context, string) (ed25519.PublicKey, bool, error) {
	return k.key, true, nil
}

func (f *targetCarrierFixture) prepareAuthority(t *testing.T, identity *runtimeidentity.Store) {
	t.Helper()
	_, _, self, err := loadNativePeerTLS(f.root, "machine_target", 1)
	if err != nil {
		t.Fatal(err)
	}
	store := config.ProfileStore{Path: filepath.Join(f.root, "peer-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "peer-network", "secrets")}}
	public := f.signer.Public().(ed25519.PublicKey)
	authority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: f.issuer, Self: self, Keys: targetCarrierKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	self.WireGuardPublicKey, self.KeyGeneration, err = authority.PrepareKey(false)
	self.KeyGeneration++
	if err != nil {
		t.Fatal(err)
	}
	self.DiscoPublicKey, err = authority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	self.VirtualAddress = "fd7a:115c:a1e0::2"
	f.clientTLS, _ = newBrowserTerminalClientCertificate(t, time.Now())
	clientPublic := f.clientTLS.PrivateKey.(ed25519.PrivateKey).Public().(ed25519.PublicKey)
	client := tailnet.NetworkBinding{AccountID: self.AccountID, EndpointID: "cli_target", Role: "cli", EndpointGeneration: 1, QUICCertificateFingerprint: "0000000000000000000000000000000000000000000000000000000000000001", QUICPublicKey: base64.RawURLEncoding.EncodeToString(clientPublic), VirtualAddress: "fd7a:115c:a1e0::1"}
	clientStore := config.ProfileStore{Path: filepath.Join(f.root, "client-network"), Secrets: config.FileSecretStore{Dir: filepath.Join(f.root, "client-network", "secrets")}}
	clientAuthority, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: clientStore, Issuer: f.issuer, Self: client, Keys: targetCarrierKeys{public}})
	if err != nil {
		t.Fatal(err)
	}
	f.clientAuthority = clientAuthority
	t.Cleanup(func() { clientAuthority.Close() })
	client.WireGuardPublicKey, client.KeyGeneration, err = clientAuthority.PrepareKey(false)
	client.KeyGeneration++
	if err != nil {
		t.Fatal(err)
	}
	client.DiscoPublicKey, err = clientAuthority.DiscoveryPublicKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	f.network = tailnet.NetworkConfiguration{Version: 1, Issuer: f.issuer, Audience: "paperboat-network", IssuedAt: now - 1, ExpiresAt: now + 300, Generation: 1, Self: self, Peers: []tailnet.NetworkPeer{{Identity: client, Scopes: []tailnet.NetworkScope{{ResourceKind: "machine_access", ResourceID: "access_target", ResourceGeneration: 1, Capability: "terminal", Direction: "accept", Port: tailnet.NetworkPort, ExpiresAt: now + 300}}}}}
	clientConfiguration := f.network
	clientConfiguration.Self = client
	scope := f.network.Peers[0].Scopes[0]
	scope.Direction = "dial"
	clientConfiguration.Peers = []tailnet.NetworkPeer{{Identity: self, Scopes: []tailnet.NetworkScope{scope}}}
	if err := clientAuthority.Apply(t.Context(), f.configurationToken(t, clientConfiguration)); err != nil {
		t.Fatal(err)
	}
	if err := authority.Apply(t.Context(), f.networkToken(t)); err != nil {
		t.Fatal(err)
	}
}

func (f *targetCarrierFixture) networkToken(t *testing.T) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.configurationToken(t, f.network)
}

func (f *targetCarrierFixture) configurationToken(t *testing.T, configuration tailnet.NetworkConfiguration) string {
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "kid": "target", "typ": "paperboat-network-config+jwt"})
	body, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(f.signer, []byte(unsigned)))
}

func (f *targetCarrierFixture) serve(t *testing.T, public ed25519.PublicKey, w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.URL.Path]++
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == machineevents.Path {
		f.waitMachineEvents(w, r)
		return
	}

	f.mu.Lock()
	revoked := f.revoked
	f.mu.Unlock()
	if revoked && r.URL.Path == "/v1/machine-peer-network/config" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/.well-known/jwks.json":
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "kid": "target", "use": "sig", "alg": "EdDSA", "x": base64.RawURLEncoding.EncodeToString(public)}}})
	case "/v1/machine-peer-network/config":
		var request clientapi.PeerNetworkConfigurationRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Endpoints != nil || request.MembershipGrants {
			t.Error("delegated authority modified primary endpoints or relay membership")
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": clientapi.PeerNetworkConfigurationResult{Configuration: f.networkToken(t), EndpointReports: true, MembershipGrants: true}})
	case "/v1/machine-peer-network/register":
		t.Error("delegated generation attempted to register a machine network identity")
		w.WriteHeader(400)
	case "/v1/helper-trust/revocations":
		io.WriteString(w, `{"jtis":[]}`)
	case "/v1/usage/native":
		io.WriteString(w, `{"success":true,"data":{}}`)
	default:
		w.WriteHeader(404)
	}
}

func (f *targetCarrierFixture) dial(t *testing.T, ctx context.Context, offer native.CarrierOffer, target ...*terminalCarrier) *native.Session {
	t.Helper()
	f.ensureNativePrimary(t, ctx)
	if len(target) != 0 {
		f.mu.Lock()
		f.extraTarget = target[0]
		f.mu.Unlock()
	}
	parent, err := f.nativeClient.Dial(ctx, f.primaryAddress, "machine_target", peerquic.ClassInteractive)
	if err != nil {
		t.Fatalf("serving native connection: %v; cause %T: %v", err, errors.Unwrap(err), errors.Unwrap(err))
	}
	t.Cleanup(func() { parent.Close() })
	f.parent = parent
	connection, err := parent.DialCarrier(ctx, offer)
	if err != nil {
		t.Fatal("production generation was not reachable at readiness", err)
	}
	t.Cleanup(func() { connection.Close() })
	return connection
}

func (f *targetCarrierFixture) admit(t *testing.T, ctx context.Context, connection *native.Session, snapshot session.Snapshot) net.Conn {
	t.Helper()
	now := time.Now()
	credential := signStaticCredential(t, f.signer, "target", auth.Claims{Issuer: f.issuer, Audience: "paperboat-machine", Subject: "account_target", JTI: "jti_target", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), Scope: []string{"terminal:operate"}, CredentialClass: "terminal_operation", EnvironmentID: "env_target", AccountID: "account_target", MachineID: "machine_target", InstallationGeneration: 1, HelperID: "hlp_target", UserID: "account_target", CLIClientSessionID: "cli_target", SessionID: snapshot.ID, ExpectedGeneration: int64(snapshot.Generation), AssignmentID: "access_target"})
	header, err := streamauth.New("target", "terminal", "target", credential, now.Add(time.Minute), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := connection.OpenAuthorized(ctx, header, "access_target", "terminal")
	if err != nil {
		t.Fatal("production generation refused its signed terminal grant", err)
	}
	t.Cleanup(func() { stream.Close() })
	stream.SetDeadline(now.Add(3 * time.Second))
	var id [16]byte
	rand.Read(id[:])
	if err := nativeproto.WritePreface(stream, nativeproto.Preface{Role: nativeproto.RoleUnified, ConnectionID: id, Token: credential}); err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(protocol.Frame{Type: "hello", RequestID: "req_target", Version: protocol.ProtocolVersion, Payload: json.RawMessage(`{"min_version":"1.0","max_version":"1.0","capabilities":["terminal.v1","health.v1"]}`)})
	if err := nativeproto.WriteRecord(stream, nativeproto.RecordStructured, hello, true); err != nil {
		t.Fatal(err)
	}
	_, payload, err := nativeproto.ReadRecord(stream, true)
	var welcome protocol.Frame
	if err != nil || json.Unmarshal(payload, &welcome) != nil || welcome.Type != "welcome" {
		t.Fatal("production endpoint did not negotiate its authenticated terminal protocol", err)
	}
	stream.SetDeadline(time.Time{})
	return stream
}

func (f *targetCarrierFixture) assertNoNewCarrierAdmission(t *testing.T, ctx context.Context, offer native.CarrierOffer) {
	t.Helper()
	config := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{peerquic.ALPN}, Certificates: []tls.Certificate{f.clientTLS}}
	public, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	connection, err := quic.DialAddr(public, offer.Claims.Endpoints[0], config, &quic.Config{})
	cancel()
	if err == nil {
		connection.CloseWithError(0, "test complete")
		t.Fatal("public socket admitted bare QUIC outside WireGuard")
	}
	attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if extra, err := f.parent.DialCarrier(attempt, offer); err == nil {
		extra.Close()
		t.Fatal("sealed generation admitted a new controller")
	}

}

func (f *targetCarrierFixture) waitMachineEvents(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Topics map[machineevents.Topic]int64 `json:"topics"`
	}
	if json.NewDecoder(r.Body).Decode(&request) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	for {
		f.mu.Lock()
		revision, changed := f.eventRevision, f.networkChanged
		f.mu.Unlock()
		if request.Topics[machineevents.TopicMachineAccess] < revision {
			json.NewEncoder(w).Encode(map[string]any{"topics": map[machineevents.Topic]int64{machineevents.TopicMachineAccess: revision}})
			return
		}
		select {
		case <-changed:
		case <-f.events:
			f.mu.Lock()
			revoked := f.revoked
			f.mu.Unlock()
			if revoked {
				w.WriteHeader(http.StatusUnauthorized)
			} else {
				w.WriteHeader(http.StatusServiceUnavailable)
			}
			return
		case <-r.Context().Done():
			return
		}
	}
}
