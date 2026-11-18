package controlplane

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
)

// A Paperboat-operated relay's lease carries the fair-use speeds and names the connected accounts that carried the month's
// included relay bytes (its slow tier); a customer-owned relay gets neither, and nothing is listed without the gate.
func TestRelayLeaseCarriesFairUseSpeedsAndSlowAccountsPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	heavy, light, node, own, epoch := "relay_fu_heavy_"+x, "relay_fu_light_"+x, "relay_fu_node_"+x, "relay_fu_own_"+x, "relay_fu_epoch_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM resource_bandwidth_events WHERE actor_account_id IN ($1,$2)`, heavy, light)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM selfhost_installations WHERE node_id=$1`, own)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id IN ($1,$2)`, node, own)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id IN ($1,$2)`, heavy, light)
	})
	for _, user := range []string{heavy, light} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	}
	now := time.Now().UTC()
	for _, n := range []string{node, own} {
		exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,0,$3,$4)`, n, epoch, now, now.Add(time.Hour))
	}
	runtimeHash := sha256.Sum256([]byte(x))
	exec(`INSERT INTO selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,$2,$3,'ciphertext','own relay','relay','account',$4,$1,true)`, own, make([]byte, 32), runtimeHash[:], heavy)
	// heavy carried 60 GB through managed relays this month, light 1 GB; last month's bytes do not count.
	event := func(user, key string, bytes int64, end time.Time) {
		exec(`INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,actor_account_id)
 VALUES($1,'personal:'||$2,'machine','m','m','m','m','relay','managed',$3,'n',$4,0,$5,$5,$2)`, key+x, user, node, bytes, end)
	}
	event(heavy, "h1", 60_000_000_000, now.Add(-time.Minute))
	event(light, "l1", 1_000_000_000, now.Add(-time.Minute))
	event(light, "l0", 90_000_000_000, now.AddDate(0, -1, -1))

	observe := func(service *EdgeService, id string) relayNodeLeaseDocument {
		t.Helper()
		out, err := service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: id, Generation: 1, ProcessEpoch: epoch, Ready: true, Subjects: []relaySubject{
			{AccountID: heavy, EndpointID: "endpoint_a", Generation: 1},
			{AccountID: light, EndpointID: "endpoint_b", Generation: 1},
		}})
		if err != nil {
			t.Fatalf("observation failed: %v", err)
		}
		return out
	}
	fair := config.Default().Billing.FairUse
	plain := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	plain.SetFairUse(fair)
	if out := observe(plain, node); out.AccountMbit != 20 || out.SlowMbit != 2 || len(out.SlowAccounts) != 0 {
		t.Fatalf("without the gate: speeds %v/%v, slow %v; want 20/2 and none", out.AccountMbit, out.SlowMbit, out.SlowAccounts)
	}
	gated := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	gated.SetFairUse(fair)
	gate := bandwidthgate.NewRelayVolume(store, fair.RelayIncludedBytes)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	gated.SetRelayVolumeGate(gate)
	if out := observe(gated, node); !slices.Equal(out.SlowAccounts, []string{heavy}) {
		t.Fatalf("slow accounts = %v, want only %s (60 GB of 50 GB; light is at 1 GB this month)", out.SlowAccounts, heavy)
	}
	if out := observe(gated, own); out.AccountMbit != 0 || out.SlowMbit != 0 || len(out.SlowAccounts) != 0 {
		t.Fatalf("a customer-owned relay was given limits: %+v", out)
	}
}
