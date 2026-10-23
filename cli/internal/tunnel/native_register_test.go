package tunnel

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
)

// N2: the daemon registered its peer identity on its first native operation, so every existing machine
// had to learn a new peer while the user's first command waited (the first ping from a just-paired
// machine lost its whole 10 s sample). The daemon now registers when it starts; the engine's own
// registration on first use must then send the same key, so the server sees no new identity and its
// peers have nothing left to learn.
func TestRegistrationAtStartIsTheIdentityTheEngineLaterRegisters(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	issuer := "https://api.example.test"
	accountID, cliID, machineID := "account_register", "cli_register", "machine_register"
	store := connectedStore(t)
	identity, _, _ := connectedEndpointAuthority(t, store, issuer, accountID, cliID, machineID)
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().Unix()
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
	var mu sync.Mutex
	var registrations []api.PeerNetworkRegistration
	var network string
	networkAPI := connectedNetworkAPI{register: func(request api.PeerNetworkRegistration) (api.PeerNetworkRegistrationResult, error) {
		mu.Lock()
		defer mu.Unlock()
		// Like the server: a new key advances the key generation, the registered key keeps it.
		generation := request.ExpectedKeyGeneration + 1
		if len(registrations) > 0 && registrations[len(registrations)-1].WireGuardPublicKey == request.WireGuardPublicKey {
			generation = request.ExpectedKeyGeneration
		}
		registrations = append(registrations, request)
		self := tailnet.NetworkBinding{AccountID: accountID, EndpointID: cliID, Role: "cli", EndpointGeneration: 1, KeyGeneration: generation, WireGuardPublicKey: request.WireGuardPublicKey, DiscoPublicKey: request.DiscoPublicKey, QUICCertificateFingerprint: request.QUICCertificateFingerprint, QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::91"}
		network = connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", connectedConfiguration(now, issuer, self, machine, "dial"))
		return api.PeerNetworkRegistrationResult{KeyGeneration: self.KeyGeneration, VirtualAddress: self.VirtualAddress}, nil
	}, config: func() (api.PeerNetworkConfigurationResult, error) {
		mu.Lock()
		defer mu.Unlock()
		return api.PeerNetworkConfigurationResult{Configuration: network}, nil
	}}
	jwks := &http.Client{Transport: connectedRoundTripper(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/.well-known/jwks.json" {
			return nil, errors.New("unexpected HTTP request")
		}
		body := fmt.Sprintf(`{"keys":[{"kty":"OKP","crv":"Ed25519","use":"sig","alg":"EdDSA","kid":"connected_key","x":"%s"}]}`, base64.RawURLEncoding.EncodeToString(signerPublic))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	relayTrust := &tls.Config{MinVersion: tls.VersionTLS13}
	for i := range 2 { // at daemon start, then on the first native operation
		authority, _, _, err := newCLINativeAuthority(ctx, issuer, store, networkAPI, jwks, relayTrust, accountID, cliID, identity)
		if err != nil {
			t.Fatalf("registration %d: %v (%d registered)", i+1, err, len(registrations))
		}
		if err := authority.Close(); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(registrations) != 2 {
		t.Fatalf("%d registrations, want 2", len(registrations))
	}
	first, second := registrations[0], registrations[1]
	if first.WireGuardPublicKey != second.WireGuardPublicKey || first.DiscoPublicKey != second.DiscoPublicKey || first.QUICCertificateFingerprint != second.QUICCertificateFingerprint {
		t.Fatalf("the first native operation registered a different identity than the daemon registered at start (key same %v, disco same %v)", first.WireGuardPublicKey == second.WireGuardPublicKey, first.DiscoPublicKey == second.DiscoPublicKey)
	}
}
