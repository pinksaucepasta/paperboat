package selfhost_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// This opt-in read-only deployment check verifies the explicitly configured
// infrastructure hosts; it never migrates state or reads credential material.
func TestLiveGlobalNodeReadinessDeploymentProbe(t *testing.T) {
	hosts := strings.Fields(os.Getenv("PAPERBOAT_LIVE_GLOBAL_NODE_HOSTS"))
	if len(hosts) == 0 {
		t.Skip("explicit deployed global-node hosts required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg, err := config.Load(ctx, config.LoadOptions{})
	if err != nil {
		t.Fatal("load deployed configuration failed")
	}
	database, err := db.Open(cfg.Database)
	if err != nil {
		t.Fatal("open deployed node database failed")
	}
	defer database.Close()
	seen := map[string]bool{}
	for _, host := range hosts {
		if seen[host] {
			t.Fatal("duplicate configured deployment host")
		}
		seen[host] = true
		for _, role := range []string{"relay", "tunnel"} {
			var count int
			heartbeatAfter := time.Now().UTC().Add(-controlplane.ControlTunnelNodeStaleAfter())
			err = database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.selfhost_installations i
JOIN paperboat.control_tunnel_nodes n ON n.id=i.node_id
WHERE i.scope_kind='global' AND i.state='active' AND i.claim_complete
AND (i.expires_at IS NULL OR i.expires_at>now())
AND i.capability=$1 AND n.endpoint_host=$2 AND n.state='ready' AND n.ready
AND n.process_epoch<>'' AND n.last_heartbeat_at>$3`, role, host, heartbeatAfter).Scan(&count)
			if err != nil || count != 1 {
				t.Fatalf("configured host %s: expected one current ready global %s", host, role)
			}
		}
	}
	t.Logf("%d configured hosts: global relay and tunnel claims, current epochs and fresh ready heartbeats verified", len(hosts))
}
