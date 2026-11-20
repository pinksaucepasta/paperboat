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
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// isolationFixture is one probe account doing small ENV reads and writes on its own pool, so that pool's
// transaction counters are the probe's alone, next to a store the load runs on.
type isolationFixture struct {
	t               *testing.T
	ctx             context.Context
	store           *db.DB
	service         *Service
	run             string
	probeStore      *db.DB
	probeService    *Service
	probeName       string
	probeCoordinate VaultLayerCoordinate
	probeRevision   uint64
}

func newIsolationFixture(t *testing.T) *isolationFixture {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	f := &isolationFixture{t: t, ctx: ctx, store: open(), probeStore: open(), run: "envlock_" + strconv.FormatInt(time.Now().UnixNano(), 36)}
	if err := db.Migrate(context.Background(), f.store); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		for _, table := range []string{"environment_vault_operations WHERE account_id", "environment_vault_record_versions WHERE owner_id", "environment_vault_record_heads WHERE owner_id", "environment_vault_scopes WHERE owner_id", "environment_password_vaults WHERE account_id"} {
			_, _ = f.store.SQL().ExecContext(c, `DELETE FROM paperboat.`+table+` LIKE $1`, f.run+"%")
		}
	})
	f.service = NewService(f.store, nil, nil, "https://control.example")
	f.probeService = NewService(f.probeStore, nil, nil, "https://control.example")
	f.probeName = f.run + "_probe"
	f.probeCoordinate = f.account(f.probeName)
	return f
}

func (f *isolationFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func (f *isolationFixture) user(name string) {
	f.t.Helper()
	f.exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, name)
	f.t.Cleanup(func() {
		_, _ = f.store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, name)
	})
}

// account is a user with a personal vault and its personal ENV scope.
func (f *isolationFixture) account(name string) VaultLayerCoordinate {
	f.t.Helper()
	f.user(name)
	if _, err := f.service.PutPasswordVault(f.ctx, name, passwordVaultFixture(f.t, "https://control.example", name, 1, make([]byte, 32))); err != nil {
		f.t.Fatal(err)
	}
	anchor := vaultScopeFixture(f.t, fixtureScopeHeader(name, "personal", name, 1, 1, 1, make([]byte, 32)), 11)
	if _, err := f.service.PutVaultScope(f.ctx, name, "personal", "personal", name, "", VaultScopePut{OperationID: "anchor_" + name, Envelope: b64Vault(anchor)}); err != nil {
		f.t.Fatal(err)
	}
	return VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: name}
}

func recordID(i int) string { return fmt.Sprintf("%064x", i+1) }

// probe reads and writes one record of the probe account for d and returns each round's latency.
func (f *isolationFixture) probe(d time.Duration) (latencies []time.Duration) {
	for until := time.Now().Add(d); time.Now().Before(until); time.Sleep(5 * time.Millisecond) {
		start := time.Now()
		if _, err := f.probeService.ListVaultRecords(f.ctx, f.probeName, f.probeCoordinate, VaultRecordsQuery{Limit: 10}); err != nil {
			f.t.Fatalf("probe read: %v", err)
		}
		op := fmt.Sprintf("probe_%s_%d", f.run, f.probeRevision)
		if _, err := f.probeService.PutVaultRecords(f.ctx, f.probeName, f.probeCoordinate, VaultRecordsPut{OperationID: op, Records: []VaultRecordMutation{{ExpectedRevision: f.probeRevision, Envelope: vaultRecordFixture(f.t, f.probeCoordinate, f.probeName, recordID(0), f.probeRevision+1, 1, false)}}}); err != nil {
			f.t.Fatalf("probe write: %v", err)
		}
		f.probeRevision++
		latencies = append(latencies, time.Since(start))
	}
	return latencies
}

// underLoad runs load (it returns when stop closes) and fails when it raised the probe account's p99 beyond the
// bound or caused any retry (40001/40P01) of the probe account's transactions.
func (f *isolationFixture) underLoad(what string, load func(stop <-chan struct{})) {
	f.t.Helper()
	alone := p99Duration(f.probe(2 * time.Second))
	before := f.probeStore.TransactionStats()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		load(stop)
	}()
	time.Sleep(300 * time.Millisecond)
	loaded := p99Duration(f.probe(4 * time.Second))
	close(stop)
	wg.Wait()
	after := f.probeStore.TransactionStats()
	f.t.Logf("probe p99 alone %v, with %s %v", alone, what, loaded)
	if retries := after.ReadCommitted.Retries - before.ReadCommitted.Retries + after.Serializable.Retries - before.Serializable.Retries; retries != 0 {
		f.t.Fatalf("the probe account's transactions were retried %d times (40001/40P01) with %s", retries, what)
	}
	if bound := 2*alone + 500*time.Millisecond; loaded > bound {
		f.t.Fatalf("%s slowed the probe account: p99 %v, bound %v (alone %v)", what, loaded, bound, alone)
	}
}

// FIX-ENVLOCK: ENV vault transactions lock their own account, not every account. Other accounts writing full
// record batches as fast as they can must not slow one account's small reads and writes beyond a bound.
func TestPostgresVaultOtherAccountsDoNotSlowAnAccount(t *testing.T) {
	f := newIsolationFixture(t)
	const flooders = 4
	coordinates := make([]VaultLayerCoordinate, flooders)
	for i := range coordinates {
		coordinates[i] = f.account(fmt.Sprintf("%s_flood%d", f.run, i))
	}
	batches := make([]int, flooders)
	f.underLoad(fmt.Sprintf("%d accounts flooding", flooders), func(stop <-chan struct{}) {
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
						records[r] = VaultRecordMutation{ExpectedRevision: revision, Envelope: vaultRecordFixture(t, c, c.OwnerID, recordID(r), revision+1, 1, false)}
					}
					if _, err := f.service.PutVaultRecords(f.ctx, c.OwnerID, c, VaultRecordsPut{OperationID: fmt.Sprintf("flood_%d_%d", i, revision), Records: records}); err != nil {
						t.Errorf("flooder %d batch %d: %v", i, revision, err)
						return
					}
					batches[i]++
				}
			}()
		}
		wg.Wait()
	})
	for i, n := range batches {
		if n == 0 {
			t.Fatalf("flooder %d wrote no batch: the flood did not run", i)
		}
	}
}

// FIX-TEAMLOCK2: membership changes lock only their team. Teams inviting, accepting and removing members as fast as
// they can must not slow an account outside them, or retry its ENV transactions.
func TestPostgresVaultTeamChurnDoesNotSlowAnAccount(t *testing.T) {
	f := newIsolationFixture(t)
	const churners = 3
	type churn struct {
		team, owner, member string
		rounds              int
	}
	work := make([]*churn, churners)
	for i := range work {
		c := &churn{team: fmt.Sprintf("%s-team%d", f.run, i), owner: fmt.Sprintf("%s_owner%d", f.run, i), member: fmt.Sprintf("%s_member%d", f.run, i)}
		f.user(c.owner)
		f.user(c.member)
		f.exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, c.team, c.owner)
		f.exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true)`, c.team, c.owner)
		f.exec(`INSERT INTO paperboat.billing_workspaces(id,team_id,state,product_code,quantity,period_start,period_end) VALUES($1,$2,'active','team-pro',5,now()-interval '1 hour',now()+interval '1 day')`, "bw_"+c.team, c.team)
		work[i] = c
	}
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		tx, err := f.store.SQL().BeginTx(c, nil)
		if err != nil {
			return
		}
		defer tx.Rollback()
		for _, q := range []string{`ALTER TABLE paperboat.audit_events DISABLE TRIGGER audit_events_append_only`, `DELETE FROM paperboat.audit_events WHERE team_id LIKE $1`, `DELETE FROM paperboat.billing_workspaces WHERE team_id LIKE $1`, `DELETE FROM paperboat.teams WHERE team_id LIKE $1`, `ALTER TABLE paperboat.audit_events ENABLE TRIGGER audit_events_append_only`} {
			var args []any
			if len(q) > 0 && q[0] == 'D' {
				args = []any{f.run + "%"}
			}
			if _, err := tx.ExecContext(c, q, args...); err != nil {
				return
			}
		}
		_ = tx.Commit()
	})
	membership := teams.NewService(f.store)
	f.underLoad(fmt.Sprintf("%d teams churning members", churners), func(stop <-chan struct{}) {
		var wg sync.WaitGroup
		for _, c := range work {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					op := fmt.Sprintf("churn_%s_%d", c.team, c.rounds)
					state, err := membership.Get(f.ctx, c.owner, c.team)
					if err == nil {
						var invitation teams.Invitation
						if invitation, err = membership.Invite(f.ctx, c.owner, c.team, teams.InviteRequest{OperationID: op + "_invite", ExpectedGeneration: state.Generation, AccountID: c.member}); err == nil {
							if state, err = membership.Accept(f.ctx, c.member, invitation.InvitationID, teams.AcceptRequest{OperationID: op + "_accept"}); err == nil {
								_, err = membership.Mutate(f.ctx, c.owner, c.team, teams.MutationRequest{OperationID: op + "_remove", ExpectedGeneration: state.Generation, Action: "remove", AccountID: c.member})
							}
						}
					}
					if err != nil {
						t.Errorf("churn %s round %d: %v", c.team, c.rounds, err)
						return
					}
					c.rounds++
				}
			}()
		}
		wg.Wait()
	})
	for _, c := range work {
		if c.rounds == 0 {
			t.Fatalf("team %s churned no member: the churn did not run", c.team)
		}
	}
}

func p99Duration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(values))
	return sorted[min(len(sorted)-1, len(sorted)*99/100)]
}
