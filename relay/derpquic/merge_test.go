package derpquic

import (
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// A merge never changes the grant it extends: a connection's grant, its fence and pair leases still hold it, and an
// in-place append or refreshed entry moved their memberships under them (audit A P2-4). A membership named again
// keeps the newest expiry, and a refused merge leaves the admitted grant unchanged.
func TestMergeGrantsLeavesTheCurrentGrantUnchanged(t *testing.T) {
	now := time.Now()
	old, renewed := now.Add(time.Hour).Unix(), now.Add(2*time.Hour).Unix()
	current := Grant{RelayGrant: peernetwork.RelayGrant{Generation: 3, Memberships: make([]Scope, 0, 8)}}
	current.Memberships = append(current.Memberships, scope("machine_access", "a", "terminal", "dial", old))
	current = current.withIndex()
	next := Grant{RelayGrant: peernetwork.RelayGrant{Generation: 3, ExpiresAt: renewed, Memberships: []Scope{scope("machine_access", "a", "terminal", "dial", renewed), scope("machine_access", "b", "terminal", "dial", old)}}}
	merged, err := mergeGrants(current, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Memberships) != 2 || merged.Memberships[0].ExpiresAt != renewed || merged.ExpiresAt != renewed {
		t.Fatalf("merged %+v", merged.Memberships)
	}
	if current.Memberships[0].ExpiresAt != old || len(current.Memberships[:cap(current.Memberships)][:2][1].ResourceID) != 0 || len(current.index.memberships.hashes) != 1 {
		t.Fatal("merging changed the grant it extended")
	}
	over := make([]Scope, peernetwork.MaxMergedMemberships)
	for i := range over {
		over[i] = scope("machine_access", string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)), "terminal", "dial", old)
	}
	if _, err := mergeGrants(merged, Grant{RelayGrant: peernetwork.RelayGrant{Memberships: over}}); !errors.Is(err, ErrAdmission) {
		t.Fatalf("merging past MaxMergedMemberships: %v", err)
	}
	if len(merged.Memberships) != 2 {
		t.Fatal("a refused merge changed the admitted grant")
	}
}
