package environment

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// FIX-ENVLOCK: ENV vault transactions lock their own account, not every account. Other accounts writing full
// record batches as fast as they can must not slow one account's small reads and writes beyond a bound, and that
// account's transactions never fail a serialization check (40001) because of them. The probe runs on its own pool,
// so that pool's transaction counters are the probe's alone.
func TestPostgresVaultOtherAccountsDoNotSlowAnAccount(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	open := func() *db.DB {
		store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	store, probeStore := open(), open()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	run := "envlock_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"environment_vault_operations WHERE account_id", "environment_vault_record_versions WHERE owner_id", "environment_vault_record_heads WHERE owner_id", "environment_vault_scopes WHERE owner_id", "environment_password_vaults WHERE account_id", "users WHERE id"} {
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.`+table+` LIKE $1`, run+"%")
		}
	})
	service := NewService(store, nil, nil, "https://control.example")
	account := func(name string) VaultLayerCoordinate {
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, name); err != nil {
			t.Fatal(err)
		}
		if _, err := service.PutPasswordVault(ctx, name, passwordVaultFixture(t, "https://control.example", name, 1, make([]byte, 32))); err != nil {
			t.Fatal(err)
		}
		anchor := vaultScopeFixture(t, fixtureScopeHeader(name, "personal", name, 1, 1, 1, make([]byte, 32)), 11)
		if _, err := service.PutVaultScope(ctx, name, "personal", "personal", name, "", VaultScopePut{OperationID: "anchor_" + name, Envelope: b64Vault(anchor)}); err != nil {
			t.Fatal(err)
		}
		return VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: name}
	}
	id := func(i int) string { return fmt.Sprintf("%064x", i+1) }

	probeName := run + "_probe"
	probeCoordinate := account(probeName)
	probeService := NewService(probeStore, nil, nil, "https://control.example")
	var probeRevision uint64
	probe := func(d time.Duration) (latencies []time.Duration) {
		for until := time.Now().Add(d); time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
			start := time.Now()
			if _, err := probeService.ListVaultRecords(ctx, probeName, probeCoordinate, VaultRecordsQuery{Limit: 10}); err != nil {
				t.Fatalf("probe read: %v", err)
			}
			op := fmt.Sprintf("probe_%s_%d", run, probeRevision)
			if _, err := probeService.PutVaultRecords(ctx, probeName, probeCoordinate, VaultRecordsPut{OperationID: op, Records: []VaultRecordMutation{{ExpectedRevision: probeRevision, Envelope: vaultRecordFixture(t, probeCoordinate, probeName, id(0), probeRevision+1, 1, false)}}}); err != nil {
				t.Fatalf("probe write: %v", err)
			}
			probeRevision++
			latencies = append(latencies, time.Since(start))
		}
		return latencies
	}
	alone := p99Duration(probe(2 * time.Second))

	// Four other accounts each write full 128-record batches back to back.
	const flooders = 4
	coordinates := make([]VaultLayerCoordinate, flooders)
	for i := range coordinates {
		coordinates[i] = account(fmt.Sprintf("%s_flood%d", run, i))
	}
	before := probeStore.TransactionStats()
	stop := make(chan struct{})
	batches := make([]int, flooders)
	var wg sync.WaitGroup
	for i, c := range coordinates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for revision := uint64(0); ; revision++ {
				select {
				case <-stop:
					return
				default:
				}
				records := make([]VaultRecordMutation, MaximumVaultRecordBatch)
				for r := range records {
					records[r] = VaultRecordMutation{ExpectedRevision: revision, Envelope: vaultRecordFixture(t, c, c.OwnerID, id(r), revision+1, 1, false)}
				}
				if _, err := service.PutVaultRecords(ctx, c.OwnerID, c, VaultRecordsPut{OperationID: fmt.Sprintf("flood_%d_%d", i, revision), Records: records}); err != nil {
					t.Errorf("flooder %d batch %d: %v", i, revision, err)
					return
				}
				batches[i]++
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	loaded := p99Duration(probe(4 * time.Second))
	close(stop)
	wg.Wait()
	after := probeStore.TransactionStats()
	t.Logf("probe p99 alone %v, with %d accounts flooding %v; flood batches %v", alone, flooders, loaded, batches)
	for i, n := range batches {
		if n == 0 {
			t.Fatalf("flooder %d wrote no batch: the flood did not run", i)
		}
	}
	if retries := after.ReadCommitted.Retries - before.ReadCommitted.Retries + after.Serializable.Retries - before.Serializable.Retries; retries != 0 {
		t.Fatalf("the probe account's transactions were retried %d times (40001/40P01) while other accounts wrote", retries)
	}
	if bound := 2*alone + 500*time.Millisecond; loaded > bound {
		t.Fatalf("other accounts' writes slowed the probe account: p99 %v, bound %v (alone %v)", loaded, bound, alone)
	}
}

func p99Duration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(values))
	return sorted[min(len(sorted)-1, len(sorted)*99/100)]
}
