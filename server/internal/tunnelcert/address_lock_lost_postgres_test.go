package tunnelcert

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// terminatingAddressProvider kills the connection that holds the publication lock right after its first record is
// created. The write landed, but the reconcile must end there: it no longer holds the lock, so it must not change
// more records.
type terminatingAddressProvider struct {
	fakeAddressProvider
	database *db.DB
}

func (p *terminatingAddressProvider) CreateAddressRecord(ctx context.Context, record AddressRecord, verified bool) (AddressRecord, error) {
	created, err := p.fakeAddressProvider.CreateAddressRecord(ctx, record, verified)
	if _, termErr := p.database.SQL().ExecContext(ctx, `SELECT pg_terminate_backend(l.pid) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid
 WHERE l.locktype='advisory' AND l.granted AND a.datname=current_database() AND l.pid<>pg_backend_pid()`); termErr != nil {
		return AddressRecord{}, termErr
	}
	return created, err
}

func TestAddressReconcileStopsWhenItsLockConnectionDiesOnPostgres(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run address publication PostgreSQL acceptance")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	host, owner := "lockloss-"+suffix+".example.test", "tunnel_"+suffix
	t.Cleanup(func() {
		_, _ = database.Pool().Exec(context.Background(), `DELETE FROM edge_dns_publications WHERE hostname=$1`, host)
	})
	provider := &terminatingAddressProvider{database: database}
	reconciler, err := NewAddressReconciler(database, provider, &fakeAddressResolver{values: []string{"8.8.8.8"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	desired := DesiredAddressPublication{Owner: owner, Hostname: host, ResourceGeneration: 1, ReadinessVersion: "ready_1", ObservedAt: now, ValidUntil: now.Add(time.Minute), Addresses: []string{"8.8.8.8"}}
	if err := reconciler.Reconcile(ctx, desired); err == nil {
		t.Fatal("reconcile reported success after its lock connection died")
	}
	if provider.creates != 1 || provider.deletes != 0 {
		t.Fatalf("reconcile changed records after its lock was lost: creates=%d deletes=%d", provider.creates, provider.deletes)
	}
	var held int
	if err := database.SQL().QueryRowContext(ctx, `SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid=l.pid WHERE l.locktype='advisory' AND l.granted AND a.datname=current_database()`).Scan(&held); err != nil || held != 0 {
		t.Fatalf("publication lock still held after the reconcile ended: %d, err=%v", held, err)
	}
}
