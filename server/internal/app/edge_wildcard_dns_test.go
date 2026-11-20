package app

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

type recordedWildcards struct {
	mu    sync.Mutex
	calls []tunnelcert.DesiredAddressPublication
}

func (p *recordedWildcards) Reconcile(_ context.Context, d tunnelcert.DesiredAddressPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, d)
	return nil
}

func (p *recordedWildcards) reset() { p.calls = nil }

type wildcardFixture struct {
	t        *testing.T
	store    *db.DB
	stamp    string
	public   *recordedWildcards
	runtime  *recordedWildcards
	wildcard *edgeWildcards
	now      time.Time
}

func newWildcardFixture(t *testing.T) *wildcardFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err = db.Migrate(t.Context(), store); err != nil {
		t.Fatal(err)
	}
	f := &wildcardFixture{t: t, store: store, stamp: fmt.Sprint(time.Now().UnixNano()), public: &recordedWildcards{}, runtime: &recordedWildcards{}, now: time.Now()}
	cfg := config.Config{RuntimeBaseDomain: "runtime.example.test", Preview: config.Preview{BaseDomain: "preview.example.test"}}
	cfg.Certificates.ManageEdgeWildcards = true
	f.wildcard = newEdgeWildcards(store, cfg, f.public, f.runtime)
	f.wildcard.now = func() time.Time { return f.now }
	f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'On',true),($2,'Off',false)`, "wc_on_"+f.stamp, "wc_off_"+f.stamp)
	t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.operator_regions WHERE id IN ($1,$2)`, "wc_on_"+f.stamp, "wc_off_"+f.stamp)
	})
	return f
}

func (f *wildcardFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatalf("%v: %s", err, query)
	}
}

// node adds a ready, verified managed edge named n<name>; mutate then changes it with a SQL SET clause.
func (f *wildcardFixture) node(name, v4, v6, region, mutate string) string {
	id := "nwc_" + name + "_" + f.stamp
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,roles,registry_expires_at,region,failure_domain,transports,capacity_limit,capacity_used,capacity_observed_at,public_ingress_ipv4,public_ingress_ipv6,public_ingress_verified_at) VALUES($1,'test','1.0','epoch','ready',true,now(),ARRAY['edge'],now()+interval '1 hour','test','test',ARRAY['http3','http2'],100,0,now(),nullif($2,'')::inet,nullif($3,'')::inet,now())`, id, v4, v6)
	if region != "" {
		f.exec(`INSERT INTO paperboat.managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,decode(md5($1)||md5($1),'hex'),'\x00','{}')`, id, region)
	}
	if mutate != "" {
		f.exec(`UPDATE paperboat.control_tunnel_nodes SET `+mutate+` WHERE id=$1`, id)
	}
	f.t.Cleanup(func() {
		f.exec(`DELETE FROM paperboat.managed_node_authority WHERE node_id=$1`, id)
		f.exec(`DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, id)
	})
	return id
}

func (f *wildcardFixture) sync() {
	f.t.Helper()
	if err := f.wildcard.Sync(context.Background()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *wildcardFixture) want(addresses ...string) {
	f.t.Helper()
	for name, got := range map[string][]tunnelcert.DesiredAddressPublication{"*.preview.example.test": f.public.calls, "*.runtime.example.test": f.runtime.calls} {
		if len(got) != 1 {
			f.t.Fatalf("%s: %d publications, want 1", name, len(got))
		}
		d := got[0]
		if d.Hostname != name || d.Owner != "edge_wildcard:"+name || !slices.Equal(d.Addresses, addresses) || d.ReadinessVersion != "edge-heartbeat" || !d.IPv6ReachabilityVerified {
			f.t.Fatalf("%s published %+v, want addresses %v", name, d, addresses)
		}
		if until := d.ValidUntil.Sub(d.ObservedAt); until != edgeWildcardValidity {
			f.t.Fatalf("%s valid for %v", name, until)
		}
	}
}

func TestEdgeWildcardPublishesReadyNodesOnly(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	f.node("a", "203.0.113.1", "2001:db8::1", on, "")
	f.node("b", "203.0.113.2", "", on, "")
	f.node("registered", "203.0.113.10", "", on, "state='registered',ready=false")
	f.node("stale", "203.0.113.11", "", on, "last_heartbeat_at=now()-interval '1 minute'")
	f.node("unverified", "203.0.113.12", "", on, "public_ingress_verified_at=NULL")
	f.node("disabled", "203.0.113.13", "", "wc_off_"+f.stamp, "")
	f.node("retired", "203.0.113.14", "", on, "")
	f.exec(`UPDATE paperboat.managed_node_authority SET retired_at=now() WHERE node_id=$1`, "nwc_retired_"+f.stamp)
	f.node("selfhosted", "203.0.113.15", "", "", "") // no managed authority row: an owner's own server
	f.node("relay", "203.0.113.16", "", on, "roles=ARRAY['relay']")
	f.sync()
	f.want("203.0.113.1", "2001:db8::1", "203.0.113.2")
}

func TestEdgeWildcardDropsDrainingNode(t *testing.T) {
	f := newWildcardFixture(t)
	on := "wc_on_" + f.stamp
	f.node("a", "203.0.113.1", "", on, "")
	drained := f.node("b", "203.0.113.2", "", on, "")
	f.sync()
	f.want("203.0.113.1", "203.0.113.2")
	f.public.reset()
	f.runtime.reset()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false,drain_deadline=now()+interval '10 minutes' WHERE id=$1`, drained)
	f.sync()
	f.want("203.0.113.1")
}

func TestEdgeWildcardNeverPublishesEmptySet(t *testing.T) {
	f := newWildcardFixture(t)
	id := f.node("a", "203.0.113.1", "", "wc_on_"+f.stamp, "")
	f.sync()
	f.want("203.0.113.1")
	f.public.reset()
	f.runtime.reset()
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='draining',ready=false WHERE id=$1`, id)
	f.sync()
	if len(f.public.calls)+len(f.runtime.calls) != 0 {
		t.Fatalf("an empty set was published: %+v %+v", f.public.calls, f.runtime.calls)
	}
}

func TestEdgeWildcardRefreshesAfterWindow(t *testing.T) {
	f := newWildcardFixture(t)
	f.node("a", "203.0.113.1", "", "wc_on_"+f.stamp, "")
	f.sync()
	f.want("203.0.113.1")
	f.public.reset()
	f.runtime.reset()
	f.now = f.now.Add(edgeWildcardRefresh - time.Second)
	f.sync()
	if len(f.public.calls)+len(f.runtime.calls) != 0 {
		t.Fatal("an unchanged set was published again inside the refresh window")
	}
	f.now = f.now.Add(2 * time.Second)
	f.sync()
	f.want("203.0.113.1")
}
