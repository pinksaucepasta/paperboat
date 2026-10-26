package peersessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEmitCompatNetworkConfiguration writes, to $COMPAT_ARTIFACTS_DIR, the network configuration claims a
// recipient that never declared endpoints receives (tools/compat decodes it with the oldest official release):
// a peer with delivered endpoints, built by the production claims function, must not carry them.
func TestEmitCompatNetworkConfiguration(t *testing.T) {
	dir := os.Getenv("COMPAT_ARTIFACTS_DIR")
	if dir == "" {
		t.Skip("set COMPAT_ARTIFACTS_DIR to emit compat artifacts for tools/compat")
	}
	now := time.Now().UTC().Truncate(time.Second)
	binding := func(id, address string) networkBinding {
		return networkBinding{AccountID: "usr_compat", EndpointID: id, Role: "machine", MachineID: id, EndpointGeneration: 1, MachineGeneration: 1, KeyGeneration: 1,
			WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", DiscoPublicKey: "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE",
			QUICCertificateFingerprint: "0000000000000000000000000000000000000000000000000000000000000000", QUICPublicKey: "AgICAgICAgICAgICAgICAgICAgICAgICAgICAgICAgI",
			VirtualAddress: address, Endpoints: []string{"203.0.113.7:41641"}}
	}
	self, peer := binding("machine_self", "100.64.0.1"), binding("machine_peer", "100.64.0.2")
	expiry := now.Add(5 * time.Minute)
	peers := []networkPeer{{Identity: peer, Scopes: []networkScope{personalNetworkScope(self, peer, expiry)}}}
	raw, err := json.MarshalIndent(configurationClaims("https://api.compat.example.test", now, expiry, 1, self, peers, false), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network_configuration.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
