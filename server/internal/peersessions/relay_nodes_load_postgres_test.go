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

// A relay the load rule holds out (a node_load_out row with out_for_load) is absent from the account's relay
// candidates; it is back once the row clears or says it is in. Other relays of the region stay.
func TestRelayCandidatesLeaveOutRelayAboveLoadWithHysteresis(t *testing.T) {
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
	account, region := "load_user_"+suffix, "lr_"+suffix
	add := func(id string) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids,peer_relay_wireguard_public_key,peer_relay_disco_public_key,peer_relay_virtual_address) VALUES($1,'load','v1','epoch',$2,443,444,'ready',true,$3,1,$4,$4||'-a',ARRAY['relay','peer_relay'],ARRAY['derp_quic','peer_relay_udp'],100,1,$3,$5,ARRAY[$6],$7,$8,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')))`, id, id+".example.test", now, region, now.Add(time.Minute), account, make32(), make32()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
		})
	}
	a, b := "load_a_"+suffix, "load_b_"+suffix
	add(a)
	add(b)
	// ids lists the region's candidates; each call uses a later clock so the 5 s candidate cache is not reused.
	step := 0
	ids := func() []string {
		t.Helper()
		step++
		nodes, err := service.relayNodes(ctx, account, now.Add(time.Duration(step)*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, n := range nodes {
			if n.region == region {
				got = append(got, n.id)
			}
		}
		return got
	}
	setOut := func(out bool) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.node_load_out(node_id,out_for_load) VALUES($1,$2) ON CONFLICT (node_id) DO UPDATE SET out_for_load=excluded.out_for_load`, a, out); err != nil {
			t.Fatal(err)
		}
	}
	if got := ids(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v, want both relays", got)
	}
	setOut(true)
	if got := ids(); !slices.Equal(got, []string{b}) {
		t.Fatalf("candidates %v with %s out for load, want only %s", got, a, b)
	}
	setOut(false)
	if got := ids(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v with %s back in, want both", got, a)
	}
	setOut(true)
	if _, err := store.SQL().ExecContext(ctx, `DELETE FROM paperboat.node_load_out WHERE node_id=$1`, a); err != nil {
		t.Fatal(err)
	}
	if got := ids(); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v after the row cleared, want both", got)
	}
}
