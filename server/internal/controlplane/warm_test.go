package controlplane

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Warm answers the ingress projection for the verified edges and opens the pool's connections; both are reads.
func TestWarmRunsTheIngressProjectionAndOpensConnections(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	node := "node_warm_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes (id,edge_pool,protocol_version,process_epoch,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,public_ingress_verified_at) VALUES ($1,'default','1.0',$2,'test','test',ARRAY['edge'],ARRAY['http2'],10,0,now(),now()+interval '1 hour',now())`, node, "process_"+suffix); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	})
	if err := store.WarmPool(ctx, 4); err != nil {
		t.Fatal(err)
	}
	if total := store.Pool().Stat().TotalConns(); total < 4 {
		t.Fatalf("the pool holds %d connections after warming 4", total)
	}
	if err := NewEdgeService(store, "credential").Warm(ctx, 3); err != nil {
		t.Fatalf("warm-up failed for a verified edge: %v", err)
	}
}
