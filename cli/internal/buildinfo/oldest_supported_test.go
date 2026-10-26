package buildinfo

import (
	"os"
	"strings"
	"testing"
)

func TestOldestSupportedOfficialMatchesTheCompatibilityBaseline(t *testing.T) {
	baseline, err := os.ReadFile("../../../tools/compat/OLDEST_OFFICIAL")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(baseline)); got != OldestSupportedOfficial {
		t.Fatalf("tools/compat/OLDEST_OFFICIAL = %q, buildinfo.OldestSupportedOfficial = %q: change both together", got, OldestSupportedOfficial)
	}
}
