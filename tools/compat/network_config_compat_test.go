// compat:target cli/internal/peertransport/tailnet
package tailnet

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCompatNetworkConfigurationDecodes: the signed network configuration a recipient that never declared
// endpoints receives decodes with this release's strict decoder; a refusal leaves the engine with no peers.
func TestCompatNetworkConfigurationDecodes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COMPAT_ARTIFACTS_DIR"), "network_configuration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg NetworkConfiguration
	if err := strictDecode(raw, &cfg); err != nil {
		t.Fatalf("network configuration refused by this release: %v", err)
	}
	if len(cfg.Peers) == 0 {
		t.Fatal("artifact has no peers")
	}
}
