package hoststate

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHostStateConsumesPreviewTunnelV1CredentialAndGenerationContracts(t *testing.T) {
	fixtures := filepath.Join("..", "..", "..", "testdata", "contracts", "preview-tunnel-v1", "fixtures", "resources.ndjson")
	file, err := os.Open(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	foundSnapshot := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var vector struct {
			Valid    bool            `json:"valid"`
			Resource json.RawMessage `json:"resource"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &vector); err != nil {
			t.Fatal(err)
		}
		if !vector.Valid {
			continue
		}
		var resource struct {
			Kind       string `json:"kind"`
			TunnelID   string `json:"tunnel_id"`
			Generation uint64 `json:"generation"`
		}
		if err := json.Unmarshal(vector.Resource, &resource); err != nil {
			t.Fatal(err)
		}
		switch resource.Kind {
		case "tunnel_config_snapshot":
			foundSnapshot = true
			if _, err := NewConfigSnapshot(resource.TunnelID, resource.Generation, vector.Resource); err != nil {
				t.Fatalf("canonical tunnel config snapshot: %v", err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !foundSnapshot {
		t.Fatal("contract fixture missing tunnel_config_snapshot")
	}
}
