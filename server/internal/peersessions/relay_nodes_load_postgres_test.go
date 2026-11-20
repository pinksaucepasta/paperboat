package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// Inside a region the account's relay candidates come least loaded first (the daemon still ranks by round trip),
// a node that reported no load sorts last, and regions keep their own group.
func TestRelayNodesLeastLoadedFirstWithinRegion(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "load-test", PrivateKey: signing}}, "load-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(now.UnixNano())
	account := "load_user_" + suffix
	regionA, regionB := "lra_"+suffix, "lrb_"+suffix
	// Ids sort a1, a2, a3 and b1, b2: the load order differs from the id order in both regions.
	add := func(id, region string, cpu *float64) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids,peer_relay_wireguard_public_key,peer_relay_disco_public_key,peer_relay_virtual_address) VALUES($1,'load','v1','epoch',$2,443,444,'ready',true,$3,1,$4,$4||'-a',ARRAY['relay','peer_relay'],ARRAY['derp_quic','peer_relay_udp'],100,1,$3,$5,ARRAY[$6],$7,$8,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')))`, id, id+".example.test", now, region, now.Add(time.Minute), account, make32(), make32()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.node_capacity WHERE node_id=$1`, id)
			store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
		if cpu != nil {
			if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,cpu_sustained_percent,reported_at) VALUES($1,'relay','epoch',$2,now())`, id, *cpu); err != nil {
				t.Fatal(err)
			}
		}
	}
	pct := func(v float64) *float64 { return &v }
	add("load_a1_"+suffix, regionA, pct(80))
	add("load_a2_"+suffix, regionA, pct(10))
	add("load_a3_"+suffix, regionA, nil) // no report: unknown load sorts last
	add("load_b1_"+suffix, regionB, pct(90))
	add("load_b2_"+suffix, regionB, pct(20))
	nodes, err := service.relayNodes(ctx, account, now)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		if n.region == regionA || n.region == regionB {
			got = append(got, n.id)
		}
	}
	want := []string{"load_a2_" + suffix, "load_a1_" + suffix, "load_a3_" + suffix, "load_b2_" + suffix, "load_b1_" + suffix}
	if !slices.Equal(got, want) {
		t.Fatalf("relay candidates %v, want %v", got, want)
	}
}
