package controlplane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/usermachines"
)

// Edges report usage in the same second (launch has more than two). One machine's bandwidth rows are
// shared by every edge reporting for its environment, and the machine row is also written by heartbeats,
// so concurrent reports used to exhaust the serializable retries and answer 503. Every report must
// succeed, no transaction may give up, and the counters and the debit must be exact.
func TestReconcileUsageFromManyEdgesAtOnceNeverFailsAndCountsExactly(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	seedUsageScope(t, store, suffix)
	userID, machineID := "usage_cc_user_"+suffix, "usage_cc_machine_"+suffix
	cleanupUsageBandwidthScope(t, store, userID, machineID, "ent_"+suffix)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, suffix+"@example.test")
	exec(`INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'machine-test','active',1,1000000000,$4,$5)`, "ent_"+suffix, userID, "sub_"+suffix, now.Add(-time.Hour), now.Add(time.Hour))
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES ($1,$2,$3,'linux','amd64','/home/test','online','occupied',true)`, machineID, userID, "env_"+suffix)
	const edges, reportsPerEdge, bytesPerReport = 8, 25, 1000
	for i := 1; i < edges; i++ {
		exec(`INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch) VALUES ($1,'default','1.0',$2)`, fmt.Sprintf("node_%d_%s", i, suffix), "process_"+suffix)
	}
	t.Cleanup(func() {
		for i := 1; i < edges; i++ {
			_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_usage_receipts WHERE edge_node_id=$1`, fmt.Sprintf("node_%d_%s", i, suffix))
			_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_usage_counters WHERE edge_node_id=$1`, fmt.Sprintf("node_%d_%s", i, suffix))
			_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, fmt.Sprintf("node_%d_%s", i, suffix))
		}
	})
	debit := usermachines.New(store, audit.NewWriter(store), usermachines.Policy{}, nil)
	stop := make(chan struct{})
	var heartbeats sync.WaitGroup
	heartbeats.Add(1)
	go func() { // the machine row is written all the time (state, last seen)
		defer heartbeats.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = store.SQL().ExecContext(ctx, `UPDATE paperboat.user_machines SET online=true, updated_at=now() WHERE id=$1`, machineID)
				time.Sleep(time.Millisecond)
			}
		}
	}()
	before := store.TransactionStats().Serializable.Exhausted
	var wg sync.WaitGroup
	errs := make(chan error, edges*reportsPerEdge)
	for e := 0; e < edges; e++ {
		node := "node_" + suffix
		if e > 0 {
			node = fmt.Sprintf("node_%d_%s", e, suffix)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 1; r <= reportsPerEdge; r++ {
				report := usageReport(fmt.Sprintf("op_cc_%s_%d", node, r), "epoch_cc_"+node, int64(r*bytesPerReport), now)
				report.EdgeNodeID, report.EnvironmentID, report.RouteID = node, "env_"+suffix, "route_"+suffix
				if _, err := ReconcileUsageWithBandwidth(ctx, store, report, now, debit); err != nil {
					errs <- fmt.Errorf("%s report %d: %w", node, r, err)
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	heartbeats.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if exhausted := store.TransactionStats().Serializable.Exhausted - before; exhausted != 0 {
		t.Errorf("%d serializable transactions gave up their retries", exhausted)
	}
	var consumed int64
	if err := store.SQL().QueryRowContext(ctx, `SELECT consumed_included_bytes FROM paperboat.user_machine_bandwidth_periods WHERE user_machine_id=$1`, machineID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if want := int64(edges * reportsPerEdge * bytesPerReport); consumed != want {
		t.Errorf("debited %d bytes, want %d", consumed, want)
	}
}
