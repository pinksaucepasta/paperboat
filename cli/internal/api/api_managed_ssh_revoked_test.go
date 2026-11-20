package api

import (
	"fmt"
	"testing"
)

// A machine with more revoked key IDs than the old 4096 cap is still accepted.
func TestValidRevokedKeyIDsHasNoCountCap(t *testing.T) {
	ids := make([]string, 5000)
	for index := range ids {
		ids[index] = fmt.Sprintf("paperboat:user_a:session_%05d:machine_1", index)
	}
	if !validRevokedKeyIDs(ids, "machine_1") {
		t.Fatal("5000 revoked key IDs for the machine were refused")
	}
}
