package config

import (
	"strings"
	"testing"
)

// F21-56: with more than one server instance, generated (per-process) mint signing keys would sign
// tokens the other instances cannot verify, so startup refuses them.
func TestValidationRefusesGeneratedSigningKeysForSeveralInstances(t *testing.T) {
	cfg := Default()
	cfg.ServerInstances = 3
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "server_instances above 1 requires shared mint signing keys") {
		t.Fatalf("validation error = %v", err)
	}
	cfg.Secrets.MintSigningKeys = []string{"configured"}
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "server_instances") {
		t.Fatalf("shared keys were refused: %v", err)
	}
	cfg.ServerInstances = 1
	cfg.Secrets.MintSigningKeys = nil
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "server_instances") {
		t.Fatalf("one instance was refused: %v", err)
	}
}
