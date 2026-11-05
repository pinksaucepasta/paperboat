package servernodes

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

type recordedPublications struct {
	mu   sync.Mutex
	last map[string]tunnelcert.DesiredAddressPublication
}

func (p *recordedPublications) Reconcile(_ context.Context, d tunnelcert.DesiredAddressPublication) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last[d.Hostname] = d
	return nil
}

// A region's name points at its active, fresh nodes only; a registered, draining or stale node is left out,
// and a region whose last node left is published empty so its records are removed.
func TestRegionalDNSPublishesActiveNodeAddressesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	p := fmt.Sprintf("d%d", time.Now().UnixNano()%1_000_000_000)
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE region LIKE $1`, p+"%")
	})
	add := func(id, region, state, host, seen string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, state, version, commit, started_at, last_seen_at, endpoint_host)
VALUES ($1,$2,$3,'v','c',now(),`+seen+`,$4)`, id, region, state, host); err != nil {
			t.Fatal(err)
		}
	}
	add(p+"a-1", p+"a", "active", "203.0.113.10", "now()")
	add(p+"a-2", p+"a", "active", "203.0.113.11", "now()")
	add(p+"a-3", p+"a", "registered", "203.0.113.12", "now()")
	add(p+"a-4", p+"a", "draining", "203.0.113.13", "now()")
	add(p+"a-5", p+"a", "active", "203.0.113.14", "now()-interval '1 minute'")
	add(p+"b-1", p+"b", "draining", "198.51.100.7", "now()")
	// Reached over a private network: DNS publishes the public address it declared, not the dialed one.
	if _, err := store.Pool().Exec(ctx, `UPDATE paperboat.control_server_nodes SET endpoint_host='10.0.0.5', public_address='203.0.113.11' WHERE id=$1`, p+"a-2"); err != nil {
		t.Fatal(err)
	}
	pub := &recordedPublications{last: map[string]tunnelcert.DesiredAddressPublication{}}
	dns := &RegionalDNS{DB: store, Publisher: pub, Domain: "example.test"}
	if err := dns.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := pub.last[p+"a.example.test"], pub.last[p+"b.example.test"]
	if !slices.Equal(a.Addresses, []string{"203.0.113.10", "203.0.113.11"}) || a.Owner != "server_region:"+p+"a" || a.ValidUntil.IsZero() {
		t.Fatalf("region a: %+v", a)
	}
	if b.Hostname == "" || len(b.Addresses) != 0 {
		t.Fatalf("a region with no serving node must be published empty: %+v", b)
	}
}
