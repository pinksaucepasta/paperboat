// compat:target cli/internal/hostruntime/auth
package auth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestCompatCredentialClaimsDecode: every credential the server sends to all clients decodes with
// this release's strict claims decoder (unknown claims refuse the credential).
func TestCompatCredentialClaimsDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COMPAT_ARTIFACTS_DIR"), "credential_claims.json"))
	if err != nil {
		t.Fatal(err)
	}
	var credentials map[string]json.RawMessage
	if err := json.Unmarshal(raw, &credentials); err != nil {
		t.Fatal(err)
	}
	for class, claims := range credentials {
		var decoded Claims
		if err := decodeSegment(base64.RawURLEncoding.EncodeToString(claims), &decoded); err != nil {
			t.Errorf("%s credential refused by this release: %v", class, err)
		}
	}
}
