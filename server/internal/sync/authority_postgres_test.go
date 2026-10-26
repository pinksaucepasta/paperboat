package sync

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPostgresLeasePersistenceConcurrencyAndTagAuthority(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated Hetzner PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if os.Getenv("PAPERBOAT_TEST_SCHEMA_READY") != "1" {
		if err = db.Migrate(ctx, store); err != nil {
			t.Fatal(err)
		}
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	owner, machine, environment := "sync-owner-"+suffix, "sync-machine-"+suffix, "sync-env-"+suffix
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, owner)
	defer store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, owner)
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, owner)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'sync-test','linux','amd64','/workspace','online','released',true,1)`, machine, owner, environment)
	authority := &SQLAuthority{DB: store}
	var wg sync.WaitGroup
	results := make(chan string, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip, _, err := authority.lease(ctx, owner, machine)
			if err != nil {
				errs <- err
			} else {
				results <- ip
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var expected string
	for ip := range results {
		if expected == "" {
			expected = ip
		}
		if expected != ip || ip == "127.100.0.1" {
			t.Fatalf("colliding or inconsistent lease %s %s", expected, ip)
		}
	}
	// Recreating the adapter cannot reset a durable lease.
	replacement := &SQLAuthority{DB: store}
	ip, _, err := replacement.lease(ctx, owner, machine)
	if err != nil || ip != expected {
		t.Fatalf("restart lease: %s %v", ip, err)
	}
	err = authority.SetTags(ctx, auth.ClientPrincipal{User: auth.User{ID: "foreign"}}, machine, []string{"dev"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign mutation: %v", err)
	}
	err = authority.SetTags(ctx, auth.ClientPrincipal{User: auth.User{ID: owner}}, machine, []string{"dev"})
	if err != nil {
		t.Fatal(err)
	}
	_, tags, err := replacement.lease(ctx, owner, machine)
	if err != nil || len(tags) != 1 || tags[0] != "dev" {
		t.Fatalf("persisted tags: %v %v", tags, err)
	}
	var invalid int
	err = store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.control_sync_leases WHERE address_slot<2 OR address_slot>65534 OR address_slot%256 IN(0,255)`).Scan(&invalid)
	if err != nil || invalid != 0 {
		t.Fatalf("reserved lease: %d %v", invalid, err)
	}
	exec(`DELETE FROM paperboat.user_machines WHERE id=$1`, machine)
	var tombstone bool
	err = store.SQL().QueryRowContext(ctx, `SELECT machine_id IS NULL FROM paperboat.control_sync_leases WHERE address_slot=$1`, func() int {
		var third, fourth int
		fmt.Sscanf(expected, "127.100.%d.%d", &third, &fourth)
		return third*256 + fourth
	}()).Scan(&tombstone)
	if err != nil || !tombstone {
		t.Fatalf("deleted machine lost its permanent address reservation: %t %v", tombstone, err)
	}

}
