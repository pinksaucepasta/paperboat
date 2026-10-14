package selfhost

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestIngressReconciliationFairnessAndCancellationOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	owner := "ingress_owner_" + randomIdentity()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, owner, owner+"@example.test"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, owner); err != nil {
			t.Errorf("owner cleanup: %v", err)
		}
	})
	key := "ingress-fairness-test-encryption-key"
	service := New(store, key)
	ids := make([]string, 0, 9)
	for range 9 {
		installed := scopedInstallationFixture(t, store, key, owner, "tunnel")
		ids = append(ids, installed.InstallationID)
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET endpoint_host='unreachable-ingress.invalid',state='ready',ready=true,last_heartbeat_at=now() WHERE id=$1`, installed.NodeID); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(ids)
	// These isolated fixtures are all pending: failures in the first batch must
	// not prevent the ninth installation from receiving its own attempt.
	if err := service.ReconcileIngress(ctx); err != nil {
		t.Fatal(err)
	}
	if service.ingressCursor != ids[7] {
		t.Fatalf("first batch cursor=%q want=%q", service.ingressCursor, ids[7])
	}
	if err := service.ReconcileIngress(ctx); err != nil {
		t.Fatal(err)
	}
	if service.ingressCursor != ids[8] {
		t.Fatal("failed first batch starved the ninth node")
	}
	if err := service.ReconcileIngress(ctx); err != nil {
		t.Fatal(err)
	}
	if service.ingressCursor != "" {
		t.Fatal("cursor did not wrap after reaching the end")
	}
	if err := service.ReconcileIngress(ctx); err != nil {
		t.Fatal(err)
	}
	if service.ingressCursor != ids[7] {
		t.Fatal("wrapped reconciliation did not retry initial batch")
	}
	listed, err := service.List(ctx, owner)
	if err != nil || len(listed) != 9 {
		t.Fatalf("inventory count=%d error=%v", len(listed), err)
	}
	for _, node := range listed {
		if node.Ready {
			t.Fatal("failed public-ingress probe marked a node ready")
		}
	}
	// Cancellation while a bounded batch is owned must finish before another
	// run acquires its ownership lock. Repeated runs also exercise cursor sharing.
	cancelledRuns := 0
	for range 3 {
		service.ingressCursor = ""
		probeCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- service.ReconcileIngress(probeCtx) }()
		time.AfterFunc(300*time.Millisecond, stop)
		select {
		case err := <-done:
			stop()
			if errors.Is(err, context.Canceled) && service.ingressCursor != "" {
				cancelledRuns++
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled reconciliation: %v", err)
			}
		case <-time.After(3 * time.Second):
			stop()
			t.Fatal("cancelled reconciliation retained owned probes")
		}
		if !service.ingressMu.TryLock() {
			t.Fatal("returned reconciliation retained ownership")
		}
		service.ingressMu.Unlock()
	}
	if cancelledRuns == 0 {
		t.Fatal("cancellation did not interrupt any owned batch")
	}
}
