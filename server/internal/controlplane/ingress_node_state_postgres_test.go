package controlplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// The ingress desired-state endpoint serves only verified public edges. A relay or an unverified edge is answered with
// a final, named status, never the retryable 503 it used to poll forever.
func TestIngressDesiredStateIsOnlyForVerifiedEdgesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	stamp := fmt.Sprint(time.Now().UnixNano())
	node := func(name, role string, verified bool) string {
		id := "ingress_state_" + name + "_" + stamp
		verifiedAt := "NULL"
		if verified {
			verifiedAt = "now()"
		}
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,public_ingress_verified_at) VALUES($1,'default','1.0','epoch','ready',true,now(),'region','domain-'||$1,ARRAY[$2]::text[],ARRAY['http3','http2'],100,0,now(),now()+interval '1 hour',`+verifiedAt+`)`, id, role); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
		return id
	}
	service := NewEdgeService(database, "credential")
	for name, want := range map[string]string{
		node("relay", "relay", false):     ingressNodeNotEdge,
		node("relayv", "relay", true):     ingressNodeNotEdge,
		node("unverified", "edge", false): ingressNodeNotVerified,
		node("verified", "edge", true):    ingressNodeVerifiedEdge,
		"ingress_state_missing_" + stamp:  ingressNodeNotEdge,
	} {
		got, err := service.ingressNodeState(ctx, name)
		if err != nil || got != want {
			t.Errorf("node %s: state %q err=%v, want %q", name, got, err, want)
		}
	}
}
