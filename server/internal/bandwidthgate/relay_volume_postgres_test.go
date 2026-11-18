package bandwidthgate

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A relay's slow tier follows the account's relay allowance: its personal workspace's plan amount, and, for a paid team it belongs to,
// the team's pool of per-seat amounts times its seats over all members (the bytes of every member count, wherever they were carried).
func TestRelayVolumePoolsPerSeatForTeamsPostgres(t *testing.T) {
	store := gateStore(t)
	f := newGateFixture(t, store)
	ctx := context.Background()
	x := fmt.Sprint(time.Now().UnixNano())
	other := "relay_vol_o_" + x
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, other)
	f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, f.team, other)
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.resource_bandwidth_events WHERE actor_account_id IN ($1,$2)`, f.user, other)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.team_members WHERE account_id=$1`, other)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, other)
	})
	f.exec(`UPDATE paperboat.billing_workspaces SET state='active',product_code='team-pro',quantity=2,period_start=now()-interval '1 hour',period_end=now()+interval '1 hour' WHERE id=$1`, f.teamWS)
	event := func(workspace, actor, key string, bytes int64) {
		f.exec(`INSERT INTO paperboat.resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
 VALUES($1,$2,'machine','m','m','m','m','relay','managed','n','n',$3,0,now(),now(),$4)`, key+x, workspace, bytes, actor)
	}
	const perSeat, free = 100_000_000_000, 5_000_000_000
	gate := NewRelayVolume(store, free, perSeat)
	over := func(account string) bool {
		t.Helper()
		got, err := gate.lookup(ctx, account, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	// The team's two seats pool 200 GB: 120 GB by one member and 70 GB by the other is under the pool for both.
	event(f.teamWS, f.user, "t1", 120_000_000_000)
	event(f.teamWS, other, "t2", 70_000_000_000)
	if over(f.user) || over(other) {
		t.Fatal("190 GB of a 200 GB team pool slowed a member")
	}
	// Past the pool every member is slowed, whoever carried the bytes.
	event(f.teamWS, other, "t3", 20_000_000_000)
	if !over(f.user) || !over(other) {
		t.Fatal("210 GB of a 200 GB team pool did not slow the members")
	}
	// A third seat raises the pool to 300 GB, and an operator's seats override counts like bought seats.
	f.exec(`UPDATE paperboat.billing_workspaces SET quantity=3 WHERE id=$1`, f.teamWS)
	if over(f.user) {
		t.Fatal("a third seat did not raise the pool")
	}
	f.exec(`INSERT INTO paperboat.billing_workspace_grants(id,workspace_id,kind,amount,reason,created_by) VALUES($1,$2,'seats',1,'test','operator')`, "grant_rv_"+x, f.teamWS)
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.billing_workspace_grants WHERE id=$1`, "grant_rv_"+x)
	})
	if !over(f.user) {
		t.Fatal("a seats override of 1 (100 GB pool, 210 GB used) did not slow the member")
	}
	// The personal workspace is its own pool: a Free account's relay allowance is 5 GB.
	event(f.personal, f.user, "p1", 6_000_000_000)
	f.exec(`DELETE FROM paperboat.billing_workspace_grants WHERE id=$1`, "grant_rv_"+x)
	if !over(f.user) {
		t.Fatal("6 GB of a Free account's 5 GB personal relay allowance did not slow it")
	}
	if over(other) {
		t.Fatal("another member's personal pool was judged on the first account's bytes")
	}
}
