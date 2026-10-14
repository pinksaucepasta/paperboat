package environment

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
)

func TestVaultLayerSigningGenerationSurvivesSameWriterCustodyAdvance(t *testing.T) {
	old := fixtureVaultMetadata(t, "layer_writer", 1, make([]byte, 32))
	current := fixtureVaultMetadata(t, "layer_writer", 2, bytes.Repeat([]byte{1}, 32))
	source := VaultScopeState{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: "layer_writer", KeyEpoch: 1, Revision: 1, DocumentID: "sha256:" + strings.Repeat("a", 64)}
	recipient := VaultLayerRecipient{RecipientAccount: "layer_writer", MachineID: "layer_machine", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: b64VaultPublic(bytes.Repeat([]byte{19}, 32)), FenceGeneration: 1}
	raw := signedVaultLayerFixture(t, source, recipient, old)
	if _, err := parseVaultLayer(raw, "https://control.example", current); err != nil {
		t.Fatal("same current signer rejected after custody advance", err)
	}
	future := old
	future.Head.Generation = 3
	if _, err := parseVaultLayer(signedVaultLayerFixture(t, source, recipient, future), "https://control.example", current); err == nil {
		t.Fatal("future signing custody accepted")
	}
	old.Head.Generation = 0
	if _, err := parseVaultLayer(signedVaultLayerFixture(t, source, recipient, old), "https://control.example", current); err == nil {
		t.Fatal("zero signing custody accepted")
	}
	copy(current.WriterPublicKey[:], ed25519.NewKeyFromSeed(bytes.Repeat([]byte{12}, 32)).Public().(ed25519.PublicKey))
	if _, err := parseVaultLayer(raw, "https://control.example", current); err == nil {
		t.Fatal("previous signer accepted after reset")
	}
}
