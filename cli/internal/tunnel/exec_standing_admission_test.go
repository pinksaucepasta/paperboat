package tunnel

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/peertransport/tailnet"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
)

// FIX-EXECLAT (pbvm2 dev-8aafdbab9): every warm `pb exec` to the caller's own machine waited 875-1457 ms for a signed
// network refresh, because the exec's new access session was not in the cached document. The host admits it by its
// standing rule for its own account, and the client dials on the personal pair that stands for it: no request.
func TestWarmExecToOwnMachineSkipsTheNetworkRefresh(t *testing.T) {
	issuer, accountID, cliID, machineID := "https://api.example.test", "account_connected", "cli_connected", "machine_connected"
	store := connectedStore(t)
	identity, _, _ := connectedEndpointAuthority(t, store, issuer, accountID, cliID, machineID)
	defer identity.Clear()
	signerPublic, signerPrivate, _ := ed25519.GenerateKey(rand.Reader)
	self := tailnet.NetworkBinding{AccountID: accountID, EndpointID: cliID, Role: "cli", EndpointGeneration: identity.LocalCertificate.Claims.Generation, QUICCertificateFingerprint: identity.LocalCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.LocalCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::91"}
	client, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: store, Issuer: issuer, Self: self, Keys: connectedNetworkKeys{signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	machineBinding := tailnet.NetworkBinding{AccountID: accountID, EndpointID: machineID, Role: "machine", MachineID: machineID, EndpointGeneration: 1, MachineGeneration: 1, QUICCertificateFingerprint: identity.MachineCertificate.Fingerprint(), QUICPublicKey: base64.RawURLEncoding.EncodeToString(identity.MachineCertificate.Claims.QUICPublicKey), VirtualAddress: "fd7a:115c:a1e0::92"}
	machine, err := tailnet.NewAuthority(tailnet.AuthorityOptions{Store: connectedStore(t), Issuer: issuer, Self: machineBinding, Keys: connectedNetworkKeys{signerPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	if machineBinding.WireGuardPublicKey, machineBinding.KeyGeneration, err = machine.PrepareKey(false); err != nil {
		t.Fatal(err)
	}
	machineBinding.KeyGeneration++
	machineBinding.DiscoPublicKey, _ = machine.DiscoveryPublicKey()
	if self.WireGuardPublicKey, self.KeyGeneration, err = client.PrepareKey(false); err != nil {
		t.Fatal(err)
	}
	self.KeyGeneration++
	self.DiscoPublicKey, _ = client.DiscoveryPublicKey()
	now := time.Now().Unix()
	document := connectedConfiguration(now, issuer, self, machineBinding, "dial")
	document.Peers[0].Scopes = []tailnet.NetworkScope{{ResourceKind: "machine_network", ResourceID: "network_own_pair", ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: tailnet.NetworkPort}}
	if err := client.Apply(t.Context(), connectedSignedJWT(t, signerPrivate, "paperboat-network-config+jwt", document)); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	peer := &PeerTerminalTunnel{nativeRuntime: &cliNativeRuntime{accountID: accountID, endpointID: cliID, identityFingerprint: identity.LocalCertificate.Fingerprint(), authority: client, done: make(chan struct{})}, config: PeerTerminalConfig{Issuer: issuer, Auth: &rotatingNativeAuth{token: "fixture-token"}, HTTPClient: &http.Client{Transport: substrateTransport(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("control plane unavailable")
	})}}}
	target := &resolver.TerminalTarget{EnvironmentID: "env_connected", Auth: resolver.AuthTarget{Token: "credential_exec", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), ResourceID: "access_new_exec"}}
	if _, consumed, err := peer.acquireNativeRuntime(t.Context(), accountID, cliID, identity, hostRulesAdmission(target, machineID, "exec")); err != nil || consumed || requests.Load() != 0 {
		t.Fatalf("warm exec waited for the control plane: requests=%d consumed=%t err=%v", requests.Load(), consumed, err)
	}
	// managed_ssh has no standing dial rule: a credentialed stream whose grant the document lacks still refreshes first.
	if _, _, err := peer.acquireNativeRuntime(t.Context(), accountID, cliID, identity, hostRulesAdmission(target, machineID, "managed_ssh")); err == nil || requests.Load() != 1 {
		t.Fatalf("a grant without a standing rule skipped the refresh: requests=%d err=%v", requests.Load(), err)
	}
}
