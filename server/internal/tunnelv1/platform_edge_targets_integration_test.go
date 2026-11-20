package tunnelv1_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
)

func TestPlatformCertificateTargetsRequireEdgeRoleOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	prefix := fmt.Sprintf("cert_roles_%d_", now.UnixNano())
	fixtures := []struct {
		name               string
		roles              []string
		ready, stale, want bool
		scope              string
	}{
		{"edge", []string{"edge"}, true, false, true, ""},
		{"combined", []string{"edge", "relay"}, true, false, true, ""},
		{"relay", []string{"relay"}, true, false, false, ""},
		{"empty", []string{}, true, false, false, ""},
		{"unready", []string{"edge"}, false, false, false, ""},
		{"stale", []string{"edge"}, true, true, false, ""},
		{"global", []string{"edge"}, true, false, true, "global"},
		{"account", []string{"edge"}, true, false, false, "account"},
		{"team", []string{"edge"}, true, false, false, "team"},
	}
	ids := make([]string, 0, len(fixtures))
	region := prefix + "region"
	if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Fixture region',true)`, region); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.operator_regions WHERE id=$1`, region); err != nil {
			t.Error(err)
		}
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.selfhost_installations WHERE node_id=ANY($1::text[])`, ids); err != nil {
			t.Error(err)
		}
		if _, err := database.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=ANY($1::text[])`, ids); err != nil {
			t.Error(err)
		}
	})
	for _, fixture := range fixtures {
		id := prefix + fixture.name
		ids = append(ids, id)
		heartbeat := now
		if fixture.stale {
			heartbeat = now.Add(-2 * time.Minute)
		}
		if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,version,roles,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem) VALUES($1,'default','v1',$2,'ready',$3,$4,1,$5,CASE WHEN cardinality($5::text[])>0 THEN $6 END,CASE WHEN cardinality($5::text[])>0 THEN 'domain' END,CASE WHEN cardinality($5::text[])>0 THEN ARRAY['http2','http3']::text[] ELSE '{}'::text[] END,100,0,$4,CASE WHEN cardinality($5::text[])>0 THEN $4::timestamptz+interval '1 hour' END,'edge.example.test',443,444,'sha256:'||repeat('0',64),'certificate')`, id, "epoch_"+id, fixture.ready, heartbeat, fixture.roles, region); err != nil {
			t.Fatal(err)
		}
		if fixture.scope != "" {
			key := sha256.Sum256([]byte(id))
			if _, err := database.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,$2,$2,$3,'certificate fixture','tunnel',$4,$4,$1,true)`, id, key[:], []byte{1}, fixture.scope); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolver, err := tunnelv1.NewSQLPlatformEdgeTargetResolver(database, time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	targets, err := resolver.Resolve(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := make(map[string]string)
	for _, target := range targets {
		selected[target.NodeID] = target.ProcessEpoch
	}
	for _, fixture := range fixtures {
		id := prefix + fixture.name
		epoch, found := selected[id]
		if found != fixture.want || found && epoch != "epoch_"+id {
			t.Errorf("%s selected=%v epoch=%q; want selected=%v", fixture.name, found, epoch, fixture.want)
		}
	}
	inventory := controlplane.NewEdgeService(database, "fixture-only")
	inventory.SetClock(func() time.Time { return now })
	page, err := inventory.ListHostedEdges(ctx, "fixture_account", prefix, 100)
	if err != nil {
		t.Fatal(err)
	}
	// The inventory lists regions, never nodes: the fixture region appears (its global and managed edges are ready),
	// and no node id does, customer-owned ones included.
	visible := map[string]string{}
	for _, edge := range page.Items {
		if strings.HasPrefix(edge.ID, prefix) {
			visible[edge.ID] = edge.Status
		}
	}
	if visible[region] != "ready" || len(visible) != 1 {
		t.Fatalf("global edge inventory must list the fixture region only: %v", visible)
	}

}
