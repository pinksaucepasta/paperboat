package controlplane

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/bandwidthgate"
)

// F13-03 relay admission: a relay's observation names the connected accounts whose workspaces
// are all out of allowance, so the relay can refuse them; nothing is listed without the gate,
// and listing never changes the observation's other results.
func TestRelayObservationNamesAccountsOutOfBandwidthPostgres(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	capped, open, node, epoch := "relay_obs_capped_"+x, "relay_obs_open_"+x, "relay_obs_node_"+x, "relay_obs_epoch_"+x
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspace_usage_periods WHERE workspace_id IN ('personal:'||$1,'personal:'||$2)`, capped, open)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM billing_workspaces WHERE id IN ('personal:'||$1,'personal:'||$2)`, capped, open)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id=$1`, node)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id IN ($1,$2)`, capped, open)
	})
	for _, user := range []string{capped, open} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
		exec(`INSERT INTO billing_workspaces(id,user_id,state) VALUES('personal:'||$1,$1,'free')`, user)
	}
	now := time.Now().UTC()
	exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,100,100,false,$2)`, capped, now.Add(-time.Hour), now.AddDate(0, 1, 0))
	exec(`INSERT INTO billing_workspace_usage_periods(workspace_id,period_start,period_end,included_bytes,consumed_bytes,paid,budget_start) VALUES('personal:'||$1,$2,$3,100,5,false,$2)`, open, now.Add(-time.Hour), now.AddDate(0, 1, 0))
	exec(`INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],100,0,$3,$4)`, node, epoch, now, now.Add(time.Hour))

	observeAs := func(service *EdgeService, supported bool) relayNodeLeaseDocument {
		t.Helper()
		out, err := service.ObserveRelayNode(ctx, relayNodeObservation{NodeID: node, Generation: 1, ProcessEpoch: epoch, Ready: true, QuotaDeniedSupported: supported, Subjects: []relaySubject{
			{AccountID: capped, EndpointID: "endpoint_a", Generation: 1},
			{AccountID: capped, EndpointID: "endpoint_b", Generation: 1},
			{AccountID: open, EndpointID: "endpoint_c", Generation: 1},
		}})
		if err != nil {
			t.Fatalf("observation failed: %v", err)
		}
		return out
	}
	observe := func(service *EdgeService) relayNodeLeaseDocument { return observeAs(service, true) }

	plain := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	if got := observe(plain).QuotaDenied; len(got) != 0 {
		t.Fatalf("without the gate the observation listed %v", got)
	}
	gated := NewEdgeService(store, "relay-observe-test-credential-0123456789")
	gate := bandwidthgate.New(store)
	gate.SetLookupTimeout(5 * time.Second) // this test database is slower than production
	gated.SetRelayQuotaGate(gate)
	if got := observe(gated).QuotaDenied; !slices.Equal(got, []string{capped}) {
		t.Fatalf("denied accounts = %v, want only %s once", got, capped)
	}
	// Relays decode leases strictly: one that did not announce support is never sent the field.
	if got := observeAs(gated, false).QuotaDenied; len(got) != 0 {
		t.Fatalf("an older relay was sent quota_denied %v", got)
	}
	if got := observe(gated).QuotaDenied; !slices.Equal(got, []string{capped}) {
		t.Fatalf("cached denied accounts = %v, want only %s", got, capped)
	}
	exec(`UPDATE billing_workspace_usage_periods SET consumed_bytes=0 WHERE workspace_id='personal:'||$1`, capped)
	fresh := bandwidthgate.New(store)
	fresh.SetLookupTimeout(5 * time.Second)
	gated.SetRelayQuotaGate(fresh)
	if got := observe(gated).QuotaDenied; len(got) != 0 {
		t.Fatalf("after allowance recovered the observation still listed %v", got)
	}
}
