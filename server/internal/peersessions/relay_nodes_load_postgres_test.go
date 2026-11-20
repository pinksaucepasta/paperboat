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

// relayLoadFixture is one account with relay nodes added by add; candidates reads the account's relay candidates.
type relayLoadFixture struct {
	t       *testing.T
	store   *db.DB
	service *NetworkService
	now     time.Time
	suffix  string
	account string
	reads   int
}

func newRelayLoadFixture(t *testing.T) *relayLoadFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(context.Background(), store); err != nil {
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
	return &relayLoadFixture{t: t, store: store, service: service, now: now, suffix: suffix, account: "load_user_" + suffix}
}

// add inserts a ready relay in region; cpu is its sustained CPU report (nil: it reported no load).
func (f *relayLoadFixture) add(id, region string, cpu *float64) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids,peer_relay_wireguard_public_key,peer_relay_disco_public_key,peer_relay_virtual_address) VALUES($1,'load','v1','epoch',$2,443,444,'ready',true,$3,1,$4,$4||'-a',ARRAY['relay','peer_relay'],ARRAY['derp_quic','peer_relay_udp'],100,1,$3,$5,ARRAY[$6],$7,$8,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')))`, id, id+".example.test", f.now, region, f.now.Add(time.Minute), f.account, make32(), make32()); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.node_capacity WHERE node_id=$1`, id)
		f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id) // node_load_out cascades
	})
	if cpu != nil {
		if _, err := f.store.SQL().ExecContext(ctx, `INSERT INTO paperboat.node_capacity(node_id,role,process_epoch,cpu_sustained_percent,reported_at) VALUES($1,'relay','epoch',$2,now())`, id, *cpu); err != nil {
			f.t.Fatal(err)
		}
	}
}

// setOut records the load rule's decision for a relay (a node_load_out row); clear removes the row.
func (f *relayLoadFixture) setOut(id string, out bool) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), `INSERT INTO paperboat.node_load_out(node_id,out_for_load) VALUES($1,$2) ON CONFLICT (node_id) DO UPDATE SET out_for_load=excluded.out_for_load`, id, out); err != nil {
		f.t.Fatal(err)
	}
}

func (f *relayLoadFixture) clear(id string) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.node_load_out WHERE node_id=$1`, id); err != nil {
		f.t.Fatal(err)
	}
}

// candidates are the relay ids of the given regions in list order; each call uses a later clock so the 5 s candidate
// cache is not reused.
func (f *relayLoadFixture) candidates(regions ...string) []string {
	f.t.Helper()
	f.reads++
	nodes, err := f.service.relayNodes(context.Background(), f.account, f.now.Add(time.Duration(f.reads)*time.Minute))
	if err != nil {
		f.t.Fatal(err)
	}
	var got []string
	for _, n := range nodes {
		if slices.Contains(regions, n.region) {
			got = append(got, n.id)
		}
	}
	return got
}

func pct(v float64) *float64 { return &v }

// Inside a region the account's relay candidates come least loaded first (the client picks the region by round trip,
// then uses the first relay the server lists in it), a node that reported no load sorts last, and regions keep their
// own group. A relay the load rule holds out is absent while the others keep their least-loaded-first order.
func TestRelayNodesLeastLoadedFirstWithinRegion(t *testing.T) {
	f := newRelayLoadFixture(t)
	regionA, regionB := "lra_"+f.suffix, "lrb_"+f.suffix
	// Ids sort a1, a2, a3 and b1, b2: the load order differs from the id order in both regions.
	f.add("load_a1_"+f.suffix, regionA, pct(80))
	f.add("load_a2_"+f.suffix, regionA, pct(10))
	f.add("load_a3_"+f.suffix, regionA, nil) // no report: unknown load sorts last
	f.add("load_b1_"+f.suffix, regionB, pct(90))
	f.add("load_b2_"+f.suffix, regionB, pct(20))
	id := func(name string) string { return "load_" + name + "_" + f.suffix }
	want := []string{id("a2"), id("a1"), id("a3"), id("b2"), id("b1")}
	if got := f.candidates(regionA, regionB); !slices.Equal(got, want) {
		t.Fatalf("relay candidates %v, want %v", got, want)
	}
	f.setOut(id("a2"), true) // the least loaded relay of region A is held out (a stale decision: the list obeys it)
	want = []string{id("a1"), id("a3"), id("b2"), id("b1")}
	if got := f.candidates(regionA, regionB); !slices.Equal(got, want) {
		t.Fatalf("relay candidates %v with %s held out, want %v", got, id("a2"), want)
	}
}

// A relay the load rule holds out (a node_load_out row with out_for_load) is absent from the account's relay
// candidates; it is back once the row says it is in or is gone. Other relays of the region stay.
func TestRelayCandidatesLeaveOutRelayAboveLoadWithHysteresis(t *testing.T) {
	f := newRelayLoadFixture(t)
	region := "lr_" + f.suffix
	a, b := "load_a_"+f.suffix, "load_b_"+f.suffix
	f.add(a, region, nil)
	f.add(b, region, nil)
	if got := f.candidates(region); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v, want both relays", got)
	}
	f.setOut(a, true)
	if got := f.candidates(region); !slices.Equal(got, []string{b}) {
		t.Fatalf("candidates %v with %s out for load, want only %s", got, a, b)
	}
	f.setOut(a, false)
	if got := f.candidates(region); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v with %s back in, want both", got, a)
	}
	f.setOut(a, true)
	f.clear(a)
	if got := f.candidates(region); !slices.Equal(got, []string{a, b}) {
		t.Fatalf("candidates %v after the row cleared, want both", got)
	}
}
