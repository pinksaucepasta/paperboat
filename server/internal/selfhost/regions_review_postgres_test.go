package selfhost

import (
	"errors"
	"strings"
	"testing"
)

func TestRenameImplicitRegionKeepsTheLimit(t *testing.T) {
	f := newPairFixture(t)
	for i := range regionLimit {
		if _, err := f.service.CreateRegion(f.ctx, f.owner, f.personal, "Region "+string(rune('A'+i/26))+string(rune('a'+i%26))); err != nil {
			t.Fatalf("region %d: %v", i, err)
		}
	}
	// The implicit region of the owner's servers would be a 65th row.
	if _, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own1, "Garage"); !errors.Is(err, ErrLimit) {
		t.Fatalf("first rename past the limit: %v", err)
	}
	// A stored region renames freely at the limit.
	if _, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, "region-aa", "Region again"); err != nil {
		t.Fatalf("rename of a stored region at the limit: %v", err)
	}
}

func TestDeleteRegionHeldByAPoolEntry(t *testing.T) {
	f := newPairFixture(t)
	if _, err := f.service.RenameRegion(f.ctx, f.owner, f.personal, f.own1, "Garage"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.SQL().Exec(`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, f.member)
	})
	// The region's servers are gone, but a workspace pool still lists one of them.
	f.exec(`UPDATE paperboat.selfhost_installations SET state='removed' WHERE id=ANY($1)`, "{"+f.s1+","+f.s1b+"}")
	f.exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'relay','self-hosted-only',ARRAY[$2])`, f.member, f.s1)
	err := f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own1)
	if !errors.Is(err, ErrRegionInUse) || !strings.Contains(err.Error(), "Switch their regions") || strings.Contains(err.Error(), f.member) {
		t.Fatalf("delete of a region a pool uses: %v", err)
	}
	f.exec(`DELETE FROM paperboat.selfhost_pool_policies WHERE scope_id=$1`, f.member)
	if err = f.service.DeleteRegion(f.ctx, f.owner, f.personal, f.own1); err != nil {
		t.Fatalf("free region: %v", err)
	}
}

func TestDeleteTeamRegionIgnoresAMembersOwnSameSlug(t *testing.T) {
	f := newPairFixture(t)
	lab, err := f.service.CreateRegion(f.ctx, f.admin, f.teamScope, "Team lab")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.SQL().Exec(`DELETE FROM paperboat.workspace_edge_sets WHERE scope_id=$1`, f.member)
		_, _ = f.store.SQL().Exec(`DELETE FROM paperboat.selfhost_regions WHERE scope_id=$1`, f.member)
	})
	// The member's pair names the member's own region of the same slug, not the team's.
	f.exec(`INSERT INTO paperboat.selfhost_regions(scope_kind,scope_id,slug,display_name) VALUES('account',$1,$2,'Mine')`, f.member, lab.ID)
	f.exec(`INSERT INTO paperboat.workspace_edge_sets(scope_kind,scope_id,mode,regions) VALUES('account',$1,'self-hosted-only',ARRAY[$2])`, f.member, lab.ID)
	if err = f.service.DeleteRegion(f.ctx, f.admin, f.teamScope, lab.ID); err != nil {
		t.Fatalf("team region with an unrelated personal region of the same slug: %v", err)
	}
}

func TestEnrollRefusesAnImplicitRegionNamedLikeAnother(t *testing.T) {
	f := newEnrollExpectFixture(t)
	owner := Scope{Kind: "account", ID: f.owner}
	r, err := f.service.CreateRegion(f.t.Context(), f.owner, owner, "Home lab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.RenameRegion(f.t.Context(), f.owner, owner, r.ID, "garage"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_regions WHERE scope_id=$1`, f.owner) })
	node := newFakeClaimNode(t, "Relay", fakeRelay("garage"))
	if _, err = f.enroll(node, "", ""); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "rename that region") {
		t.Fatalf("implicit region named like another: %v", err)
	}
	if f.installationCount(node) != 0 {
		t.Fatal("refused claim stored an installation")
	}
}

func TestEnrollRefusesARemovedExpectedRegion(t *testing.T) {
	f := newEnrollExpectFixture(t)
	owner := Scope{Kind: "account", ID: f.owner}
	r, err := f.service.CreateRegion(f.t.Context(), f.owner, owner, "Lab")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteRegion(f.t.Context(), f.owner, owner, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.enrollRaw(newFakeClaimNode(t, "Relay", fakeRelay(r.ID)), r.ID, "relay"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("claim for a removed region: %v", err)
	}
	// Claimed first, the region then cannot be removed; and a claim without an expectation still starts a region.
	if _, err = f.service.CreateRegion(f.t.Context(), f.owner, owner, "Lab"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.exec(`DELETE FROM paperboat.selfhost_regions WHERE scope_id=$1`, f.owner) })
	if _, err = f.enrollRaw(newFakeClaimNode(t, "Relay 2", fakeRelay(r.ID)), r.ID, "relay"); err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteRegion(f.t.Context(), f.owner, owner, r.ID); !errors.Is(err, ErrRegionInUse) {
		t.Fatalf("delete after the claim: %v", err)
	}
	if _, err = f.enroll(newFakeClaimNode(t, "Relay 3", fakeRelay("new-place")), "", ""); err != nil {
		t.Fatalf("implicit region: %v", err)
	}
}

func TestEnrollRefusesAKnownKeyWithOtherComponents(t *testing.T) {
	f := newEnrollExpectFixture(t)
	node := newFakeClaimNode(t, "Box", fakeRelay("box"))
	if _, err := f.enroll(node, "", ""); err != nil {
		t.Fatal(err)
	}
	node.components = []Component{fakeEdge("box")}
	if _, err := f.enroll(node, "", ""); !errors.Is(err, ErrUsed) {
		t.Fatalf("same key, another capability: %v", err)
	}
	other := f.user("other")
	f.owner = other
	if _, err := f.enroll(node, "", ""); !errors.Is(err, ErrUsed) {
		t.Fatalf("same key, another scope: %v", err)
	}
	if f.installationCount(node) != 1 {
		t.Fatal("a second component was stored under the key")
	}
}
