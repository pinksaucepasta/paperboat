package machineservices

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func TestSnapshotBoundsAndExactPorts(t *testing.T) {
	now := time.Now().UTC()
	good := Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []Port{{5432, "ipv4"}}}
	if e := ValidateSnapshot(good, now); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*Snapshot){"duplicate": func(s *Snapshot) { s.Services = append(s.Services, Port{5432, "ipv6"}) }, "overflow": func(s *Snapshot) { s.Services = make([]Port, 65) }, "wildcard family": func(s *Snapshot) { s.Services = []Port{{5432, "0.0.0.0"}} }, "zero port": func(s *Snapshot) { s.Services = []Port{{0, "ipv4"}} }, "counter overflow": func(s *Snapshot) { s.Generation = 9007199254740992 }, "future boot": func(s *Snapshot) { s.StartedAt = now.Add(6 * time.Second) }, "invalid boot": func(s *Snapshot) { s.BootID = strings.Repeat("x", 65) }} {
		t.Run(name, func(t *testing.T) {
			s := good
			change(&s)
			if ValidateSnapshot(s, now) == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	for _, route := range []string{"tcp:0", "tcp:65536", "tcp:05432", "tcp:+5432", "tcp: 5432", "http:5432"} {
		if _, e := ParseRoute(route); e == nil {
			t.Fatalf("accepted %q", route)
		}
	}
	if n, e := ParseRoute("tcp:5432"); e != nil || n != 5432 {
		t.Fatalf("route %d %v", n, e)
	}
}

func TestBrowserHostnameValidation(t *testing.T) {
	if !validBrowserHostname("calm-coastal-harbor-0732.preview.example.test") {
		t.Fatal("readable host rejected")
	}
	for _, host := range []string{"", "a.b/path", "https://example.test", "bad..test", "-alias.example.test", "alias-.example.test", "UPPER.example.test", strings.Repeat("a", 64) + ".example.test"} {
		if validBrowserHostname(host) {
			t.Fatalf("invalid hostname accepted %q", host)
		}
	}
}

func TestNativeBrowserHostnameUsesCanonicalPortAndAccountScopedAlias(t *testing.T) {
	host, err := NativeBrowserHostname("hp", 3000, "local.example.test")
	if err != nil || host != "3000.hp.local.example.test" {
		t.Fatalf("host=%q err=%v", host, err)
	}
	for _, domain := range []string{"", "https://local.example.test", "UPPER.example.test", "bad..test", "127.0.0.1"} {
		if _, err := NativeBrowserHostname("hp", 3000, domain); err == nil {
			t.Fatalf("accepted domain %q", domain)
		}
	}
	longBase := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 58)
	if _, err := NativeBrowserHostname("hp", 3000, longBase); err == nil {
		t.Fatal("published hostname exceeding DNS total length")
	}
	if host, err := NativeBrowserHostname("hp", 3000, "another.example.test"); err != nil || host != "3000.hp.another.example.test" {
		t.Fatalf("changed domain host=%q err=%v", host, err)
	}
	for _, alias := range []string{"HP", "hp.other", "hp/path", "-hp", ""} {
		if _, err := NativeBrowserHostname(alias, 3000, "local.example.test"); err == nil {
			t.Fatalf("accepted alias %q", alias)
		}
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := NativeBrowserHostname("hp", port, "local.example.test"); err == nil {
			t.Fatalf("accepted port %d", port)
		}
	}
}

func TestMachineServiceRowMissingRequiresExactSQLNoRows(t *testing.T) {
	if !machineServiceRowMissing(sql.ErrNoRows) {
		t.Fatal("exact database/sql no-rows sentinel was not recognized")
	}
	if machineServiceRowMissing(errors.Join(sql.ErrNoRows, syscall.EIO)) {
		t.Fatal("joined database I/O failure was treated as absent machine authority")
	}
	if machineServiceRowMissing(fmt.Errorf("query machine policy: %w", sql.ErrNoRows)) {
		t.Fatal("wrapped storage error was treated as absent machine authority")
	}
}

func TestServiceCleanupFailureRetainsBothCausesWithoutFormattingThem(t *testing.T) {
	operation := ErrDenied
	cleanup := errors.New("private rollback detail")
	failure := withServiceCleanupFailure(operation, cleanup)
	if got := failure.Error(); got != "machine service cleanup failed" || strings.Contains(got, cleanup.Error()) {
		t.Fatalf("cleanup error exposed cause text: %q", got)
	}
	if !errors.Is(failure, operation) || !errors.Is(failure, cleanup) {
		t.Fatalf("cleanup failure lost causes: %T", failure)
	}
	var typed *serviceCleanupFailure
	if !errors.As(failure, &typed) {
		t.Fatalf("cleanup wrapper was not discoverable: %T", failure)
	}
}

func TestServiceRollbackSuppressesOnlyExactErrTxDone(t *testing.T) {
	operation := ErrDenied
	if got := withServiceRollbackFailure(operation, sql.ErrTxDone); got != operation {
		t.Fatalf("exact already-done rollback changed operation: %v", got)
	}
	wrapped := fmt.Errorf("wrapped done: %w", sql.ErrTxDone)
	got := withServiceRollbackFailure(operation, wrapped)
	if !errors.Is(got, operation) || !errors.Is(got, sql.ErrTxDone) {
		t.Fatalf("wrapped rollback failure was suppressed: %T", got)
	}
}

func TestSQLRollbackFailurePreservesCauseAndPoolRecovers(t *testing.T) {
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
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn, err := store.SQL().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var backendPID int
	if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&backendPID); err != nil {
		_ = tx.Rollback()
		_ = conn.Close()
		t.Fatalf("read transaction backend identity: %v", err)
	}
	var terminated bool
	if err := store.SQL().QueryRowContext(ctx, `SELECT pg_terminate_backend($1)`, backendPID).Scan(&terminated); err != nil || !terminated {
		_ = tx.Rollback()
		_ = conn.Close()
		t.Fatalf("terminate owned transaction backend: terminated=%t err=%v", terminated, err)
	}
	rollbackErr := tx.Rollback()
	if rollbackErr == nil || rollbackErr == sql.ErrTxDone {
		_ = conn.Close()
		t.Fatalf("rollback did not expose terminated-backend failure: %v", rollbackErr)
	}
	operationErr := ErrDenied
	combined := withServiceRollbackFailure(operationErr, rollbackErr)
	if !errors.Is(combined, operationErr) || !errors.Is(combined, rollbackErr) {
		_ = conn.Close()
		t.Fatalf("rollback failure lost original cause: %T", combined)
	}
	if err := conn.Close(); err != nil && err != sql.ErrConnDone {
		t.Fatalf("close discarded SQL connection: %v", err)
	}
	var one int
	if err := store.SQL().QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("SQL pool did not recover on a fresh connection: one=%d err=%v", one, err)
	}
}
