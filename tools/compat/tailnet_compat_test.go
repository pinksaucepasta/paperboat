// compat:target cli/internal/peertransport/tailnet
package tailnet

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCompatRegionalCandidatesDecode: the signed regional candidate set decodes with this release's
// strict decoder; a refusal drops every relay candidate.
func TestCompatRegionalCandidatesDecode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(os.Getenv("COMPAT_ARTIFACTS_DIR"), "regional_candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	var set RegionalCandidates
	if err := strictDecode(raw, &set); err != nil {
		t.Fatalf("regional candidate set refused by this release: %v", err)
	}
	if len(set.Nodes) == 0 {
		t.Fatal("artifact has no nodes")
	}
}
