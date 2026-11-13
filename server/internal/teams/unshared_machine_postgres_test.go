package teams

import "testing"

// An unshared machine (its binding inactive) is no longer one of the team's machines: the machine page, its total
// and the team summary's machine_count leave it out. They counted and listed it, so it still showed as shared.
func TestPostgresUnsharedMachineLeavesTheTeamPages(t *testing.T) {
	f := newPolicyFixture(t)
	before, err := f.service.Machines(f.ctx, f.owner, f.team, "", 100)
	if err != nil || before.Total < 2 {
		t.Fatalf("fixture machines: total %d, err %v", before.Total, err)
	}
	unshared := before.Items[0].MachineID
	f.exec(`UPDATE paperboat.team_resource_bindings SET active=false,generation=generation+1 WHERE team_id=$1 AND resource_kind='machine' AND resource_id=$2`, f.team, unshared)
	page, err := f.service.Machines(f.ctx, f.owner, f.team, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range page.Items {
		if m.MachineID == unshared {
			t.Fatal("the unshared machine is still listed on the team's machine page")
		}
	}
	summary, err := f.service.Get(f.ctx, f.owner, f.team)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != before.Total-1 || len(page.Items) != before.Total-1 || summary.MachineCount != before.Total-1 {
		t.Fatalf("after unsharing one of %d machines: page total %d, listed %d, machine_count %d; want %d", before.Total, page.Total, len(page.Items), summary.MachineCount, before.Total-1)
	}
}
