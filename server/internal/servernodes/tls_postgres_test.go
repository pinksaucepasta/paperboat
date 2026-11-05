package servernodes

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// The shared ACME cache round-trips across nodes, never stores the key material in clear, and reports a miss
// the way autocert expects; a node turns active (and enters the region map) only from registered.
func TestRegionalACMECacheAndActivationOnPostgres(t *testing.T) {
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
	p := fmt.Sprintf("t%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_node_acme_cache WHERE key LIKE $1`, p+"%")
		_, _ = store.Pool().Exec(ctx, `DELETE FROM paperboat.control_server_nodes WHERE region=$1`, p)
	})
	key := "0123456789abcdef0123456789abcdef"
	nodeA, nodeB := &ACMECache{DB: store, Key: key}, &ACMECache{DB: store, Key: key}
	secret := []byte("-----BEGIN EC PRIVATE KEY----- test")
	if _, err := nodeA.Get(ctx, p+"name"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("missing entry: %v", err)
	}
	if err := nodeA.Put(ctx, p+"name", secret); err != nil {
		t.Fatal(err)
	}
	if got, err := nodeB.Get(ctx, p+"name"); err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("another node read %q %v", got, err)
	}
	var sealed []byte
	if err := store.Pool().QueryRow(ctx, `SELECT sealed FROM paperboat.control_server_node_acme_cache WHERE key=$1`, p+"name").Scan(&sealed); err != nil || bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatalf("cache stored key material in clear: %v", err)
	}
	if err := nodeA.Delete(ctx, p+"name"); err != nil {
		t.Fatal(err)
	}
	if _, err := nodeB.Get(ctx, p+"name"); !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("deleted entry still read: %v", err)
	}

	for id, state := range map[string]string{p + "-registered": "registered", p + "-draining": "draining"} {
		if _, err := store.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes (id, region, state, version, commit, started_at, last_seen_at)
VALUES ($1,$2,$3,'v','c',now(),now())`, id, p, state); err != nil {
			t.Fatal(err)
		}
	}
	inMap := func() string {
		list, err := Regions(ctx, store, "example.test")
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range list {
			if r.ID == p {
				return r.State
			}
		}
		return ""
	}
	if s := inMap(); s != "draining" {
		t.Fatalf("before activation the region is %q, want draining (only the draining node serves)", s)
	}
	for _, id := range []string{p + "-registered", p + "-draining"} {
		if err := MarkActive(ctx, store, id); err != nil {
			t.Fatal(err)
		}
	}
	var drainingState string
	if err := store.Pool().QueryRow(ctx, `SELECT state FROM paperboat.control_server_nodes WHERE id=$1`, p+"-draining").Scan(&drainingState); err != nil || drainingState != "draining" {
		t.Fatalf("activation revived a draining node: %s %v", drainingState, err)
	}
	if s := inMap(); s != "healthy" {
		t.Fatalf("after activation the region is %q, want healthy", s)
	}
}
