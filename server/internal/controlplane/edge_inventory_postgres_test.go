package controlplane

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestHostedEdgeInventoryAppliesAccountAndFreshnessOnPostgres(t *testing.T) {
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
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	prefix := "zz_edgeinv_" + strings.ReplaceAll(now.Format("20060102150405.000000"), ".", "") + "_"
	account := "acct_" + prefix
	regionID := "edgeinv_" + now.Format("20060102150405")
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.selfhost_pool_policies WHERE scope_kind='account' AND scope_id=$1`, account)
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.selfhost_installations WHERE node_id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.managed_node_authority WHERE node_id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, prefix+"%")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.operator_regions WHERE id=$1 OR id=$2`, regionID, regionID+"_on")
		_, _ = database.SQL().ExecContext(cleanupCtx, `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$2,$3,'active')`, account, "workos_"+account, account+"@example.test")
	for _, item := range []struct {
		suffix, state string
		ready         bool
		heartbeat     time.Time
		roles         string
	}{{"a", "ready", true, now, "edge"}, {"b", "ready", true, now.Add(-16 * time.Second), "edge"}, {"c", "draining", false, now, "edge"}, {"d", "ready", true, now, "edge"}, {"e", "ready", true, now, "edge"}, {"f", "ready", true, now, "edge"}, {"g", "ready", true, now, "relay"}} {
		exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem)
VALUES($1,'default','1.0','epoch', $2,$3,$4,'fsn1','domain',ARRAY[$5]::text[],ARRAY['http3','http2']::text[],100,0,$4,$4::timestamptz+interval '1 hour','edge.example.test',443,444,$6,'certificate')`, prefix+item.suffix, item.state, item.ready, item.heartbeat, item.roles, "sha256:"+strings.Repeat("0", 64))
	}
	exec(`UPDATE paperboat.control_tunnel_nodes SET allowed_account_ids=ARRAY['different_account'] WHERE id=$1`, prefix+"d")
	// An account-scoped self-hosted installation is not a hosted edge.
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'encrypted','test edge','tunnel','account',$4,$5,true)`, prefix+"installation", prefix+"e", make([]byte, 32), account, prefix+"claim")
	exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Test region',false)`, regionID)
	exec(`INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,$3,'encrypted','{}'::jsonb)`, prefix+"f", regionID, make([]byte, 32))
	// Edge a is a managed node of an enabled region without a site: it shows the region's display name.
	exec(`UPDATE paperboat.control_tunnel_nodes SET endpoint_host='edge.example.test',endpoint_tcp_port=443,endpoint_quic_port=444 WHERE id=$1`, prefix+"a")
	exec(`UPDATE paperboat.control_tunnel_nodes SET site='Mumbai' WHERE id=$1`, prefix+"b")
	// Edge c is one of Paperboat's own pbh installations (no operator row): its name never reaches users.
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'encrypted','oracle','tunnel','global','global',$4,true)`, prefix+"ginstallation", prefix+"c", append([]byte{2}, make([]byte, 31)...), prefix+"gclaim")
	exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Test region',true)`, regionID+"_on")
	exec(`INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,$3,'encrypted','{}'::jsonb)`, prefix+"a", regionID+"_on", append([]byte{1}, make([]byte, 31)...)) // the runtime hash is unique
	service := NewEdgeService(database, "unused")
	service.SetClock(func() time.Time { return now })
	first, err := service.ListHostedEdges(ctx, account, prefix, 2)
	if err != nil || len(first.Items) != 2 || first.Items[0].ID != prefix+"a" || first.Items[0].Status != "ready" || first.Items[1].Status != "unavailable" || first.Items[0].Site != "Test region" || first.Items[0].EndpointHost != "edge.example.test" || first.Items[1].Site != "Mumbai" || first.NextCursor != prefix+"b" {
		t.Fatalf("first page=%+v error=%v", first, err)
	}
	second, err := service.ListHostedEdges(ctx, account, first.NextCursor, 2)
	if err != nil || len(second.Items) != 1 || second.Items[0].ID != prefix+"c" || second.Items[0].Status != "draining" || second.Items[0].Site != "" || second.NextCursor != "" {
		t.Fatalf("second page=%+v error=%v", second, err)
	}
	exec(`INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account',$1,'relay','self-hosted-only',ARRAY[]::text[])`, account)
	fallback, err := service.ListHostedEdges(ctx, account, prefix, 2)
	if err != nil || len(fallback.Items) != 2 || fallback.Items[0].ID != prefix+"a" {
		t.Fatalf("hosted display inventory under strict pool=%+v error=%v", fallback, err)
	}
}
