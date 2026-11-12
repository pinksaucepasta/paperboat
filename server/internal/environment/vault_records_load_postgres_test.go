//go:build load

package environment

// F21-170: ENV under load, no storm and no slow neighbor. Many accounts write full 128-record batches and read their
// scope at once while one unrelated account does small reads and writes; the neighbor's latency under that load
// against its latency alone is the slow-neighbor check, the database's idle-in-transaction and lock-wait samples
// are the storm check, and the bounds (129th record of a batch, 256 KiB live) must still refuse cleanly under it.
//
//	PAPERBOAT_TEST_DATABASE_DSN=... go test -tags load -run TestLoadVaultRecords -v ./internal/environment/
//	(PAPERBOAT_LOAD_ENV_ACCOUNTS, PAPERBOAT_LOAD_ENV_SECONDS: 20 accounts and 20 s by default (8 of them at full batches))

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func loadRecord(t *testing.T, c VaultLayerCoordinate, account, id string, revision uint64, size int) string {
	t.Helper()
	claims := vaultRecordClaims{Domain: "paperboat.environment.record", Version: 1, Issuer: "https://control.example", WorkspaceID: c.WorkspaceID, OwnerKind: c.OwnerKind, OwnerID: c.OwnerID, MachineID: c.MachineID, KeyEpoch: 1, RecordID: id, Revision: revision, WriterAccount: account, WriterVaultGeneration: 1, Nonce: bytes.Repeat([]byte{7}, 12)}
	header, err := EncodeCanonical(claims)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext := bytes.Repeat([]byte{19}, size)
	msg, _ := EncodeCanonical([]any{"paperboat.environment.record-signature", uint64(1), header, ciphertext})
	raw, err := EncodeCanonical(vaultRecordEnvelope{Claims: header, Ciphertext: ciphertext, Signature: ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32)), msg)})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

type loadAccount struct {
	name string
	c    VaultLayerCoordinate
}

func loadVaultAccount(t *testing.T, ctx context.Context, store *db.DB, service *Service, name string) loadAccount {
	t.Helper()
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, name); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PutPasswordVault(ctx, name, passwordVaultFixture(t, "https://control.example", name, 1, make([]byte, 32))); err != nil {
		t.Fatal(err)
	}
	anchor := vaultScopeFixture(t, fixtureScopeHeader(name, "personal", name, 1, 1, 1, make([]byte, 32)), 11)
	if _, err := service.PutVaultScope(ctx, name, "personal", "personal", name, "", VaultScopePut{OperationID: "load_anchor_" + name, Envelope: b64Vault(anchor)}); err != nil {
		t.Fatal(err)
	}
	return loadAccount{name: name, c: VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: name}}
}

func p99(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	slices.Sort(values)
	return values[min(len(values)-1, len(values)*99/100)]
}

func TestLoadVaultRecords(t *testing.T) {
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
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	accountsN, seconds := 20, 20
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_ENV_ACCOUNTS")); err == nil && v > 0 {
		accountsN = v
	}
	if v, err := strconv.Atoi(os.Getenv("PAPERBOAT_LOAD_ENV_SECONDS")); err == nil && v > 0 {
		seconds = v
	}
	run := strconv.FormatInt(time.Now().UnixNano(), 36)
	service := NewService(store, nil, nil, "https://control.example")
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"environment_vault_operations WHERE account_id", "environment_vault_scopes WHERE owner_id", "users WHERE id"} {
			_, _ = store.SQL().ExecContext(c, `DELETE FROM paperboat.`+table+` LIKE $1`, "loadenv_"+run+"%")
		}
	})
	neighbor := loadVaultAccount(t, ctx, store, service, "loadenv_"+run+"_n")
	workers := make([]loadAccount, accountsN+8) // the last 8 write the full batches
	for i := range workers {
		workers[i] = loadVaultAccount(t, ctx, store, service, fmt.Sprintf("loadenv_%s_w%03d", run, i))
	}
	id := func(i int) string { return fmt.Sprintf("%064x", i+1) }

	// The neighbor writes one record and reads its scope every 10 ms; it reports its own latencies.
	var neighborRevision uint64
	probe := func(until time.Time) (reads, writes []time.Duration, failures int) {
		for time.Now().Before(until) {
			start := time.Now()
			if _, err := service.ListVaultRecords(ctx, neighbor.name, neighbor.c, VaultRecordsQuery{Limit: 10}); err != nil {
				failures++
			}
			reads = append(reads, time.Since(start))
			start = time.Now()
			_, err := service.PutVaultRecords(ctx, neighbor.name, neighbor.c, VaultRecordsPut{OperationID: fmt.Sprintf("probe_%s_%d", run, neighborRevision), Records: []VaultRecordMutation{{ExpectedRevision: neighborRevision, Envelope: loadRecord(t, neighbor.c, neighbor.name, id(0), neighborRevision+1, 32)}}})
			if err != nil {
				failures++
			} else {
				neighborRevision++
			}
			writes = append(writes, time.Since(start))
			time.Sleep(10 * time.Millisecond)
		}
		return
	}
	baseReads, baseWrites, baseFailures := probe(time.Now().Add(5 * time.Second))
	if baseFailures != 0 {
		t.Fatalf("the neighbor alone failed %d times", baseFailures)
	}
	t.Logf("neighbor alone: read p99 %v, write p99 %v", p99(baseReads), p99(baseWrites))

	// phase runs `workers` accounts, each writing `batch` records per batch then reading its scope, pausing `pause` between
	// batches, for `seconds`, with the neighbor probing at the same time and the database sampled for idle-in-transaction
	// sessions and lock waits. Every ENV write and read takes the global team write lock (lockVaultScopes), so a phase at
	// full batches measures that queue; "daily" is an account editing a few variables per second.
	phase := func(name string, workers []loadAccount, batch int, pause time.Duration, assertNeighbor bool) {
		var maxIdleInTx, maxLockWait atomic.Int64
		stopSampling := make(chan struct{})
		var sampling sync.WaitGroup
		sampling.Add(1)
		go func() {
			defer sampling.Done()
			for {
				select {
				case <-stopSampling:
					return
				case <-time.After(250 * time.Millisecond):
				}
				var idle, wait int64
				if err := store.SQL().QueryRowContext(context.Background(), `SELECT count(*) FILTER (WHERE state='idle in transaction'), count(*) FILTER (WHERE wait_event_type='Lock') FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()`).Scan(&idle, &wait); err == nil {
					maxIdleInTx.Store(max(maxIdleInTx.Load(), idle))
					maxLockWait.Store(max(maxLockWait.Load(), wait))
				}
			}
		}()
		var batches, reads, errorsTotal atomic.Int64
		var firstError atomic.Value
		deadline := time.Now().Add(time.Duration(seconds) * time.Second)
		var load sync.WaitGroup
		for w := range workers {
			load.Add(1)
			go func() {
				defer load.Done()
				a := workers[w]
				var revision uint64
				for seq := 0; time.Now().Before(deadline); seq++ {
					in := VaultRecordsPut{OperationID: fmt.Sprintf("load_%s_%s_%d_%d", run, name, w, seq)}
					for r := 0; r < batch; r++ {
						in.Records = append(in.Records, VaultRecordMutation{ExpectedRevision: revision, Envelope: loadRecord(t, a.c, a.name, id(r), revision+1, 32)})
					}
					if _, err := service.PutVaultRecords(ctx, a.name, a.c, in); err != nil {
						errorsTotal.Add(1)
						firstError.CompareAndSwap(nil, err.Error())
						continue
					}
					revision++
					batches.Add(1)
					if _, err := service.ListVaultRecords(ctx, a.name, a.c, VaultRecordsQuery{Limit: 200}); err != nil {
						errorsTotal.Add(1)
						firstError.CompareAndSwap(nil, err.Error())
					}
					reads.Add(1)
					time.Sleep(pause)
				}
			}()
		}
		loadReads, loadWrites, loadFailures := probe(deadline)
		load.Wait()
		close(stopSampling)
		sampling.Wait()
		t.Logf("%s: %d accounts, %d-record batches every %v for %d s: %d batches (%.1f/s), %d scope reads, %d worker errors (first: %v)", name, len(workers), batch, pause, seconds, batches.Load(), float64(batches.Load())/float64(seconds), reads.Load(), errorsTotal.Load(), firstError.Load())
		t.Logf("%s: neighbor read p99 %v (%d samples), write p99 %v (%d samples), %d failures; database: max idle-in-transaction %d, max sessions waiting on a lock %d", name, p99(loadReads), len(loadReads), p99(loadWrites), len(loadWrites), loadFailures, maxIdleInTx.Load(), maxLockWait.Load())
		if errorsTotal.Load() != 0 || loadFailures != 0 {
			t.Errorf("%s: failures under load: workers %d (%v), neighbor %d", name, errorsTotal.Load(), firstError.Load(), loadFailures)
		}
		if int(maxIdleInTx.Load()) > len(workers) {
			t.Errorf("%s: idle-in-transaction sessions: %d for %d workers", name, maxIdleInTx.Load(), len(workers))
		}
		if assertNeighbor {
			if got, bound := p99(loadWrites), max(20*p99(baseWrites), time.Second); got > bound {
				t.Errorf("%s: slow neighbor: write p99 %v under load against %v alone (bound %v)", name, got, p99(baseWrites), bound)
			}
			if got, bound := p99(loadReads), max(20*p99(baseReads), time.Second); got > bound {
				t.Errorf("%s: slow neighbor: read p99 %v under load against %v alone (bound %v)", name, got, p99(baseReads), bound)
			}
		}
	}
	phase("daily", workers[:accountsN], 8, time.Second, true)
	phase("full-batches", workers[accountsN:], MaximumVaultRecordBatch, 0, false)

	// The bounds refuse cleanly while the database is busy: the 129th record of a batch, and the byte after 256 KiB live.
	over := workers[0]
	tooMany := VaultRecordsPut{OperationID: "load_over_batch_" + run}
	for r := 0; r <= MaximumVaultRecordBatch; r++ {
		tooMany.Records = append(tooMany.Records, VaultRecordMutation{Envelope: loadRecord(t, over.c, over.name, id(1000+r), 1, 32)})
	}
	if _, err := service.PutVaultRecords(ctx, over.name, over.c, tooMany); !errors.Is(err, ErrProtocolInvalid) {
		t.Errorf("129 records in one batch: %v, want ErrProtocolInvalid", err)
	}
	big := loadVaultAccount(t, ctx, store, service, "loadenv_"+run+"_b")
	live := VaultRecordsPut{OperationID: "load_over_bytes_" + run}
	for r := 0; r < 9; r++ { // nine 30,000-byte records are 270,000 bytes live, over 256 KiB
		live.Records = append(live.Records, VaultRecordMutation{Envelope: loadRecord(t, big.c, big.name, id(2000+r), 1, 30000)})
	}
	if _, err := service.PutVaultRecords(ctx, big.name, big.c, live); !errors.Is(err, ErrLimitExceeded) {
		t.Errorf("more than 256 KiB live: %v, want ErrLimitExceeded", err)
	}
}
