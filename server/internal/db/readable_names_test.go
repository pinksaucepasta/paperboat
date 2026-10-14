package db_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestReadableHostnameReservationsOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano())
	user := "names-user-" + suffix
	machine := "names-machine-" + suffix
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, user); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{machine, machine + "-other"} {
		if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state) VALUES($1,$2,$1,$1,'linux','amd64','/workspace','offline','occupied')`, id, user); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(id string, port int, candidate string) string {
		t.Helper()
		var host string
		if err := store.SQL().QueryRowContext(ctx, `SELECT paperboat.reserve_machine_hostname($1,$2,$3)`, id, port, candidate).Scan(&host); err != nil {
			t.Fatal(err)
		}
		return host
	}
	host := "calm-coastal-harbor-0007." + suffix + ".example.test"
	if got := reserve(machine, 3000, host); got != host {
		t.Fatalf("first allocation %q", got)
	}
	if got := reserve(machine, 3000, "bright-autumn-cabin-9999."+suffix+".example.test"); got != host {
		t.Fatalf("replay changed name %q", got)
	}
	if got := reserve(machine+"-other", 3000, host); got != "" {
		t.Fatalf("collision accepted %q", got)
	}
	alternative := "bold-stormy-anchor-0732." + suffix + ".example.test"
	if got := reserve(machine+"-other", 3000, alternative); got != alternative {
		t.Fatalf("collision recovery %q", got)
	}
	for _, kind := range []string{"tunnel", "preview"} {
		_, err = store.SQL().ExecContext(ctx, `SELECT paperboat.claim_managed_hostname($1,$2,$3)`, host, kind, "names-resource-"+suffix)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23505" || pg.ConstraintName != "managed_hostname_reserved" {
			t.Fatalf("cross-producer collision %v", err)
		}
	}
	// Competing transactions must reserve a hostname for exactly one producer.
	raceHost := "happy-winter-cabin-0012." + suffix + ".example.test"
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, kind := range []string{"tunnel", "preview"} {
		wg.Add(1)
		go func(kind string) {
			defer wg.Done()
			<-start
			_, err := store.SQL().ExecContext(ctx, `SELECT paperboat.claim_managed_hostname($1,$2,$3)`, raceHost, kind, "names-race-"+suffix)
			results <- err
		}(kind)
	}
	close(start)
	wg.Wait()
	close(results)
	success, collision := 0, 0
	for err := range results {
		if err == nil {
			success++
			continue
		}
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "managed_hostname_reserved" {
			collision++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || collision != 1 {
		t.Fatalf("concurrent allocation success=%d collision=%d", success, collision)
	}
	if _, err = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.user_machines WHERE id=$1`, machine); err != nil {
		t.Fatal(err)
	}
	var tombstone bool
	if err = store.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.managed_hostnames WHERE hostname=$1 AND machine_id IS NULL)`, host).Scan(&tombstone); err != nil || !tombstone {
		t.Fatalf("machine cleanup retained name without live binding %v %v", tombstone, err)
	}
}
