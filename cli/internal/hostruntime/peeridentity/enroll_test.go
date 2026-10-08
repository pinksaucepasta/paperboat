package peeridentity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	identitystore "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/endpointidentity"
)

type staticCredentials struct{}

func (staticCredentials) Token(context.Context) (string, error) { return strings.Repeat("t", 32), nil }
func (staticCredentials) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("proof"), nil
}

func TestEnsureRegistersMachineOwnedMachineKeyWithoutPairedCLI(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "identity")
	store, err := identitystore.Open(identitystore.Config{StateRoot: stateRoot})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	key := store.Current()
	if err := store.SaveRegistration(identitystore.Registration{ServerURL: "https://api.example.test", AccountID: "account_01", MachineID: "machine_01", EnvironmentID: "env_01", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(stateRoot, "inbox"), InstallationGeneration: 3, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	endpoint, err := store.PeerEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	quicPublic := endpoint.QUICPublicKey()
	keyFingerprint := sha256.Sum256(quicPublic)
	keyID := "aek_" + hex.EncodeToString(keyFingerprint[:])
	otherPublic, _, _ := ed25519.GenerateKey(nil)
	otherFingerprint := sha256.Sum256(otherPublic)
	var registered api.EndpointCertificateDocument
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/machine-peer-identity":
			var request struct {
				OperationID string `json:"operation_id"`
				Generation  uint64 `json:"generation"`
				Certificate string `json:"certificate"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Generation != 3 || request.OperationID == "" {
				t.Fatalf("invalid machine registration: %+v %v", request, err)
			}
			raw, err := base64.RawURLEncoding.DecodeString(request.Certificate)
			if err != nil {
				t.Fatal(err)
			}
			certificate, err := endpointidentity.Verify(raw, quicPublic, endpointidentity.Expected{AccountID: "account_01", Role: endpointidentity.RoleMachine, EndpointID: "machine_01", Generation: 3}, now)
			if err != nil || !bytes.Equal(certificate.Claims.QUICPublicKey, endpoint.QUICPublicKey()) {
				t.Fatalf("machine did not prove its own endpoint key: %v", err)
			}
			fingerprint := sha256.Sum256(raw)
			registered = api.EndpointCertificateDocument{Version: 1, AccountID: "account_01", KeyID: keyID, EndpointID: "machine_01", Role: "machine", Generation: 3, Serial: certificate.Claims.Serial, IssuedAt: certificate.Claims.IssuedAt.Format(time.RFC3339), ExpiresAt: certificate.Claims.ExpiresAt.Format(time.RFC3339), Certificate: request.Certificate, CertificateFingerprint: hex.EncodeToString(fingerprint[:])}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": registered})
		case "/v1/machine-peer-identity/status":
			if registered.Certificate == "" {
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"data":{"state":"pending"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"state": "approved", "trusted_keys": []api.E2EEKey{
				{KeyID: keyID, PublicKey: base64.RawURLEncoding.EncodeToString(quicPublic), Fingerprint: hex.EncodeToString(keyFingerprint[:]), Generation: 1},
				{KeyID: "aek_" + hex.EncodeToString(otherFingerprint[:]), PublicKey: base64.RawURLEncoding.EncodeToString(otherPublic), Fingerprint: hex.EncodeToString(otherFingerprint[:]), Generation: 1},
			}, "certificate": registered}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(Config{ControlURL: server.URL, StateRoot: stateRoot, Transport: server.Client().Transport, Clock: func() time.Time { return now }}, staticCredentials{})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	stored, err := store.PeerEndpoint()
	if err != nil || len(stored.Certificate) == 0 || len(stored.TrustedKeys) != 2 || stored.RootKeyID != keyID {
		t.Fatalf("stored machine identity: cert=%d keys=%d key_id=%s err=%v", len(stored.Certificate), len(stored.TrustedKeys), stored.RootKeyID, err)
	}
	// A state file without its trust directory refreshes through the authenticated
	// machine status route; it does not need a CLI signer to come online.
	path := filepath.Join(stateRoot, "peer-endpoint.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	delete(state, "trusted_keys")
	raw, _ = json.Marshal(state)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.PeerEndpoint()
	if err != nil || len(refreshed.TrustedKeys) != 2 {
		t.Fatalf("trust refresh keys=%d err=%v", len(refreshed.TrustedKeys), err)
	}
	soon, err := endpointidentity.Sign(endpoint.QUICPrivateKey, endpointidentity.Claims{AccountID: "account_01", Role: endpointidentity.RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 3, Serial: 1, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(6 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	soonRaw, err := soon.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SavePeerEndpointCertificateWithTrustedKeys(quicPublic, refreshed.TrustedKeys, soonRaw, now); err != nil {
		t.Fatal(err)
	}
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatalf("renew machine certificate: %v", err)
	}
	if registered.Serial != 2 {
		t.Fatalf("renewed serial=%d", registered.Serial)
	}
	legacyPublic, legacyPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyFingerprint := sha256.Sum256(legacyPublic)
	legacy, err := endpointidentity.Sign(legacyPrivate, endpointidentity.Claims{AccountID: "account_01", Role: endpointidentity.RoleMachine, EndpointID: "machine_01", QUICPublicKey: quicPublic, Generation: 3, Serial: 2, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(90 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	legacyRaw, err := legacy.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	legacyKey := endpointidentity.TrustedKey{KeyID: "aek_" + hex.EncodeToString(legacyFingerprint[:]), PublicKey: legacyPublic, Fingerprint: legacyFingerprint, Generation: 1}
	if err := store.SavePeerEndpointCertificateWithTrustedKeys(legacyPublic, append(refreshed.TrustedKeys, legacyKey), legacyRaw, now); err != nil {
		t.Fatal(err)
	}
	if err := client.Ensure(context.Background()); err != nil {
		t.Fatalf("renew old account-signed certificate: %v", err)
	}
	if registered.Serial != 3 {
		t.Fatalf("renewed old certificate serial=%d", registered.Serial)
	}
	updated, err := store.PeerEndpoint()
	if err != nil || updated.Generation != endpoint.Generation || !bytes.Equal(updated.QUICPublicKey(), quicPublic) {
		t.Fatalf("endpoint identity changed during renewal: %v", err)
	}
}
