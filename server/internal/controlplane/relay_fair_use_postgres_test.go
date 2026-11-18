package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A Paperboat-operated relay's lease carries the account speed and names the connected accounts that carried their plan's included
// relay bytes this month, each with its plan's slower speed (Free 5 GB then 512 kbit/s, paid 100 GB then 1 Mbit/s); a customer-owned
// relay gets neither, and nothing is listed without the gate.
func TestRelayLeaseCarriesAccountSpeedAndPlanSlowTiersPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	free12, paid60, paid120, light := "relay_fu_f12_"+x, "relay_fu_p60_"+x, "relay_fu_p120_"+x, "relay_fu_light_"+x
	both := "relay_fu_both_" + x
	users := []string{free12, paid60, paid120, light, both}
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
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM resource_bandwidth_events WHERE actor_account_id=$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE id='personal:'||$1`, u)
			_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id=$1`, u)
		}
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
	now := time.Now().UTC()
	for _, n := range []string{node, node2, own} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,0,$3,$4)`, n, epoch, now, now.Add(time.Hour))
	}
	runtimeHash := sha256.Sum256([]byte(x))
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,$2,$3,'ciphertext','own relay','relay','account',$4,$1,true)`, own, make([]byte, 32), runtimeHash[:], free12)
	event := func(user, key string, bytes int64, end time.Time) {
		exec(`INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
 VALUES($1,'personal:'||$2,'machine','m','m','m','m','relay','managed',$3,'n',$4,0,$5,$5,$2)`, key+x, user, node, bytes, end)
	}
	event(free12, "f12", 7_000_000_000, now.Add(-time.Minute))
	event(paid60, "p60", 60_000_000_000, now.Add(-time.Minute))
	event(paid120, "p120", 120_000_000_000, now.Add(-time.Minute))
	event(light, "l1", 1_000_000_000, now.Add(-time.Minute))
	event(light, "l0", 90_000_000_000, now.AddDate(0, -1, -1)) // last month's bytes do not count

	observe := func(service *EdgeService, id string, accounts ...string) relayNodeLeaseDocument {
		t.Helper()
		subjects := []relaySubject{}
		for i, u := range accounts {
			subjects = append(subjects, relaySubject{AccountID: u, EndpointID: fmt.Sprintf("endpoint_%d", i), Generation: 0})
		}
		out, err := service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: id, Generation: 1, ProcessEpoch: epoch, Ready: true, Subjects: subjects, SubjectsDigest: relaySubjectsDigest(subjects)})
		if err != nil {
			t.Fatalf("observation failed: %v", err)
		}
		return out
	}
	fair := config.Default().Billing.FairUse
	plain := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	plain.SetFairUse(fair)
	if out := observe(plain, node, users[:4]...); out.AccountMbit != 10 || len(out.AccountSpeeds) != 0 {
		t.Fatalf("without the gate: speed %v, slow %v; want 10 and none", out.AccountMbit, out.AccountSpeeds)
	}
	gated := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	gated.SetFairUse(fair)
	gate := bandwidthgate.NewRelayVolume(store, fair.RelayFreeIncludedBytes, fair.RelayIncludedBytes)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	gated.SetRelayVolumeGate(gate)
	got := map[string]float64{}
	for _, a := range observe(gated, node, users[:4]...).AccountSpeeds {
		got[a.Account] = a.Mbit
	}
	if len(got) != 2 || got[free12] != 0.512 || got[paid120] != 1 {
		t.Fatalf("slow accounts = %v, want %s at 0.512 (7 of 5 GB, Free) and %s at 1 (120 of 100 GB, paid); %s is under its plan's amount", got, free12, paid120, paid60)
	}
	if out := observe(gated, own, users[:4]...); out.AccountMbit != 0 || len(out.AccountSpeeds) != 0 {
		t.Fatalf("a customer-owned relay was given limits: %+v", out)
	}

	// An account's speed is ONE bucket across relays: an account on two relays gets half of its speed on each (the fast speed, or its plan's
	// slower one), so the shares add up to it; the customer-owned relay does not count as one of them.
	shared := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	shared.SetFairUse(fair)
	shared.SetRelayVolumeGate(gate)
	observe(shared, node2, both, paid120)
	observe(shared, own, both)
	first := observe(shared, node, append(users[:4:4], both)...)
	speeds := map[string]float64{}
	for _, a := range first.AccountSpeeds {
		speeds[a.Account] = a.Mbit
	}
	if speeds[both] != 5 || speeds[paid120] != 0.5 || speeds[free12] != 0.512 {
		t.Fatalf("shares = %v, want %s at 5 (10 Mbit/s over two relays), %s at 0.5 (its 1 Mbit/s tier over two relays), %s at 0.512 (one relay)", speeds, both, paid120, free12)
	}
}
