package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"github.com/pinksaucepasta/paperboat/shared/relayshare"
)

// A Paperboat-operated relay's lease carries the account speed and names the connected accounts whose workspace pool (edge and relay
// traffic together) is used up this month, each with its plan's slower speed (Free 512 kbit/s, paid 1 Mbit/s); a customer-owned relay
// gets neither. Without the gate a Free account runs at the default 10 Mbit/s and a paid one is listed at 100.
func TestRelayLeaseCarriesAccountSpeedAndPlanSlowTiersPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	free12, paid60, paid120, light := "relay_fu_f12_"+x, "relay_fu_p60_"+x, "relay_fu_p120_"+x, "relay_fu_light_"+x
	both, teamer, team := "relay_fu_both_"+x, "relay_fu_team_"+x, "relay_fu_teamid_"+x
	users := []string{free12, paid60, paid120, light, both, teamer}
	node, node2, own, epoch := "relay_fu_node_"+x, "relay_fu_node2_"+x, "relay_fu_own_"+x, "relay_fu_epoch_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, u := range users {
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id='personal:'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE id='personal:'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id=$1`, u)
		}
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM team_members WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM teams WHERE team_id=$1`, team)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM selfhost_installations WHERE node_id=$1`, own)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id IN ($1,$2,$3)`, node, node2, own)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM relay_subject_sets WHERE node_id IN ($1,$2,$3)`, node, node2, own)
	})
	for _, user := range users {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	}
	for _, user := range []string{paid60, paid120} {
		exec(`INSERT INTO billing_workspaces(id,user_id,state,product_code,quantity,period_start,period_end) VALUES('personal:'||$1,$1,'active','pro-month',1,now()-interval '1 hour',now()+interval '1 hour')`, user)
	}
	// A member of a paid team is on a paid plan too.
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, teamer)
	exec(`INSERT INTO billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES('team:'||$1,$1,'active','team-pro',2,now()-interval '1 hour',now()+interval '1 hour')`, team)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, team, teamer)
	// A free personal workspace is a row too (state free): its usage period references it.
	for _, user := range []string{free12, light} {
		exec(`INSERT INTO billing_workspaces(id,user_id) VALUES('personal:'||$1,$1)`, user)
	}
	now := time.Now().UTC()
	for _, n := range []string{node, node2, own} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,0,$3,$4)`, n, epoch, now, now.Add(time.Hour))
	}
	runtimeHash := sha256.Sum256([]byte(x))
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,$2,$3,'ciphertext','own relay','relay','account',$4,$1,true)`, own, make([]byte, 32), runtimeHash[:], free12)
	period := func(user string, included, consumed int64, paid bool) {
		exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,$4,$5,$6,$2)`, user, now.Add(-time.Hour), now.AddDate(0, 1, 0), included, consumed, paid)
	}
	period(free12, 5_000_000_000, 7_000_000_000, false)     // out of its 5 GB pool
	period(paid60, 100_000_000_000, 60_000_000_000, true)   // under its pool
	period(paid120, 100_000_000_000, 120_000_000_000, true) // out of its 100 GB pool
	period(light, 5_000_000_000, 1_000_000_000, false)

	observeDemand := func(service *EdgeService, id string, demand map[string]relayshare.Demand, accounts ...string) relayNodeLeaseDocument {
		t.Helper()
		subjects := []relaySubject{}
		for i, u := range accounts {
			subjects = append(subjects, relaySubject{AccountID: u, EndpointID: fmt.Sprintf("endpoint_%d", i), Generation: 0})
		}
		out, err := service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: id, Generation: 1, ProcessEpoch: epoch, Ready: true, Subjects: subjects, SubjectsDigest: relaySubjectsDigest(subjects), AccountDemand: demand})
		if err != nil {
			t.Fatalf("observation failed: %v", err)
		}
		return out
	}
	observe := func(service *EdgeService, id string, accounts ...string) relayNodeLeaseDocument {
		t.Helper()
		return observeDemand(service, id, nil, accounts...)
	}
	fair := config.Default().Billing.FairUse
	plain := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	plain.SetFairUse(fair)
	ungated := map[string]float64{}
	out := observe(plain, node, users[0], users[1], users[2], users[3], teamer)
	for _, a := range out.AccountSpeeds {
		ungated[a.Account] = a.Mbit
	}
	if out.AccountMbit != 10 || len(ungated) != 3 || ungated[paid60] != 100 || ungated[paid120] != 100 || ungated[teamer] != 100 {
		t.Fatalf("without the gate: default %v, listed %v; want Free's 10, the two Pro accounts and the Team member at 100", out.AccountMbit, ungated)
	}
	gated := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	gated.SetFairUse(fair)
	gate := bandwidthgate.NewRelayPool(store)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	gated.SetRelayPoolGate(gate)
	got := map[string]float64{}
	for _, a := range observe(gated, node, users[:4]...).AccountSpeeds {
		got[a.Account] = a.Mbit
	}
	if len(got) != 3 || got[free12] != 0.512 || got[paid120] != 1 || got[paid60] != 100 {
		t.Fatalf("listed accounts = %v, want %s at 0.512 (7 of 5 GB, Free pool), %s at 1 (120 of 100 GB, paid pool) and %s at 100 (paid, under its pool); Free %s is at the default", got, free12, paid120, paid60, light)
	}
	if out := observe(gated, own, users[:4]...); out.AccountMbit != 0 || len(out.AccountSpeeds) != 0 {
		t.Fatalf("a customer-owned relay was given limits: %+v", out)
	}

	// An account's speed is ONE budget across relays, divided by what the relays report: the relay whose flows are held back gets
	// everything but the idle one's start allowance (one active flow runs at the plan speed), and the shares add up to the speed (Free's
	// 10, its slower tiers); the customer-owned relay is no carrier.
	shared := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	shared.SetFairUse(fair)
	shared.SetRelayPoolGate(gate)
	held := func(mbit float64) relayshare.Demand {
		return relayshare.Demand{Offered: 2 * mbit * 125000, Held: mbit * 125000}
	}
	observeDemand(shared, own, map[string]relayshare.Demand{both: held(5)}, both)
	busy := map[string]relayshare.Demand{both: held(5), paid120: held(0.5)}
	observeDemand(shared, node2, busy, both, paid120)
	idle := observeDemand(shared, node, map[string]relayshare.Demand{both: {}}, append(users[:4:4], both)...)
	speeds := map[string]float64{}
	for _, a := range idle.AccountSpeeds {
		speeds[a.Account] = a.Mbit
	}
	if speeds[both] != relaylimits.AccountStartMbit || speeds[paid120] != relaylimits.AccountStartMbit || speeds[free12] != 0.512 || speeds[paid60] != 100 {
		t.Fatalf("idle relay's shares = %v, want %s and %s at the start allowance %v, %s at 0.512 (one relay), %s at 100", speeds, both, paid120, relaylimits.AccountStartMbit, free12, paid60)
	}
	speeds = map[string]float64{}
	for _, a := range observeDemand(shared, node2, busy, both, paid120).AccountSpeeds {
		speeds[a.Account] = a.Mbit
	}
	if math.Abs(speeds[both]-(10-relaylimits.AccountStartMbit)) > 1e-9 || speeds[paid120] != 1 {
		t.Fatalf("busy relay's shares = %v, want %s at %v (all but the idle relay's start allowance: 99%% of the plan) and %s at 1 (the other relay does not carry it)", speeds, both, 10-relaylimits.AccountStartMbit, paid120)
	}
}
