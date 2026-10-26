// compat:target cli/internal/hostruntime/connector
package connector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/auth"
)

// TestCompatConnectorAdmissionPolicy: the published file transfer policy passes this release's
// connector admission check; a refusal here drops the machine's edge connector entirely.
func TestCompatConnectorAdmissionPolicy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COMPAT_ARTIFACTS_DIR"), "file_transfer_policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy auth.FileTransferPolicy
	if err := strictJSON(raw, &policy); err != nil {
		t.Fatalf("policy refused by this release's strict decode: %v", err)
	}
	if !validFileTransferPolicy(policy) {
		t.Fatalf("policy refused by this release's connector admission check: %+v", policy)
	}
}
